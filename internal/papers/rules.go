package papers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode/utf8"
)

const (
	maxPapersLimit = 20
	maxSignals     = 80
	maxDecisions   = 40
	maxQueryTerms  = 80
	maxPatternLen  = 500
)

// Selection is the JSON form of one topic's paper filter. Patterns run against
// lowercased title and abstract text.
type Selection struct {
	MaxPapers      int        `json:"max_papers"`
	MinTier        int        `json:"min_tier"`
	Query          Query      `json:"query"`
	Masks          []Mask     `json:"masks,omitempty"`
	Signals        []Signal   `json:"signals"`
	Decisions      []Decision `json:"decisions"`
	FallbackReason string     `json:"fallback_reason"`
}

type Query struct {
	Categories []string    `json:"categories,omitempty"`
	Terms      []QueryTerm `json:"terms,omitempty"`
}

type QueryTerm struct {
	Field string `json:"field"`
	Text  string `json:"text"`
}

type Mask struct {
	Name   string    `json:"name"`
	Remove string    `json:"remove"`
	When   *MaskWhen `json:"when,omitempty"`
}

type MaskWhen struct {
	Contains string     `json:"contains,omitempty"`
	Pattern  string     `json:"pattern,omitempty"`
	Any      []MaskWhen `json:"any,omitempty"`
	All      []MaskWhen `json:"all,omitempty"`
}

type Signal struct {
	Name       string   `json:"name"`
	Pattern    string   `json:"pattern,omitempty"`
	Fields     []string `json:"fields,omitempty"`
	Categories []string `json:"categories,omitempty"`
	Near       *Near    `json:"near,omitempty"`
	Tier       int      `json:"tier,omitempty"`
	Raw        bool     `json:"raw,omitempty"`
	Unless     []string `json:"unless,omitempty"`
}

type Near struct {
	Term     string `json:"term"`
	Within   int    `json:"within"`
	Contains string `json:"contains"`
}

type Decision struct {
	Tier          int      `json:"tier"`
	Reason        string   `json:"reason"`
	MinSignalTier *int     `json:"min_signal_tier,omitempty"`
	AllSignals    []string `json:"all_signals,omitempty"`
	AnySignals    []string `json:"any_signals,omitempty"`
}

// Rules is a validated selection. The zero value selects nothing.
type Rules struct {
	Query     string
	active    bool
	maxPapers int
	minTier   int
	masks     []compiledMask
	signals   []compiledSignal
	decisions []compiledDecision
	fallback  string
}

type compiledMask struct {
	name   string
	remove string
	when   *compiledWhen
}

type compiledWhen struct {
	contains string
	pattern  *regexp.Regexp
	any      []compiledWhen
	all      []compiledWhen
}

type compiledSignal struct {
	name       string
	pattern    *regexp.Regexp
	fields     []string
	categories []string
	nearTerm   string
	nearWithin int
	nearHas    string
	tier       int
	raw        bool
	unless     []string
	kind       signalKind
}

type signalKind int

const (
	signalPattern signalKind = iota
	signalCategories
	signalNear
)

type compiledDecision struct {
	tier    int
	reason  string
	minTier *int
	all     []string
	any     []string
}

func (r Rules) Active() bool { return r.active }

func (r Rules) MaxPapers() int { return r.maxPapers }

func (r Rules) MinTier() int { return r.minTier }

// OrQuery returns papers that match either arXiv query.
func OrQuery(active, draft string) string {
	if draft == "" || draft == active {
		return active
	}
	if active == "" {
		return draft
	}
	return "(" + active + ") OR (" + draft + ")"
}

// ParseSelection decodes one selection object and rejects unknown fields.
func ParseSelection(data []byte) (Selection, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var spec Selection
	if err := decoder.Decode(&spec); err != nil {
		return Selection{}, errors.New("筛选规则 JSON 格式或字段不合法")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Selection{}, errors.New("筛选规则只能包含一个 JSON 对象")
	}
	return spec, nil
}

// LoadSelection reads a candidate rule file. It does not change the live config.
func LoadSelection(path string) (Selection, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Selection{}, fmt.Errorf("读取筛选规则失败: %w", err)
	}
	return ParseSelection(data)
}

