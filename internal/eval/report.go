package eval

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/papers"
)

const maxRangeDays = 14

// LabelRelevant and LabelNotRelevant are the only stored judgments.
const (
	LabelRelevant    = "relevant"
	LabelNotRelevant = "not_relevant"
)

// Cutoff is 09:00 Asia/Shanghai on date, the end of that morning's generation window.
func Cutoff(date string) (time.Time, error) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.Time{}, err
	}
	day, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil || day.Format("2006-01-02") != date {
		return time.Time{}, fmt.Errorf("invalid date %q", date)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), 9, 0, 0, 0, loc), nil
}

// Dates returns each calendar day from start through end, inclusive.
func Dates(start, end string) ([]string, error) {
	if end == "" {
		end = start
	}
	from, err := Cutoff(start)
	if err != nil {
		return nil, err
	}
	to, err := Cutoff(end)
	if err != nil {
		return nil, err
	}
	if to.Before(from) {
		return nil, fmt.Errorf("end date is before start date")
	}
	if to.Sub(from) > time.Duration(maxRangeDays-1)*24*time.Hour {
		return nil, fmt.Errorf("date range is longer than %d days", maxRangeDays)
	}
	var dates []string
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		dates = append(dates, day.Format("2006-01-02"))
	}
	return dates, nil
}

// ScreenTargets is the coarse set a screening call should see: the active
// selection, plus any papers only the draft rules would collect.
func ScreenTargets(fetched []papers.Paper, window papers.Window, active papers.Rules, draft *papers.Rules) []papers.Paper {
	selected := papers.Select(fetched, window, active, nil)
	if draft == nil {
		return selected
	}
	seen := map[string]bool{}
	for _, paper := range selected {
		seen[paper.ID] = true
	}
	for _, paper := range papers.Select(fetched, window, *draft, nil) {
		if seen[paper.ID] {
			continue
		}
		selected = append(selected, paper)
		seen[paper.ID] = true
	}
	return selected
}

type SentPaper struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type Candidate struct {
	ID             string    `json:"id"`
	URL            string    `json:"url"`
	Title          string    `json:"title"`
	Authors        []string  `json:"authors"`
	Abstract       string    `json:"abstract"`
	Published      time.Time `json:"published"`
	Categories     []string  `json:"categories"`
	Tier           int       `json:"tier"`
	Signals        []string  `json:"signals"`
	Reason         string    `json:"reason"`
	Selected       bool      `json:"selected"`
	Position       int       `json:"position"`
	Pushed         bool      `json:"pushed"`
	LooseSelected  bool      `json:"looseSelected"`
	LoosePosition  int       `json:"loosePosition"`
	InDigest       bool      `json:"inDigest"`
	Label          string    `json:"label"`
	DraftSelected  bool      `json:"draftSelected"`
	DraftPosition  int       `json:"draftPosition"`
	DraftPushed    bool      `json:"draftPushed"`
	DraftTier      int       `json:"draftTier"`
	DraftSignals   []string  `json:"draftSignals"`
	DraftReason    string    `json:"draftReason"`
	Screened       bool      `json:"screened"`
	ScreenRelevant bool      `json:"screenRelevant"`
	ScreenScore    float64   `json:"screenScore"`
	ScreenReason   string    `json:"screenReason"`
	ScreenSkill    string    `json:"screenSkill"`
	FinalPushed    bool      `json:"finalPushed"`
	FinalPosition  int       `json:"finalPosition"`
}

type DraftComparison struct {
	Selected   []Candidate `json:"selected"`
	Pushed     []Candidate `json:"pushed"`
	OnlyDraft  []string    `json:"onlyDraft"`
	OnlyActive []string    `json:"onlyActive"`
	Score      Score       `json:"score"`
}

type Score struct {
	Selected    int      `json:"selected"`
	Labeled     int      `json:"labeled"`
	Relevant    int      `json:"relevant"`
	NotRelevant int      `json:"notRelevant"`
	Precision   *float64 `json:"precision"`
}

