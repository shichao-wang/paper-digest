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
	"github.com/shichao-wang/paper-digest/internal/eval"
	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/internal/state"
)

const evalUsage = "用法: paper-digest eval --date YYYY-MM-DD [--to YYYY-MM-DD] [--rules candidate.json] [--input papers.json] [--json]\n      paper-digest eval rules [--topic id]\n      paper-digest eval label --id <arxiv id> --label relevant|not-relevant|clear [--input papers.json]\n      paper-digest eval fixtures"

var fetchEvalPapers = func(ctx context.Context, since time.Time, searchQuery string) ([]papers.Paper, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	return papers.FetchSince(fetchCtx, &http.Client{Timeout: 45 * time.Second}, "", since, searchQuery)
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
	topicFlag := flags.String("topic", job.Topic, "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *date == "" {
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
		cutoff, err := eval.Cutoff(dates[0])
		if err != nil {
			return err
		}
		since := cutoff.Add(-time.Duration(cfg.Arxiv.LookbackDays) * 24 * time.Hour)
		query := active.Query
		if draft != nil {
			query = papers.OrQuery(active.Query, draft.Query)
		}
		fetched, err = fetchEvalPapers(ctx, since, query)
		if err != nil {
			return err
		}
	}
	store, err := openEvalStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	labels, sent, err := loadEvalContext(ctx, store, topic, dates)
	if err != nil {
		return err
	}
	report, err := eval.Build(topic, dates, cfg.Arxiv.LookbackDays, active, draft, fetched, sent, labels)
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

func loadEvalContext(ctx context.Context, store *state.Store, topic string, dates []string) (map[string]string, map[string][]eval.SentPaper, error) {
	rows, err := store.SelectionLabels(ctx, topic)
	if err != nil {
		return nil, nil, err
	}
	labels := map[string]string{}
	for _, row := range rows {
		labels[row.PaperID] = row.Label
	}
	sent := map[string][]eval.SentPaper{}
	for _, date := range dates {
		picks, err := store.DigestPicks(ctx, topic, date)
		if err != nil {
			return nil, nil, err
		}
		list := make([]eval.SentPaper, 0, len(picks))
		for _, pick := range picks {
			list = append(list, eval.SentPaper{ID: pick.ID, Title: pick.Title})
		}
		sent[date] = list
	}
	return labels, sent, nil
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