// Compile validates spec and builds the matchers used by Select and Explain.
func Compile(spec Selection) (Rules, error) {
	if spec.MaxPapers < 1 || spec.MaxPapers > maxPapersLimit {
		return Rules{}, fmt.Errorf("max_papers 必须为 1 到 %d", maxPapersLimit)
	}
	if spec.MinTier < 1 || spec.MinTier > 5 {
		return Rules{}, errors.New("min_tier 必须为 1 到 5")
	}
	if strings.TrimSpace(spec.FallbackReason) == "" || strings.ContainsAny(spec.FallbackReason, "\r\n") {
		return Rules{}, errors.New("fallback_reason 不能为空")
	}
	query, err := compileQuery(spec.Query)
	if err != nil {
		return Rules{}, err
	}
	if len(spec.Signals) == 0 || len(spec.Signals) > maxSignals {
		return Rules{}, fmt.Errorf("signals 需要 1 到 %d 条", maxSignals)
	}
	if len(spec.Decisions) == 0 || len(spec.Decisions) > maxDecisions {
		return Rules{}, fmt.Errorf("decisions 需要 1 到 %d 条", maxDecisions)
	}
	names := map[string]Signal{}
	for i, signal := range spec.Signals {
		if err := validateName(signal.Name); err != nil {
			return Rules{}, fmt.Errorf("signals[%d]: %w", i, err)
		}
		if _, exists := names[signal.Name]; exists {
			return Rules{}, fmt.Errorf("signals[%d]（%s）名称重复", i, signal.Name)
		}
		names[signal.Name] = signal
	}
	masks := make([]compiledMask, 0, len(spec.Masks))
	for i, mask := range spec.Masks {
		compiled, err := compileMask(mask)
		if err != nil {
			return Rules{}, fmt.Errorf("masks[%d]: %w", i, err)
		}
		if _, exists := names[mask.Name]; exists {
			return Rules{}, fmt.Errorf("masks[%d]（%s）与信号重名", i, mask.Name)
		}
		names[mask.Name] = Signal{Name: mask.Name}
		masks = append(masks, compiled)
	}
	signals := make([]compiledSignal, 0, len(spec.Signals))
	for i, signal := range spec.Signals {
		compiled, err := compileSignal(signal, names)
		if err != nil {
			return Rules{}, fmt.Errorf("signals[%d]（%s）: %w", i, signal.Name, err)
		}
		signals = append(signals, compiled)
	}
	decisions := make([]compiledDecision, 0, len(spec.Decisions))
	for i, decision := range spec.Decisions {
		compiled, err := compileDecision(decision, names)
		if err != nil {
			return Rules{}, fmt.Errorf("decisions[%d]: %w", i, err)
		}
		decisions = append(decisions, compiled)
	}
	return Rules{
		Query:     query,
		active:    true,
		maxPapers: spec.MaxPapers,
		minTier:   spec.MinTier,
		masks:     masks,
		signals:   signals,
		decisions: decisions,
		fallback:  spec.FallbackReason,
	}, nil
}

func compileQuery(query Query) (string, error) {
	if len(query.Categories)+len(query.Terms) == 0 {
		return "", errors.New("query 至少需要一个分类或检索词")
	}
	if len(query.Terms) > maxQueryTerms || len(query.Categories) > 20 {
		return "", errors.New("query 的分类或检索词过多")
	}
	parts := make([]string, 0, len(query.Categories)+len(query.Terms))
	for i, category := range query.Categories {
		if !categoryPattern.MatchString(category) {
			return "", fmt.Errorf("query.categories[%d] 不是合法的 arXiv 分类", i)
		}
		parts = append(parts, "cat:"+category)
	}
	for i, term := range query.Terms {
		field, ok := queryFields[term.Field]
		if !ok {
			return "", fmt.Errorf("query.terms[%d].field 只能是 title 或 abstract", i)
		}
		text := strings.TrimSpace(term.Text)
		if text == "" || text != term.Text || len(text) > 80 || !queryTextPattern.MatchString(text) || strings.EqualFold(text, "or") || strings.EqualFold(text, "and") {
			return "", fmt.Errorf("query.terms[%d].text 含有不能放进 arXiv 查询的字符", i)
		}
		parts = append(parts, field+":"+quoteTerm(text))
	}
	return "(" + strings.Join(parts, " OR ") + ")", nil
}

