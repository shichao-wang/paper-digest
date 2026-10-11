package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/digest"
	"github.com/shichao-wang/paper-digest/internal/eval"
	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/internal/state"
	"github.com/shichao-wang/paper-digest/skills"
)

const evalUsage = "用法: paper-digest eval --date YYYY-MM-DD [--to YYYY-MM-DD] [--rules candidate.json] [--input papers.json] [--screen] [--skill SKILL.md] [--json]\n      --date 和 --to 是北京时间日报日期，含首尾，最多 14 天。每一天只预览对应的一个 arXiv 公告日；周日和周一没有新公告。\n      --screen 会调用模型做精筛并产生费用；不加它只显示已经保存的精筛结论。--skill 是草稿精筛技能，必须和 --screen 一起用，不会写回文件。\n      paper-digest eval rules [--topic id]\n      paper-digest eval label --id <arxiv id> --label relevant|not-relevant|clear [--input papers.json]\n      paper-digest eval fixtures"

var fetchEvalPapers = func(ctx context.Context, window papers.Window, searchQuery string) ([]papers.Paper, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	return papers.FetchWindow(fetchCtx, &http.Client{Timeout: 45 * time.Second}, "", window, searchQuery)
}

func runEval(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) > 0 && args[0] == "label" {
		return runEvalLabel(ctx, cfg, args[1:])
	}
	if len(args) > 0 && args[0] == "fixtures" {
		return runEvalFixtures(ctx, cfg, args[1:])
	}
	if len(args) > 0 && args[0] == "rules" {
		return runEvalRules(cfg, args[1:])
	}
	return runEvalPreview(ctx, cfg, args)
}

func runEvalPreview(ctx context.Context, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("eval", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	date := flags.String("date", "", "")
	to := flags.String("to", "", "")
	input := flags.String("input", "", "")
	rulesPath := flags.String("rules", "", "")
	asJSON := flags.Bool("json", false, "")
	screen := flags.Bool("screen", false, "")
	skillPath := flags.String("skill", "", "")
	topicFlag := flags.String("topic", job.Topic, "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *date == "" || (*skillPath != "" && !*screen) {
		return errors.New(evalUsage)
	}
	topic, active, _, err := topicRules(cfg, *topicFlag)
	if err != nil {
		return err
	}
	var draft *papers.Rules
	if *rulesPath != "" {
		spec, err := papers.LoadSelection(*rulesPath)
		if err != nil {
			return err
		}
		compiled, err := papers.Compile(spec)
		if err != nil {
			return err
		}
		draft = &compiled
	}
	dates, err := eval.Dates(*date, *to)
	if err != nil {
		return err
	}
	var fetched []papers.Paper
	if *input != "" {
		fetched, err = readCandidates(*input)
		if err != nil {
			return err
		}
	} else {
		query := active.Query
		if draft != nil {
			query = papers.OrQuery(active.Query, draft.Query)
		}
		fetched, err = papers.FetchEachBatch(ctx, dates, query, fetchEvalPapers)
		if err != nil {
			return err
		}
	}
	store, err := openEvalStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	labels, sent, notes, err := loadEvalContext(ctx, store, topic, dates)
	if err != nil {
		return err
	}
	if *screen {
		notes, err = screenEval(ctx, cfg, topic, dates, active, draft, fetched, notes, *skillPath)
		if err != nil {
			return err
		}
	}
	report, err := eval.Build(topic, dates, active, draft, fetched, sent, labels, notes)
	if err != nil {
		return err
	}
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(report)
	}
	fmt.Print(eval.Format(report))
	return nil
}