type Day struct {
	Date             string           `json:"date"`
	Cutoff           time.Time        `json:"cutoff"`
	HasBatch         bool             `json:"hasBatch"`
	Announcement     string           `json:"announcement,omitempty"`
	SubmittedAfter   time.Time        `json:"submittedAfter,omitempty"`
	SubmittedThrough time.Time        `json:"submittedThrough,omitempty"`
	Selected         []Candidate      `json:"selected"`
	Pushed           []Candidate      `json:"pushed"`
	PushLimit        int              `json:"pushLimit"`
	Loose            []Candidate      `json:"loose"`
	Sent             []SentPaper      `json:"sent"`
	Candidates       []Candidate      `json:"candidates"`
	OnlyCurrent      []string         `json:"onlyCurrent"`
	OnlyLoose        []string         `json:"onlyLoose"`
	OnlySent         []string         `json:"onlySent"`
	Score            Score            `json:"score"`
	Draft            *DraftComparison `json:"draft,omitempty"`
	Final            []Candidate      `json:"final"`
	ScreenSource     string           `json:"screenSource,omitempty"`
	ScreenError      string           `json:"screenError,omitempty"`
	JobStatus        string           `json:"jobStatus,omitempty"`
	JobMessage       string           `json:"jobMessage,omitempty"`
}

type Report struct {
	Topic string `json:"topic"`
	Days  []Day  `json:"days"`
}

// ScreenNote is one stored or freshly computed screening verdict.
type ScreenNote struct {
	ID       string
	Relevant bool
	Score    float64
	Reason   string
	Skill    string
}

// DayNote is what the digest already recorded for a date, plus an optional live screen.
type DayNote struct {
	Status    string
	Message   string
	Source    string
	Decisions []ScreenNote
}

// Build previews selection for each date. It does not read or write storage.
// labels maps paper ID to relevant or not_relevant. sent maps a date to the
// papers recorded for that digest, in saved order. notes carries stored or
// live screening verdicts; a nil map leaves screening blank.
func Build(topic string, dates []string, active papers.Rules, draft *papers.Rules, fetched []papers.Paper, sent map[string][]SentPaper, labels map[string]string, notes map[string]DayNote) (Report, error) {
	if len(dates) == 0 {
		return Report{}, fmt.Errorf("date is required")
	}
	if !active.Active() {
		return Report{}, fmt.Errorf("active selection rules are required")
	}
	report := Report{Topic: topic, Days: make([]Day, 0, len(dates))}
	for _, date := range dates {
		cutoff, err := Cutoff(date)
		if err != nil {
			return Report{}, err
		}
		batch, ok, err := papers.DigestBatch(date)
		if err != nil {
			return Report{}, err
		}
		report.Days = append(report.Days, buildDay(date, cutoff, batch, ok, active, draft, fetched, sent[date], labels, notes[date]))
	}
	return report, nil
}