func quoteTerm(text string) string {
	if strings.ContainsAny(text, " \t-:") {
		return `"` + text + `"`
	}
	return text
}

var (
	categoryPattern   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*([.-][A-Za-z0-9]+)+$`)
	queryTextPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 .:-]*$`)
	signalNamePattern = regexp.MustCompile(`^[A-Za-z0-9:._+-]{1,80}$`)
	queryFields       = map[string]string{"title": "ti", "abstract": "abs"}
)

func validateName(name string) error {
	if !signalNamePattern.MatchString(name) {
		return errors.New("名称只能包含字母、数字、冒号、点、下划线、加号和连字符")
	}
	return nil
}

func compileMask(mask Mask) (compiledMask, error) {
	if err := validateName(mask.Name); err != nil {
		return compiledMask{}, err
	}
	remove := strings.TrimSpace(mask.Remove)
	if remove == "" || remove != mask.Remove || utf8.RuneCountInString(remove) > 120 {
		return compiledMask{}, errors.New("remove 不能为空，且不能有首尾空白")
	}
	var when *compiledWhen
	if mask.When != nil {
		compiled, err := compileWhen(*mask.When, 0)
		if err != nil {
			return compiledMask{}, err
		}
		when = &compiled
	}
	return compiledMask{name: mask.Name, remove: strings.ToLower(remove), when: when}, nil
}

func compileWhen(when MaskWhen, depth int) (compiledWhen, error) {
	if depth > 4 {
		return compiledWhen{}, errors.New("when 嵌套过深")
	}
	kinds := 0
	if when.Contains != "" {
		kinds++
	}
	if when.Pattern != "" {
		kinds++
	}
	if len(when.Any) > 0 {
		kinds++
	}
	if len(when.All) > 0 {
		kinds++
	}
	if kinds != 1 {
		return compiledWhen{}, errors.New("when 必须且只能设置 contains、pattern、any、all 之一")
	}
	compiled := compiledWhen{contains: strings.ToLower(when.Contains)}
	if when.Pattern != "" {
		pattern, err := compilePattern(when.Pattern)
		if err != nil {
			return compiledWhen{}, err
		}
		compiled.pattern = pattern
	}
	for _, item := range when.Any {
		child, err := compileWhen(item, depth+1)
		if err != nil {
			return compiledWhen{}, err
		}
		compiled.any = append(compiled.any, child)
	}
	for _, item := range when.All {
		child, err := compileWhen(item, depth+1)
		if err != nil {
			return compiledWhen{}, err
		}
		compiled.all = append(compiled.all, child)
	}
	return compiled, nil
}