func runEvalLabel(ctx context.Context, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("eval label", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	id := flags.String("id", "", "")
	label := flags.String("label", "", "")
	input := flags.String("input", "", "")
	topicFlag := flags.String("topic", job.Topic, "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || strings.TrimSpace(*id) == "" || *label == "" {
		return errors.New(evalUsage)
	}
	topic, _, _, err := topicRules(cfg, *topicFlag)
	if err != nil {
		return err
	}
	normalized := *label
	switch normalized {
	case "relevant":
		normalized = state.LabelRelevant
	case "not-relevant", "not_relevant":
		normalized = state.LabelNotRelevant
	case "clear":
	default:
		return errors.New(evalUsage)
	}
	snapshot := ""
	if *input != "" && normalized != "clear" {
		fetched, err := readCandidates(*input)
		if err != nil {
			return err
		}
		found := false
		for _, paper := range fetched {
			if paper.ID != strings.TrimSpace(*id) {
				continue
			}
			snapshot, err = state.SelectionLabelSnapshot(paper)
			if err != nil {
				return err
			}
			found = true
			break
		}
		if !found {
			return fmt.Errorf("候选文件中没有 %s", strings.TrimSpace(*id))
		}
	}
	store, err := openEvalStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	if normalized == "clear" {
		return store.ClearSelectionLabel(ctx, topic, strings.TrimSpace(*id))
	}
	if err := store.SetSelectionLabel(ctx, topic, strings.TrimSpace(*id), normalized, snapshot); err != nil {
		return err
	}
	fmt.Printf("已保存 %s 的人工判断。\n", strings.TrimSpace(*id))
	return nil
}

func runEvalFixtures(ctx context.Context, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("eval fixtures", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	topicFlag := flags.String("topic", job.Topic, "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New(evalUsage)
	}
	topic, _, _, err := topicRules(cfg, *topicFlag)
	if err != nil {
		return err
	}
	store, err := openEvalStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	rows, err := store.SelectionLabels(ctx, topic)
	if err != nil {
		return err
	}
	stored := make([]eval.StoredLabel, 0, len(rows))
	for _, row := range rows {
		stored = append(stored, eval.StoredLabel{PaperID: row.PaperID, Label: row.Label, Snapshot: row.Snapshot})
	}
	fixtures, skipped := eval.Fixtures(stored)
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "以下标注没有可用的论文快照，未导出：%s\n", strings.Join(skipped, ", "))
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(fixtures)
}

func runEvalRules(cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("eval rules", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	topicFlag := flags.String("topic", job.Topic, "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New(evalUsage)
	}
	_, _, spec, err := topicRules(cfg, *topicFlag)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(spec)
}

func topicRules(cfg config.Config, topic string) (string, papers.Rules, papers.Selection, error) {
	if strings.TrimSpace(topic) == "" {
		topic = job.Topic
	}
	rules, spec, ok := cfg.TopicSelection(topic)
	if !ok {
		return "", papers.Rules{}, papers.Selection{}, fmt.Errorf("主题 %s 没有筛选规则", topic)
	}
	return topic, rules, spec, nil
}

func openEvalStore(cfg config.Config) (*state.Store, error) {
	path, err := cfg.DatabasePath()
	if err != nil {
		return nil, err
	}
	return state.Open(path)
}

func loadEvalContext(ctx context.Context, store *state.Store, topic string, dates []string) (map[string]string, map[string][]eval.SentPaper, map[string]eval.DayNote, error) {
	rows, err := store.SelectionLabels(ctx, topic)
	if err != nil {
		return nil, nil, nil, err
	}
	labels := map[string]string{}
	for _, row := range rows {
		labels[row.PaperID] = row.Label
	}
	sent := map[string][]eval.SentPaper{}
	notes := map[string]eval.DayNote{}
	for _, date := range dates {
		picks, err := store.DigestPicks(ctx, topic, date)
		if err != nil {
			return nil, nil, nil, err
		}
		list := make([]eval.SentPaper, 0, len(picks))
		for _, pick := range picks {
			list = append(list, eval.SentPaper{ID: pick.ID, Title: pick.Title})
		}
		sent[date] = list
		current, decisions, err := store.DailyRecord(ctx, topic, date)
		if err != nil {
			return nil, nil, nil, err
		}
		note := eval.DayNote{Status: current.Status, Message: current.Message}
		if len(decisions) > 0 {
			note.Source = "stored"
			note.Decisions = make([]eval.ScreenNote, len(decisions))
			for i, decision := range decisions {
				note.Decisions[i] = eval.ScreenNote{ID: decision.PaperID, Relevant: decision.Relevant, Score: decision.Score, Reason: decision.Reason, Skill: decision.Skill}
			}
		}
		notes[date] = note
	}
	return labels, sent, notes, nil
}

func screenEval(ctx context.Context, cfg config.Config, topic string, dates []string, active papers.Rules, draft *papers.Rules, fetched []papers.Paper, notes map[string]eval.DayNote, skillPath string) (map[string]eval.DayNote, error) {
	if strings.TrimSpace(cfg.Anthropic.APIKey) == "" {
		return nil, errors.New("精筛需要 anthropic.api_key")
	}
	set, err := skills.Load(cfg.Skills.Dir)
	if err != nil {
		return nil, err
	}
	if err := cfg.ResolveSkills(set); err != nil {
		return nil, err
	}
	name, _ := cfg.SkillNames(topic)
	skill, err := set.Get(name)
	if err != nil {
		return nil, err
	}
	if skillPath != "" {
		data, err := os.ReadFile(skillPath)
		if err != nil {
			return nil, err
		}
		skill, err = skills.Parse(string(data))
		if err != nil {
			return nil, err
		}
	}
	model := strings.TrimSpace(cfg.Anthropic.Model)
	if model == "" {
		model = digest.DefaultModel
	}
	call := cfg.ScreeningCall()
	screener := digest.Screener{
		Model: model, APIKey: cfg.Anthropic.APIKey, BaseURL: cfg.Anthropic.BaseURL,
		Instruction: skill.Body, Skill: skill.Version(),
		MaxTokens: call.MaxTokens, Timeout: call.Timeout,
		BatchSize: call.BatchSize, Concurrency: call.Concurrency,
	}
	if notes == nil {
		notes = map[string]eval.DayNote{}
	}
	for _, date := range dates {
		batch, ok, err := papers.DigestBatch(date)
		if err != nil {
			return nil, err
		}
		window := batch.Window
		if !ok {
			window = papers.Window{}
		}
		selected := eval.ScreenTargets(fetched, window, active, draft)
		note := notes[date]
		note.Source = "model"
		note.Decisions = nil
		if len(selected) > 0 {
			decisions, err := screener.Screen(ctx, selected)
			if err != nil {
				return nil, fmt.Errorf("%s 精筛失败，没有写入结果: %w", date, err)
			}
			note.Decisions = make([]eval.ScreenNote, len(decisions))
			for i, decision := range decisions {
				note.Decisions[i] = eval.ScreenNote{ID: decision.ID, Relevant: decision.Relevant, Score: decision.Score, Reason: decision.Reason, Skill: decision.Skill}
			}
		}
		notes[date] = note
	}
	return notes, nil
}

func readCandidates(path string) ([]papers.Paper, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeCandidates(data)
}

func decodeCandidates(data []byte) ([]papers.Paper, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var rows []candidateRow
	if err := decoder.Decode(&rows); err != nil {
		return nil, fmt.Errorf("读取候选文件: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("候选文件只能包含一个 JSON 数组")
	}
	out := make([]papers.Paper, 0, len(rows))
	for _, row := range rows {
		paper := row.paper()
		if paper.ID == "" {
			return nil, errors.New("候选论文缺少 ID")
		}
		out = append(out, paper)
	}
	return out, nil
}

type candidateRow struct {
	ID           string    `json:"ID"`
	AltID        string    `json:"id"`
	Title        string    `json:"Title"`
	AltTitle     string    `json:"title"`
	Abstract     string    `json:"Abstract"`
	AltAbstract  string    `json:"abstract"`
	Authors      []string  `json:"Authors"`
	AltAuthors   []string  `json:"authors"`
	Published    time.Time `json:"Published"`
	AltPublished time.Time `json:"published"`
	URL          string    `json:"URL"`
	AltURL       string    `json:"url"`
	Categories   []string  `json:"categories"`
	Version      string    `json:"Version"`
	AltVersion   string    `json:"version"`
}

func (row candidateRow) paper() papers.Paper {
	paper := papers.Paper{
		ID:         firstText(row.ID, row.AltID),
		Version:    firstText(row.Version, row.AltVersion),
		Title:      firstText(row.Title, row.AltTitle),
		Abstract:   firstText(row.Abstract, row.AltAbstract),
		Authors:    row.Authors,
		Published:  row.Published,
		URL:        firstText(row.URL, row.AltURL),
		Categories: row.Categories,
	}
	if paper.Authors == nil {
		paper.Authors = row.AltAuthors
	}
	if paper.Published.IsZero() {
		paper.Published = row.AltPublished
	}
	return paper
}

func firstText(primary, alternate string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	return alternate
}