func buildDay(date string, cutoff time.Time, batch papers.Batch, hasBatch bool, active papers.Rules, draft *papers.Rules, fetched []papers.Paper, sent []SentPaper, labels map[string]string, note DayNote) Day {
	window := batch.Window
	if !hasBatch {
		window = papers.Window{}
	}
	selected := papers.Select(fetched, window, active, nil)
	pushed := papers.ForPush(selected, active.MaxPapers())
	loose := papers.SelectLoose(fetched, window, 5, nil)
	var drafted []papers.Paper
	var draftPushed []papers.Paper
	if draft != nil {
		drafted = papers.Select(fetched, window, *draft, nil)
		draftPushed = papers.ForPush(drafted, draft.MaxPapers())
	}
	selectedAt := positions(selected)
	pushedAt := positions(pushed)
	looseAt := positions(loose)
	draftAt := positions(drafted)
	draftPushedAt := positions(draftPushed)
	inDigest := map[string]bool{}
	for _, paper := range sent {
		inDigest[paper.ID] = true
	}
	byID := map[string]Candidate{}
	for _, paper := range windowPapers(fetched, window) {
		explanation := papers.Explain(paper, active)
		var draftExplanation papers.Explanation
		if draft != nil {
			draftExplanation = papers.Explain(paper, *draft)
		}
		item := candidate(paper, explanation, selectedAt, pushedAt, looseAt, inDigest, labels, draft != nil, draftExplanation, draftAt, draftPushedAt)
		byID[paper.ID] = item
	}
	applyScreen(byID, selected, note, active.MaxPapers())
	if sent == nil {
		sent = []SentPaper{}
	}
	day := Day{
		Date:             date,
		Cutoff:           cutoff,
		HasBatch:         hasBatch,
		Announcement:     batch.Announcement,
		SubmittedAfter:   batch.Window.After,
		SubmittedThrough: batch.Window.Through,
		Sent:             sent,
		Selected:         pick(selected, byID),
		Pushed:           pick(pushed, byID),
		PushLimit:        active.MaxPapers(),
		Loose:            pick(loose, byID),
		Candidates:       []Candidate{},
		OnlyCurrent:      []string{},
		OnlyLoose:        []string{},
		OnlySent:         []string{},
	}
	seen := map[string]bool{}
	for _, paper := range selected {
		if item, ok := byID[paper.ID]; ok {
			day.Candidates = append(day.Candidates, item)
			seen[paper.ID] = true
		}
	}
	if draft != nil {
		for _, paper := range drafted {
			if seen[paper.ID] {
				continue
			}
			if item, ok := byID[paper.ID]; ok {
				day.Candidates = append(day.Candidates, item)
				seen[paper.ID] = true
			}
		}
		day.Draft = &DraftComparison{
			Selected:   pick(drafted, byID),
			Pushed:     pick(draftPushed, byID),
			OnlyDraft:  idsIn(pick(drafted, byID), func(item Candidate) bool { return !item.Selected }),
			OnlyActive: idsIn(day.Selected, func(item Candidate) bool { return !item.DraftSelected }),
			Score:      score(pick(draftPushed, byID)),
		}
	}
	for _, paper := range loose {
		if seen[paper.ID] {
			continue
		}
		if item, ok := byID[paper.ID]; ok {
			day.Candidates = append(day.Candidates, item)
			seen[paper.ID] = true
		}
	}
	rest := make([]Candidate, 0)
	for id, item := range byID {
		if !seen[id] {
			rest = append(rest, item)
		}
	}
	sortCandidates(rest)
	day.Candidates = append(day.Candidates, rest...)
	day.OnlyCurrent = idsIn(day.Selected, func(item Candidate) bool { return !item.LooseSelected })
	day.OnlyLoose = idsIn(day.Loose, func(item Candidate) bool { return !item.Selected })
	for _, paper := range sent {
		if _, pushed := pushedAt[paper.ID]; !pushed {
			day.OnlySent = append(day.OnlySent, paper.ID)
		}
	}
	day.Final = finalOf(byID)
	day.ScreenSource = note.Source
	day.ScreenError = screenError(note)
	day.JobStatus = note.Status
	day.JobMessage = note.Message
	if len(note.Decisions) > 0 {
		day.Score = score(day.Final)
	} else {
		day.Score = score(day.Pushed)
	}
	return day
}

func applyScreen(byID map[string]Candidate, selected []papers.Paper, note DayNote, limit int) {
	if len(note.Decisions) == 0 {
		return
	}
	scores := map[string]float64{}
	byDecision := map[string]ScreenNote{}
	for _, decision := range note.Decisions {
		byDecision[decision.ID] = decision
		if decision.Relevant {
			scores[decision.ID] = decision.Score
		}
	}
	finalAt := map[string]int{}
	for i, paper := range papers.ForPush(papers.RankRelevant(selected, scores), limit) {
		finalAt[paper.ID] = i + 1
	}
	for id, item := range byID {
		decision, ok := byDecision[id]
		if !ok {
			continue
		}
		item.Screened = true
		item.ScreenRelevant = decision.Relevant
		item.ScreenScore = decision.Score
		item.ScreenReason = decision.Reason
		item.ScreenSkill = decision.Skill
		item.FinalPosition = finalAt[id]
		item.FinalPushed = finalAt[id] > 0
		byID[id] = item
	}
}