func compileSignal(signal Signal, names map[string]Signal) (compiledSignal, error) {
	if signal.Tier < 0 || signal.Tier > 5 {
		return compiledSignal{}, errors.New("tier 必须为 0 到 5")
	}
	kinds := 0
	if signal.Pattern != "" {
		kinds++
	}
	if len(signal.Categories) > 0 {
		kinds++
	}
	if signal.Near != nil {
		kinds++
	}
	if kinds != 1 {
		return compiledSignal{}, errors.New("必须且只能设置 pattern、categories、near 之一")
	}
	compiled := compiledSignal{name: signal.Name, tier: signal.Tier, raw: signal.Raw, unless: append([]string(nil), signal.Unless...)}
	for _, name := range signal.Unless {
		other, ok := names[name]
		if !ok {
			return compiledSignal{}, fmt.Errorf("unless 引用了不存在的信号 %s", name)
		}
		if len(other.Unless) > 0 || name == signal.Name {
			return compiledSignal{}, fmt.Errorf("unless 不能引用 %s", name)
		}
	}
	if signal.Pattern != "" {
		if len(signal.Fields) == 0 {
			return compiledSignal{}, errors.New("pattern 需要 fields")
		}
		pattern, err := compilePattern(signal.Pattern)
		if err != nil {
			return compiledSignal{}, err
		}
		compiled.pattern = pattern
		compiled.kind = signalPattern
	}
	if signal.Near != nil {
		if len(signal.Fields) == 0 {
			return compiledSignal{}, errors.New("near 需要 fields")
		}
		if strings.TrimSpace(signal.Near.Term) == "" || strings.TrimSpace(signal.Near.Contains) == "" || signal.Near.Within < 1 || signal.Near.Within > 500 {
			return compiledSignal{}, errors.New("near 需要 term、contains，且 within 为 1 到 500")
		}
		compiled.nearTerm = strings.ToLower(signal.Near.Term)
		compiled.nearHas = strings.ToLower(signal.Near.Contains)
		compiled.nearWithin = signal.Near.Within
		compiled.kind = signalNear
	}
	if len(signal.Categories) > 0 {
		if signal.Raw || len(signal.Fields) > 0 {
			return compiledSignal{}, errors.New("categories 不能同时设置 fields 或 raw")
		}
		for i, category := range signal.Categories {
			if !categoryPattern.MatchString(category) {
				return compiledSignal{}, fmt.Errorf("categories[%d] 不是合法的 arXiv 分类", i)
			}
		}
		compiled.categories = append([]string(nil), signal.Categories...)
		compiled.kind = signalCategories
	}
	if signal.Pattern != "" || signal.Near != nil {
		fields, err := normalizeFields(signal.Fields)
		if err != nil {
			return compiledSignal{}, err
		}
		compiled.fields = fields
	}
	return compiled, nil
}

func normalizeFields(fields []string) ([]string, error) {
	if len(fields) == 0 || len(fields) > 2 {
		return nil, errors.New("fields 只能是 title、abstract 或两者")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if field != "title" && field != "abstract" {
			return nil, errors.New("fields 只能是 title 或 abstract")
		}
		if seen[field] {
			return nil, errors.New("fields 重复")
		}
		seen[field] = true
		out = append(out, field)
	}
	return out, nil
}

func compileDecision(decision Decision, names map[string]Signal) (compiledDecision, error) {
	if decision.Tier < 0 || decision.Tier > 5 {
		return compiledDecision{}, errors.New("tier 必须为 0 到 5")
	}
	if strings.TrimSpace(decision.Reason) == "" || strings.ContainsAny(decision.Reason, "\r\n") {
		return compiledDecision{}, errors.New("reason 不能为空")
	}
	if decision.MinSignalTier == nil && len(decision.AllSignals) == 0 && len(decision.AnySignals) == 0 {
		return compiledDecision{}, errors.New("需要 min_signal_tier、all_signals 或 any_signals")
	}
	if decision.MinSignalTier != nil && (*decision.MinSignalTier < 1 || *decision.MinSignalTier > 5) {
		return compiledDecision{}, errors.New("min_signal_tier 必须为 1 到 5")
	}
	for _, name := range append(append([]string{}, decision.AllSignals...), decision.AnySignals...) {
		if _, ok := names[name]; !ok {
			return compiledDecision{}, fmt.Errorf("引用了不存在的信号 %s", name)
		}
	}
	var minTier *int
	if decision.MinSignalTier != nil {
		value := *decision.MinSignalTier
		minTier = &value
	}
	return compiledDecision{
		tier:    decision.Tier,
		reason:  decision.Reason,
		minTier: minTier,
		all:     append([]string(nil), decision.AllSignals...),
		any:     append([]string(nil), decision.AnySignals...),
	}, nil
}

func compilePattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" || len(pattern) > maxPatternLen {
		return nil, errors.New("正则为空或过长")
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		reason := "无法编译"
		var syntaxErr *syntax.Error
		if errors.As(err, &syntaxErr) {
			reason = syntaxErr.Code.String()
		}
		return nil, fmt.Errorf("正则无效：%s", reason)
	}
	return compiled, nil
}

