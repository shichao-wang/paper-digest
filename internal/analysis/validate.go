package analysis

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/shichao-wang/paper-digest/internal/library"
)

func digest(s string) string                              { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func blockKey(d library.Document, b library.Block) string { return d.ID + "/" + b.ID }
func validateDocument(v library.Version, d library.Document) error {
	if v.Identity.Validate() != nil || d.Identity != v.Identity || d.ID == "" || d.Text.SHA256 == "" || d.Source.SHA256 == "" || len(d.Blocks) == 0 {
		return fmt.Errorf("%w: document identity or artifact missing", ErrValidation)
	}
	if d.Quality != "ready" {
		return library.ErrQuality
	}
	pages := map[int]library.Page{}
	ends := map[int]int{}
	ids := map[string]bool{}
	for _, p := range d.Pages {
		if p.Number < 1 || pages[p.Number].Number != 0 || !utf8.ValidString(p.Text) || p.SHA256 != digest(p.Text) {
			return fmt.Errorf("%w: page hash", ErrValidation)
		}
		pages[p.Number] = p
	}
	for _, b := range d.Blocks {
		p, ok := pages[b.Page]
		if !ok || b.ID == "" || ids[b.ID] || b.Start != ends[b.Page] || b.End <= b.Start || b.End > len(p.Text) || !utf8.ValidString(p.Text[b.Start:b.End]) || b.Text != p.Text[b.Start:b.End] || b.SHA256 != digest(b.Text) {
			return fmt.Errorf("%w: block range or hash", ErrValidation)
		}
		ids[b.ID] = true
		ends[b.Page] = b.End
	}
	for n, p := range pages {
		if ends[n] != len(p.Text) {
			return fmt.Errorf("%w: incomplete document blocks", ErrValidation)
		}
	}
	return nil
}

type grounding struct {
	docs    map[string]library.Document
	read    map[string]bool
	allowed map[string]library.Evidence // non-nil for synthesis, prevents invented excerpts
}

func (g grounding) evidence(items []library.Evidence) (map[string]library.Evidence, error) {
	byID := map[string]library.Evidence{}
	for _, e := range items {
		if e.ID == "" || byID[e.ID].ID != "" || e.Quote == "" || e.Section == "" {
			return nil, fmt.Errorf("%w: invalid or duplicate evidence", ErrValidation)
		}
		d, ok := g.docs[e.DocumentID]
		if !ok || e.Version != d.Version {
			return nil, fmt.Errorf("%w: evidence version binding", ErrValidation)
		}
		var block library.Block
		for _, b := range d.Blocks {
			if b.ID == e.BlockID {
				block = b
				break
			}
		}
		if block.ID == "" || !g.read[blockKey(d, block)] || e.Page != block.Page || e.Start < block.Start || e.End > block.End || e.End <= e.Start {
			return nil, fmt.Errorf("%w: evidence outside read block", ErrValidation)
		}
		var page library.Page
		for _, p := range d.Pages {
			if p.Number == e.Page {
				page = p
				break
			}
		}
		if e.End > len(page.Text) || !utf8.ValidString(page.Text[e.Start:e.End]) || page.Text[e.Start:e.End] != e.Quote || digest(block.Text) != block.SHA256 {
			return nil, fmt.Errorf("%w: quote or read hash mismatch", ErrValidation)
		}
		if g.allowed != nil {
			original, ok := g.allowed[e.ID]
			if !ok || original != e {
				return nil, fmt.Errorf("%w: evidence must be retained from chunk notes", ErrValidation)
			}
		}
		byID[e.ID] = e
	}
	return byID, nil
}
func validateRefs(ids []string, ev map[string]library.Evidence, required bool) error {
	if required && len(ids) == 0 {
		return fmt.Errorf("%w: claim requires evidence", ErrValidation)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if _, ok := ev[id]; !ok || seen[id] {
			return fmt.Errorf("%w: unknown or duplicate evidence reference", ErrValidation)
		}
		seen[id] = true
	}
	return nil
}
func claims(value any, ev map[string]library.Evidence) error {
	return walkClaims(reflect.ValueOf(value), ev)
}
func walkClaims(v reflect.Value, ev map[string]library.Evidence) error {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		return walkClaims(v.Elem(), ev)
	}
	if v.Type() == reflect.TypeOf(library.Claim{}) {
		c := v.Interface().(library.Claim)
		if strings.TrimSpace(c.Text) == "" {
			return fmt.Errorf("%w: empty claim", ErrValidation)
		}
		return validateRefs(c.EvidenceIDs, ev, true)
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if err := walkClaims(v.Field(i), ev); err != nil {
				return err
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			if err := walkClaims(v.Index(i), ev); err != nil {
				return err
			}
		}
	}
	return nil
}
func (g grounding) chunk(c chunkContent) error {
	ev, err := g.evidence(c.Evidence)
	if err != nil {
		return err
	}
	if err = claims(c.Notes, ev); err != nil {
		return err
	}
	return missing(c.MissingFields)
}
func missing(fields []string) error {
	seen := map[string]bool{}
	for _, s := range fields {
		switch s {
		case "table_context", "formula_context", "critical_table_context", "critical_formula_context":
			return library.ErrQuality
		}
		if strings.TrimSpace(s) == "" || seen[s] {
			return fmt.Errorf("%w: invalid missing_fields", ErrValidation)
		}
		seen[s] = true
	}
	return nil
}
func normalize(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

var number = regexp.MustCompile(`[-+]?\d+(?:[.,]\d+)*(?:[eE][-+]?\d+)?`)
var contextBoundary = regexp.MustCompile(`(?:[.!?](?:[ \t]+|\n)|\n[ \t]*\n)`)

func containsValue(text, value string) bool {
	text = normalize(text)
	value = normalize(value)
	if value == "" {
		return false
	}
	// 数值字面量及 token 边界防止用 80 或 18 为 8 提供证据。
	for offset := 0; offset <= len(text); {
		at := strings.Index(text[offset:], value)
		if at < 0 {
			return false
		}
		at += offset
		end := at + len(value)
		boundary := func(b byte) bool { return b >= '0' && b <= '9' || b == '.' || b == ',' }
		if (at == 0 || !boundary(text[at-1])) && (end == len(text) || !boundary(text[end])) {
			return true
		}
		offset = at + 1
	}
	return false
}
func metricValueContext(text, metric, value string) bool {
	text, metric, value = normalize(text), normalize(metric), normalize(value)
	if !containsValue(text, value) {
		return false
	}
	// 同句多个指标不能借用彼此数值，如 NDCG 0.9 and Precision 0.8 不支持 NDCG=0.8。
	numbers := number.FindAllStringIndex(text, -1)
	for mo := 0; mo < len(text); {
		mi := strings.Index(text[mo:], metric)
		if mi < 0 {
			break
		}
		mi += mo
		for vo := 0; vo < len(text); {
			vi := strings.Index(text[vo:], value)
			if vi < 0 {
				break
			}
			vi += vo
			lo, hi := mi+len(metric), vi
			if vi < mi {
				lo, hi = vi+len(value), mi
			}
			if hi < lo {
				vo = vi + 1
				continue
			}
			between := text[lo:hi]
			valid := !strings.Contains(between, ";") && !strings.Contains(between, "|")
			for _, n := range numbers {
				if n[0] >= lo && n[1] <= hi {
					valid = false
				}
			}
			// 在完整句子内重新核查数值边界。
			boundary := func(b byte) bool { return b >= '0' && b <= '9' || b == '.' || b == ',' }
			end := vi + len(value)
			if valid && (vi == 0 || !boundary(text[vi-1])) && (end == len(text) || !boundary(text[end])) {
				return true
			}
			vo = vi + 1
		}
		mo = mi + 1
	}
	return false
}
func numeric(r library.NumericResult, ev map[string]library.Evidence) error {
	if strings.TrimSpace(r.Metric) == "" || strings.TrimSpace(r.Value) == "" || !number.MatchString(r.Value) {
		return fmt.Errorf("%w: numeric result requires metric and numeric value", ErrValidation)
	}
	if err := validateRefs(r.EvidenceIDs, ev, true); err != nil {
		return err
	}
	// 指标、数值和所有非空上下文必须同在一条证据中，不能从全文借数字。
	for _, id := range r.EvidenceIDs {
		// 不允许一段引文跨句借数字；表格行和标题保留在同段，小数点不切句。
		for _, context := range contextBoundary.Split(ev[id].Quote, -1) {
			q := normalize(context)
			if !metricValueContext(q, r.Metric, r.Value) {
				continue
			}
			ok := true
			for _, s := range []*string{r.Dataset, r.Method, r.Baseline, r.Unit, r.Setting} {
				if s != nil && (normalize(*s) == "" || !strings.Contains(q, normalize(*s))) {
					ok = false
				}
			}
			if ok {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: numeric result lacks matching metric/value/context in cited excerpt", ErrValidation)
}
func (g grounding) analysis(c library.AnalysisContent) error {
	ev, err := g.evidence(c.Evidence)
	if err != nil {
		return err
	}
	if strings.TrimSpace(c.TitleZH) == "" || c.Relevance.Validate() != nil {
		return fmt.Errorf("%w: title or relevance", ErrValidation)
	}
	if err = validateRefs(c.Relevance.EvidenceIDs, ev, true); err != nil {
		return err
	}
	if err = claims(c, ev); err != nil {
		return err
	}
	for _, r := range c.Results {
		if err = numeric(r, ev); err != nil {
			return err
		}
	}
	for _, list := range [][]library.Application{c.AuthorClaims, c.AgentInferences} {
		for _, a := range list {
			if a.Scenario == "" {
				return ErrValidation
			}
			if err = validateRefs(a.EvidenceIDs, ev, true); err != nil {
				return err
			}
		}
	}
	if err = missing(c.MissingFields); err != nil {
		return err
	}
	absent := map[string]bool{}
	for _, f := range c.MissingFields {
		absent[f] = true
	}
	for name, claim := range map[string]*library.Claim{"problem": c.Problem, "motivation": c.Motivation, "method": c.Method} {
		if claim == nil && !absent[name] {
			return fmt.Errorf("%w: absent field must be recorded in missing_fields", ErrValidation)
		}
	}
	return nil
}
func (g grounding) comparison(c library.ComparisonContent, current, previous library.Document) error {
	ev, err := g.evidence(c.Evidence)
	if err != nil {
		return err
	}
	for _, change := range c.Changes {
		switch change.Kind {
		case "added", "removed", "changed", "unchanged":
		default:
			return fmt.Errorf("%w: change kind", ErrValidation)
		}
		if strings.TrimSpace(change.Description) == "" {
			return ErrValidation
		}
		if err = validateRefs(change.CurrentEvidenceIDs, ev, change.Kind != "removed"); err != nil {
			return err
		}
		if err = validateRefs(change.PreviousEvidenceIDs, ev, change.Kind != "added"); err != nil {
			return err
		}
		for _, id := range change.CurrentEvidenceIDs {
			if ev[id].DocumentID != current.ID {
				return fmt.Errorf("%w: change current version binding", ErrValidation)
			}
		}
		for _, id := range change.PreviousEvidenceIDs {
			if ev[id].DocumentID != previous.ID {
				return fmt.Errorf("%w: change previous version binding", ErrValidation)
			}
		}
	}
	return nil
}
func cloneCheckpoint(c Checkpoint) Checkpoint {
	b, _ := json.Marshal(c)
	var out Checkpoint
	_ = json.Unmarshal(b, &out)
	return out
}
