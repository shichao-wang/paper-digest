package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/internal/state"
)

func TestEvalPreviewLabelsAndFixturesDoNotTouchDigestState(t *testing.T) {
	originalFetch := fetchEvalPapers
	t.Cleanup(func() { fetchEvalPapers = originalFetch })
	fetchEvalPapers = func(context.Context, time.Time, string) ([]papers.Paper, error) {
		t.Fatal("offline preview fetched arXiv")
		return nil, nil
	}

	database := filepath.Join(t.TempDir(), "digest.db")
	store, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimDay(context.Background(), job.Topic, "2026-10-01"); err != nil {
		t.Fatal(err)
	}
	store.Close()

	input := filepath.Join(t.TempDir(), "papers.json")
	body := `[
	  {"ID":"arxiv:strong","Title":"Sequential Recommendation","Abstract":"session logs","Published":"2026-10-07T17:38:35Z","URL":"https://arxiv.org/abs/strong","categories":["cs.IR"]},
	  {"id":"arxiv:weak","title":"Cross community","abstract":"social","published":"2026-10-07T18:00:00Z","categories":["cs.IR"]}
	]`
	if err := os.WriteFile(input, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	args := configArgs(t, database, "", "", false, "eval", "--date", "2026-10-09", "--input", input)
	text, err := captureStdout(t, func() error { return run(args) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "不调用模型，不发送飞书，不写入日报或去重记录。") || !strings.Contains(text, "Sequential Recommendation") || !strings.Contains(text, "仅宽松基线") || strings.Contains(text, "Cross community\n1.") {
		t.Fatalf("preview text = %s", text)
	}
	if !strings.Contains(text, "arxiv:weak") {
		t.Fatalf("weak paper missing: %s", text)
	}

	jsonArgs := append(append([]string{}, args...), "--json")
	raw, err := captureStdout(t, func() error { return run(jsonArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Days []struct {
			Selected []struct {
				ID string `json:"id"`
			} `json:"selected"`
			OnlyLoose []string `json:"onlyLoose"`
			Score     struct {
				Precision *float64 `json:"precision"`
			} `json:"score"`
		} `json:"days"`
	}
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Days) != 1 || len(report.Days[0].Selected) != 1 || report.Days[0].Selected[0].ID != "arxiv:strong" || report.Days[0].Score.Precision != nil || len(report.Days[0].OnlyLoose) != 1 || report.Days[0].OnlyLoose[0] != "arxiv:weak" {
		t.Fatalf("report = %s", raw)
	}

	labelArgs := configArgs(t, database, "", "", false, "eval", "label", "--id", "arxiv:strong", "--label", "not-relevant", "--input", input)
	if _, err := captureStdout(t, func() error { return run(labelArgs) }); err != nil {
		t.Fatal(err)
	}
	raw, err = captureStdout(t, func() error { return run(jsonArgs) })
	if err != nil {
		t.Fatal(err)
	}
	report = struct {
		Days []struct {
			Selected []struct {
				ID string `json:"id"`
			} `json:"selected"`
			OnlyLoose []string `json:"onlyLoose"`
			Score     struct {
				Precision *float64 `json:"precision"`
			} `json:"score"`
		} `json:"days"`
	}{}
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatal(err)
	}
	if report.Days[0].Score.Precision == nil || *report.Days[0].Score.Precision != 0 {
		t.Fatalf("precision = %s", raw)
	}

	fixtures, err := captureStdout(t, func() error {
		return run(configArgs(t, database, "", "", false, "eval", "fixtures"))
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fixtures, `"role": "negative"`) || !strings.Contains(fixtures, `"id": "arxiv:strong"`) || !strings.Contains(fixtures, "Sequential Recommendation") {
		t.Fatalf("fixtures = %s", fixtures)
	}

	store, err = state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	kept, err := store.GetJob(context.Background(), job.Topic, "2026-10-01")
	if err != nil || kept.Status != "new" {
		t.Fatalf("existing job changed: %+v %v", kept, err)
	}
	if _, err := store.GetJob(context.Background(), job.Topic, "2026-10-09"); !errors.Is(err, state.ErrJobNotFound) {
		t.Fatalf("preview created a digest job: %v", err)
	}
	seen, err := store.Seen(context.Background(), job.Topic, "arxiv:strong")
	if err != nil || seen {
		t.Fatalf("preview marked a paper seen: %v %v", seen, err)
	}
}

func TestEvalRejectsBadDatesBeforeOpeningTheDatabase(t *testing.T) {
	database := filepath.Join(t.TempDir(), "nested", "digest.db")
	args := configArgs(t, database, "", "", false, "eval", "--date", "2026-10-01", "--to", "2026-10-20")
	if err := run(args); err == nil || !strings.Contains(err.Error(), "14") {
		t.Fatalf("long range err = %v", err)
	}
	if _, err := os.Stat(database); !os.IsNotExist(err) {
		t.Fatal("invalid eval created a database")
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	if err := run([]string{"--config", missing, "eval", "--date", "2026-10-09"}); err == nil || !strings.Contains(err.Error(), "读取配置文件失败") {
		t.Fatalf("missing config err = %v", err)
	}
	if err := run(configArgs(t, filepath.Join(t.TempDir(), "digest.db"), "", "", false, "eval", "--topic", "another", "--date", "2026-10-09")); err == nil || !strings.Contains(err.Error(), "没有筛选规则") {
		t.Fatalf("other topic err = %v", err)
	}
}

func TestEvalDraftRulesDoNotRewriteConfigOrCreateJobs(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "digest.db")
	configPath := filepath.Join(dir, "config.json")
	configBody := `{"database":{"path":"` + database + `"},"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search"}]}`
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	rulesPath := filepath.Join(dir, "candidate.json")
	rulesBody := `{"max_papers":5,"min_tier":1,"query":{"categories":["cs.LG"]},"signals":[{"name":"title:hit","pattern":"\\bzzzz-no-match\\b","fields":["title"],"tier":2}],"decisions":[{"tier":2,"reason":"title match","min_signal_tier":2}],"fallback_reason":"no topic signal"}`
	if err := os.WriteFile(rulesPath, []byte(rulesBody), 0o600); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "papers.json")
	papersBody := `[{"ID":"arxiv:strong","Title":"Sequential Recommendation","Abstract":"session logs","Published":"2026-10-07T17:38:35Z","URL":"https://arxiv.org/abs/strong","categories":["cs.IR"]}]`
	if err := os.WriteFile(input, []byte(papersBody), 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := captureStdout(t, func() error {
		return run([]string{"--config", configPath, "eval", "--date", "2026-10-09", "--input", input, "--rules", rulesPath})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "草稿入选 0 篇") || !strings.Contains(text, "草稿规则不会写回配置") || !strings.Contains(text, "仅当前规则（草稿未入选）") || !strings.Contains(text, "arxiv:strong") {
		t.Fatalf("draft preview = %s", text)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("config changed:\n%s", after)
	}
	store, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.GetJob(context.Background(), job.Topic, "2026-10-09"); !errors.Is(err, state.ErrJobNotFound) {
		t.Fatalf("draft eval created a job: %v", err)
	}

	missingDB := filepath.Join(t.TempDir(), "missing.db")
	rulesConfig := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(rulesConfig, []byte(`{"database":{"path":"`+missingDB+`"},"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	printed, err := captureStdout(t, func() error {
		return run([]string{"--config", rulesConfig, "eval", "rules"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(printed, `"max_papers": 5`) || !strings.Contains(printed, `"fallback_reason"`) {
		t.Fatalf("eval rules = %s", printed)
	}
	if _, err := os.Stat(missingDB); !os.IsNotExist(err) {
		t.Fatal("eval rules opened the database")
	}
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, reader)
		close(done)
	}()
	runErr := fn()
	_ = writer.Close()
	os.Stdout = old
	<-done
	_ = reader.Close()
	return buf.String(), runErr
}