func (r Rules) explain(paper Paper) Explanation {
	if !r.active {
		return Explanation{Tier: 0, Signals: []string{}, Reason: "no topic signal"}
	}
	rawTitle := strings.ToLower(paper.Title)
	rawAbstract := strings.ToLower(paper.Abstract)
	context := rawTitle + " " + rawAbstract
	title, abstract := rawTitle, rawAbstract
	signals := make([]string, 0)
	matched := map[string]compiledSignal{}
	for _, mask := range r.masks {
		if !mask.fires(context) {
			continue
		}
		signals = append(signals, mask.name)
		matched[mask.name] = compiledSignal{name: mask.name}
		if mask.remove != "" {
			title = strings.ReplaceAll(title, mask.remove, " ")
			abstract = strings.ReplaceAll(abstract, mask.remove, " ")
		}
	}
	for _, signal := range r.signals {
		if len(signal.unless) > 0 {
			continue
		}
		if signal.matches(paper, title, abstract, rawTitle, rawAbstract) {
			signals = append(signals, signal.name)
			matched[signal.name] = signal
		}
	}
	for _, signal := range r.signals {
		if len(signal.unless) == 0 {
			continue
		}
		blocked := false
		for _, name := range signal.unless {
			if _, ok := matched[name]; ok {
				blocked = true
				break
			}
		}
		if blocked || !signal.matches(paper, title, abstract, rawTitle, rawAbstract) {
			continue
		}
		signals = append(signals, signal.name)
		matched[signal.name] = signal
	}
	tier := 0
	reason := r.fallback
	for _, decision := range r.decisions {
		if decision.matches(matched) {
			tier = decision.tier
			reason = decision.reason
			break
		}
	}
	return Explanation{Tier: tier, Signals: signals, Reason: reason}
}

func (m compiledMask) fires(context string) bool {
	if m.when == nil {
		return strings.Contains(context, m.remove)
	}
	return m.when.match(context)
}

func (w compiledWhen) match(context string) bool {
	if w.contains != "" {
		return strings.Contains(context, w.contains)
	}
	if w.pattern != nil {
		return w.pattern.MatchString(context)
	}
	if len(w.any) > 0 {
		for _, child := range w.any {
			if child.match(context) {
				return true
			}
		}
		return false
	}
	for _, child := range w.all {
		if !child.match(context) {
			return false
		}
	}
	return len(w.all) > 0
}

func (s compiledSignal) matches(paper Paper, title, abstract, rawTitle, rawAbstract string) bool {
	switch s.kind {
	case signalCategories:
		for _, want := range s.categories {
			if hasCategory(paper, want) {
				return true
			}
		}
		return false
	case signalPattern:
		return s.pattern.MatchString(s.text(title, abstract, rawTitle, rawAbstract))
	case signalNear:
		return near(s.text(title, abstract, rawTitle, rawAbstract), s.nearTerm, s.nearWithin, s.nearHas)
	default:
		return false
	}
}

func (s compiledSignal) text(title, abstract, rawTitle, rawAbstract string) string {
	if s.raw {
		title, abstract = rawTitle, rawAbstract
	}
	if len(s.fields) == 1 {
		if s.fields[0] == "title" {
			return title
		}
		return abstract
	}
	return title + " " + abstract
}

func near(text, term string, within int, contains string) bool {
	for start := 0; ; {
		rel := strings.Index(text[start:], term)
		if rel < 0 {
			return false
		}
		at := start + rel
		from := at - within
		if from < 0 {
			from = 0
		}
		to := at + len(term) + within
		if to > len(text) {
			to = len(text)
		}
		if strings.Contains(text[from:to], contains) {
			return true
		}
		start = at + len(term)
		if start > len(text) {
			return false
		}
	}
}

func (d compiledDecision) matches(matched map[string]compiledSignal) bool {
	if d.minTier != nil {
		found := false
		for _, signal := range matched {
			if signal.tier >= *d.minTier {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, name := range d.all {
		if _, ok := matched[name]; !ok {
			return false
		}
	}
	if len(d.any) > 0 {
		found := false
		for _, name := range d.any {
			if _, ok := matched[name]; ok {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