func finalOf(byID map[string]Candidate) []Candidate {
	out := make([]Candidate, 0)
	for _, item := range byID {
		if item.FinalPushed {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FinalPosition < out[j].FinalPosition })
	return out
}

func screenError(note DayNote) string {
	if note.Status == "missed" && strings.TrimSpace(note.Message) != "" {
		return strings.TrimSpace(note.Message)
	}
	return ""
}

func windowPapers(fetched []papers.Paper, window papers.Window) []papers.Paper {
	kept := map[string]papers.Paper{}
	for _, paper := range fetched {
		if paper.ID == "" || !window.Contains(paper.Published) {
			continue
		}
		if current, exists := kept[paper.ID]; !exists || paper.Published.After(current.Published) {
			kept[paper.ID] = paper
		}
	}
	out := make([]papers.Paper, 0, len(kept))
	for _, paper := range kept {
		out = append(out, paper)
	}
	return out
}

func positions(list []papers.Paper) map[string]int {
	out := make(map[string]int, len(list))
	for i, paper := range list {
		out[paper.ID] = i + 1
	}
	return out
}

func candidate(paper papers.Paper, explanation papers.Explanation, selectedAt, pushedAt, looseAt map[string]int, inDigest map[string]bool, labels map[string]string, hasDraft bool, draftExplanation papers.Explanation, draftAt, draftPushedAt map[string]int) Candidate {
	position := selectedAt[paper.ID]
	loosePosition := looseAt[paper.ID]
	categories := paper.Categories
	if categories == nil {
		categories = []string{}
	}
	signals := explanation.Signals
	if signals == nil {
		signals = []string{}
	}
	authors := paper.Authors
	if authors == nil {
		authors = []string{}
	}
	item := Candidate{
		ID:            paper.ID,
		URL:           paper.URL,
		Title:         paper.Title,
		Authors:       authors,
		Abstract:      paper.Abstract,
		Published:     paper.Published,
		Categories:    categories,
		Tier:          explanation.Tier,
		Signals:       signals,
		Reason:        explanation.Reason,
		Selected:      position > 0,
		Position:      position,
		Pushed:        pushedAt[paper.ID] > 0,
		LooseSelected: loosePosition > 0,
		LoosePosition: loosePosition,
		InDigest:      inDigest[paper.ID],
		Label:         labels[paper.ID],
	}
	if hasDraft {
		draftSignals := draftExplanation.Signals
		if draftSignals == nil {
			draftSignals = []string{}
		}
		item.DraftSelected = draftAt[paper.ID] > 0
		item.DraftPosition = draftAt[paper.ID]
		item.DraftPushed = draftPushedAt[paper.ID] > 0
		item.DraftTier = draftExplanation.Tier
		item.DraftSignals = draftSignals
		item.DraftReason = draftExplanation.Reason
	}
	return item
}

func pick(list []papers.Paper, byID map[string]Candidate) []Candidate {
	out := make([]Candidate, 0, len(list))
	for _, paper := range list {
		if item, ok := byID[paper.ID]; ok {
			out = append(out, item)
		}
	}
	return out
}

func idsIn(list []Candidate, keep func(Candidate) bool) []string {
	out := []string{}
	for _, item := range list {
		if keep(item) {
			out = append(out, item.ID)
		}
	}
	return out
}

func sortCandidates(list []Candidate) {
	// Newest first, then ID, matching the loose baseline's tie break.
	for i := 1; i < len(list); i++ {
		item := list[i]
		j := i
		for j > 0 && candidateLess(item, list[j-1]) {
			list[j] = list[j-1]
			j--
		}
		list[j] = item
	}
}

func candidateLess(a, b Candidate) bool {
	if !a.Published.Equal(b.Published) {
		return a.Published.After(b.Published)
	}
	return a.ID < b.ID
}

func score(selected []Candidate) Score {
	result := Score{Selected: len(selected)}
	for _, item := range selected {
		switch item.Label {
		case LabelRelevant:
			result.Labeled++
			result.Relevant++
		case LabelNotRelevant:
			result.Labeled++
			result.NotRelevant++
		}
	}
	if result.Labeled > 0 {
		value := float64(result.Relevant) / float64(result.Labeled)
		result.Precision = &value
	}
	return result
}

// Format renders a report for a terminal. It does not include model output.
func Format(report Report) string {
	var b strings.Builder
	if len(report.Days) == 0 {
		return "没有可预览的日期。\n"
	}
	for i, day := range report.Days {
		if i > 0 {
			b.WriteString("\n")
		}
		if !day.HasBatch {
			fmt.Fprintf(&b, "筛选预览 %s  没有新的 arXiv 公告\n", day.Date)
		} else {
			fmt.Fprintf(&b, "筛选预览 %s  arXiv 公告日 %s（美东）  提交 %s 之后至 %s\n", day.Date, day.Announcement, day.SubmittedAfter.UTC().Format(time.RFC3339), day.SubmittedThrough.UTC().Format(time.RFC3339))
		}
		if day.ScreenSource == "model" {
			b.WriteString("本次调用了模型做精筛，不发送飞书，不写入日报、精筛结论或去重记录。\n")
		} else {
			b.WriteString("不调用模型，不发送飞书，不写入日报或去重记录。\n")
		}
		fmt.Fprintf(&b, "粗筛收集 %d 篇，粗筛上限内 %d 篇（上限 %d）；精筛后将推送 %d 篇；宽松基线 %d 篇；当日已保存 %d 篇。\n", len(day.Selected), len(day.Pushed), day.PushLimit, len(day.Final), len(day.Loose), len(day.Sent))
		if day.ScreenError != "" {
			fmt.Fprintf(&b, "当天未发送：%s\n", day.ScreenError)
		} else if day.JobStatus == "missed" {
			b.WriteString("当天未发送，没有留下原因。\n")
		}
		if day.Draft != nil {
			fmt.Fprintf(&b, "草稿收集 %d 篇，将推送 %d 篇。草稿规则不会写回配置。\n", len(day.Draft.Selected), len(day.Draft.Pushed))
			if day.Draft.Score.Labeled == 0 {
				b.WriteString("草稿已标注将推送 0 篇，精确率暂无。\n")
			} else {
				fmt.Fprintf(&b, "草稿已标注将推送 %d 篇，其中相关 %d、不相关 %d，精确率 %.2f。\n", day.Draft.Score.Labeled, day.Draft.Score.Relevant, day.Draft.Score.NotRelevant, *day.Draft.Score.Precision)
			}
		}
		if day.Score.Labeled == 0 {
			b.WriteString("已标注将推送 0 篇，精确率暂无。\n")
		} else {
			fmt.Fprintf(&b, "已标注将推送 %d 篇，其中相关 %d、不相关 %d，精确率 %.2f。\n", day.Score.Labeled, day.Score.Relevant, day.Score.NotRelevant, *day.Score.Precision)
		}
		b.WriteString("\n收集到的论文\n")
		if len(day.Selected) == 0 {
			b.WriteString("（无）\n")
		}
		for _, item := range day.Selected {
			writeCandidate(&b, item)
		}
		writeIDList(&b, "\n仅当前规则（宽松基线未入选）\n", day.OnlyCurrent)
		writeIDList(&b, "仅宽松基线（当前规则未入选）\n", day.OnlyLoose)
		writeIDList(&b, "仅当日已保存（当前规则未入选）\n", day.OnlySent)
		if day.Draft != nil {
			writeIDList(&b, "仅草稿（当前规则未入选）\n", day.Draft.OnlyDraft)
			writeIDList(&b, "仅当前规则（草稿未入选）\n", day.Draft.OnlyActive)
		}
		b.WriteString("\n全部候选\n")
		for _, item := range day.Candidates {
			mark := "未收集"
			if item.Pushed {
				mark = fmt.Sprintf("将推送 #%d", item.Position)
			} else if item.Selected {
				mark = fmt.Sprintf("收集 #%d，超出推送上限", item.Position)
			}
			fmt.Fprintf(&b, "- [%s][tier %d] %s\n", mark, item.Tier, item.Title)
			fmt.Fprintf(&b, "  %s  %s\n", item.ID, item.URL)
			fmt.Fprintf(&b, "  分类：%s\n", strings.Join(item.Categories, " "))
			fmt.Fprintf(&b, "  信号：%s\n", strings.Join(item.Signals, ", "))
			fmt.Fprintf(&b, "  原因：%s\n", reasonText(item.Reason))
			if item.Screened {
				verdict := "不相关"
				if item.ScreenRelevant {
					verdict = "相关"
				}
				fmt.Fprintf(&b, "  精筛：%s %.2f %s\n", verdict, item.ScreenScore, item.ScreenReason)
			}
			if day.Draft != nil {
				mark := "草稿未收集"
				if item.DraftPushed {
					mark = fmt.Sprintf("草稿将推送 #%d", item.DraftPosition)
				} else if item.DraftSelected {
					mark = fmt.Sprintf("草稿收集 #%d，超出推送上限", item.DraftPosition)
				}
				fmt.Fprintf(&b, "  草稿：[%s][tier %d] %s\n", mark, item.DraftTier, reasonText(item.DraftReason))
			}
			fmt.Fprintf(&b, "  人工：%s\n", labelText(item.Label))
		}
	}
	return b.String()
}

func writeCandidate(b *strings.Builder, item Candidate) {
	push := "超出推送上限"
	if item.Pushed {
		push = "将推送"
	}
	fmt.Fprintf(b, "%d. [%s][tier %d] %s\n", item.Position, push, item.Tier, item.Title)
	fmt.Fprintf(b, "   %s  %s\n", item.ID, item.URL)
	fmt.Fprintf(b, "   分类：%s\n", strings.Join(item.Categories, " "))
	fmt.Fprintf(b, "   信号：%s\n", strings.Join(item.Signals, ", "))
	fmt.Fprintf(b, "   原因：%s  人工：%s\n", reasonText(item.Reason), labelText(item.Label))
	if item.Screened {
		verdict := "不相关"
		if item.ScreenRelevant {
			verdict = "相关"
		}
		final := "不推送"
		if item.FinalPushed {
			final = fmt.Sprintf("精筛后将推送 #%d", item.FinalPosition)
		}
		fmt.Fprintf(b, "   精筛：%s %.2f %s（%s）\n", verdict, item.ScreenScore, item.ScreenReason, final)
	}
}

func writeIDList(b *strings.Builder, heading string, ids []string) {
	b.WriteString(heading)
	if len(ids) == 0 {
		b.WriteString("（无）\n")
		return
	}
	for _, id := range ids {
		fmt.Fprintf(b, "- %s\n", id)
	}
}

func reasonText(reason string) string {
	switch reason {
	case "title match":
		return "标题命中"
	case "cs.IR with a technique signal":
		return "cs.IR 且有技术信号"
	case "abstract phrase only":
		return "仅摘要中的强短语"
	case "private information retrieval is not search":
		return "私有信息检索，不是搜索"
	case "recommendation appears only as an ordinary word":
		return "recommendation 只是普通用词"
	case "cs.IR alone is not a topic match":
		return "仅有 cs.IR，不是主题命中"
	case "search or ads appears only as an ordinary word":
		return "search 或 ads 只是普通用词"
	case "no topic signal":
		return "没有主题信号"
	default:
		return reason
	}
}

func labelText(label string) string {
	switch label {
	case LabelRelevant:
		return "相关"
	case LabelNotRelevant:
		return "不相关"
	default:
		return "未标注"
	}
}
