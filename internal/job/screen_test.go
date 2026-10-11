package job

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/digest"
	"github.com/shichao-wang/paper-digest/internal/papers"
)

func TestScreeningFailureSendsNothingAndStaysMissed(t *testing.T) {
	r := testRunner(t)
	date := BeijingDate(r.Now())
	r.Screen = screenFunc(func(context.Context, []papers.Paper) ([]digest.Decision, error) {
		return nil, errors.New("invalid json")
	})
	err := r.Generate(context.Background(), date)
	if err == nil || !errors.Is(err, errScreeningClosed) || !strings.Contains(err.Error(), "精筛失败，当天不发送") {
		t.Fatalf("Generate() = %v", err)
	}
	current, err := r.Store.GetJob(context.Background(), Topic, date)
	if err != nil || current.Status != "missed" || !strings.Contains(current.Message, "invalid json") {
		t.Fatalf("job = %+v, err %v", current, err)
	}
	pending, err := r.Store.PendingPapers(context.Background(), Topic, date)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %+v, err %v", pending, err)
	}
	rows, err := r.Store.Screening(context.Background(), Topic, date)
	if err != nil || len(rows) != 0 {
		t.Fatalf("screening = %+v, err %v", rows, err)
	}
	fetches := 0
	r.Fetch = func(context.Context, papers.Batch) ([]papers.Paper, error) {
		fetches++
		return nil, nil
	}
	if err := r.Generate(context.Background(), date); err != nil || fetches != 0 || jobStatus(t, r, date) != "missed" {
		t.Fatalf("retry err=%v fetches=%d", err, fetches)
	}
}

func TestCanceledScreeningCanResume(t *testing.T) {
	r := testRunner(t)
	date := BeijingDate(r.Now())
	r.Screen = screenFunc(func(context.Context, []papers.Paper) ([]digest.Decision, error) {
		return nil, context.Canceled
	})
	if err := r.Generate(context.Background(), date); !errors.Is(err, context.Canceled) {
		t.Fatalf("Generate() = %v", err)
	}
	current, err := r.Store.GetJob(context.Background(), Topic, date)
	if err != nil || current.Status != "new" || current.Message != "" {
		t.Fatalf("shutdown must leave the day new: %+v, %v", current, err)
	}
	r.Screen = acceptAllScreen()
	if err := r.Generate(context.Background(), date); err != nil || jobStatus(t, r, date) != "ready" {
		t.Fatalf("resume err=%v status=%s", err, jobStatus(t, r, date))
	}
}

func TestDeadlineDuringScreeningFailsClosed(t *testing.T) {
	r := testRunner(t)
	date := BeijingDate(r.Now())
	r.Screen = screenFunc(func(context.Context, []papers.Paper) ([]digest.Decision, error) {
		return nil, context.DeadlineExceeded
	})
	if err := r.Generate(context.Background(), date); !errors.Is(err, errScreeningClosed) {
		t.Fatalf("Generate() = %v", err)
	}
	if jobStatus(t, r, date) != "missed" {
		t.Fatal(jobStatus(t, r, date))
	}
}

func TestIrrelevantScreenIsAnEmptyDigest(t *testing.T) {
	r := testRunner(t)
	date := BeijingDate(r.Now())
	analyzed := 0
	r.Analyzer = analyzerFunc(func(context.Context, papers.Paper) (digest.Summary, error) {
		analyzed++
		return digest.Summary{}, nil
	})
	r.Screen = screenFunc(func(_ context.Context, list []papers.Paper) ([]digest.Decision, error) {
		out := make([]digest.Decision, len(list))
		for i, paper := range list {
			out[i] = digest.Decision{ID: paper.ID, Relevant: false, Score: 0.2, Reason: "不是检索", Skill: "paper-screening@test"}
		}
		return out, nil
	})
	if err := r.Generate(context.Background(), date); err != nil {
		t.Fatal(err)
	}
	current, err := r.Store.GetJob(context.Background(), Topic, date)
	if err != nil || current.Status != "ready" || !strings.Contains(current.Message, "未发现") || analyzed != 0 {
		t.Fatalf("job=%+v analyzed=%d err=%v", current, analyzed, err)
	}
	rows, err := r.Store.Screening(context.Background(), Topic, date)
	if err != nil || len(rows) != 1 || rows[0].Relevant || rows[0].Reason != "不是检索" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestHigherScreeningScoreIsPushedFirst(t *testing.T) {
	r := testRunner(t)
	date := BeijingDate(r.Now())
	spec := papers.DefaultSelection()
	spec.MaxPapers = 1
	rules, err := papers.Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	r.Rules = rules
	r.Fetch = func(context.Context, papers.Batch) ([]papers.Paper, error) {
		batch, ok, err := papers.DigestBatch(date)
		if err != nil || !ok {
			t.Fatalf("batch ok=%v err=%v", ok, err)
		}
		return []papers.Paper{
			{ID: "arxiv:newer", Title: "Web Search Ranking", Published: batch.Window.Through.Add(-time.Hour)},
			{ID: "arxiv:older", Title: "Web Search Ranking", Published: batch.Window.Through.Add(-2 * time.Hour)},
		}, nil
	}
	r.Screen = screenFunc(func(context.Context, []papers.Paper) ([]digest.Decision, error) {
		return []digest.Decision{
			{ID: "arxiv:newer", Relevant: true, Score: 0.4, Reason: "一般", Skill: "paper-screening@test"},
			{ID: "arxiv:older", Relevant: true, Score: 0.9, Reason: "更相关", Skill: "paper-screening@test"},
		}, nil
	})
	var analyzed []string
	r.Analyzer = analyzerFunc(func(_ context.Context, paper papers.Paper) (digest.Summary, error) {
		analyzed = append(analyzed, paper.ID)
		return digest.Summary{Text: "基于摘要的中文要点", Model: "fixture", PromptVersion: "test"}, nil
	})
	if err := r.Generate(context.Background(), date); err != nil {
		t.Fatal(err)
	}
	if len(analyzed) != 1 || analyzed[0] != "arxiv:older" {
		t.Fatalf("summarized %v", analyzed)
	}
}

func TestOctoberNinthNegativesStayOutOfTheDigest(t *testing.T) {
	data, err := os.ReadFile("../papers/testdata/2026-10-09-ras.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ID         string    `json:"id"`
		Title      string    `json:"title"`
		Abstract   string    `json:"abstract"`
		Published  time.Time `json:"published"`
		Categories []string  `json:"categories"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	fetched := make([]papers.Paper, 0, len(rows))
	for _, row := range rows {
		fetched = append(fetched, papers.Paper{
			ID: row.ID, Title: row.Title, Abstract: row.Abstract, Published: row.Published, Categories: row.Categories,
			URL: "https://arxiv.org/abs/" + strings.TrimPrefix(row.ID, "arxiv:"),
		})
	}
	negatives := []string{"arxiv:2610.10441", "arxiv:2610.10256", "arxiv:2610.10224", "arxiv:2610.10211"}

	r := testRunner(t)
	r.Fetch = func(context.Context, papers.Batch) ([]papers.Paper, error) { return fetched, nil }
	var screened []string
	r.Screen = screenFunc(func(_ context.Context, list []papers.Paper) ([]digest.Decision, error) {
		out := make([]digest.Decision, len(list))
		for i, paper := range list {
			screened = append(screened, paper.ID)
			out[i] = digest.Decision{ID: paper.ID, Relevant: false, Score: 0.15, Reason: "测试用：全部拒绝", Skill: "paper-screening@test"}
		}
		return out, nil
	})
	if err := r.Generate(context.Background(), "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	seenScreen := map[string]bool{}
	for _, id := range screened {
		seenScreen[id] = true
	}
	if !seenScreen["arxiv:2610.10483"] {
		t.Fatalf("screened %v, want the coarse softmax paper", screened)
	}
	for _, id := range negatives {
		if seenScreen[id] {
			t.Fatalf("negative %s was screened: %v", id, screened)
		}
	}
	pending, err := r.Store.PendingPapers(context.Background(), Topic, "2026-10-09")
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	rowsSaved, err := r.Store.Screening(context.Background(), Topic, "2026-10-09")
	if err != nil || len(rowsSaved) == 0 {
		t.Fatalf("screening = %+v, %v", rowsSaved, err)
	}
	foundSoftmax := false
	for _, row := range rowsSaved {
		if row.Relevant {
			t.Fatalf("rejected screen was stored as relevant: %+v", row)
		}
		if row.PaperID == "arxiv:2610.10483" {
			foundSoftmax = true
		}
		for _, id := range negatives {
			if row.PaperID == id {
				t.Fatalf("negative stored in screening: %s", id)
			}
		}
	}
	if !foundSoftmax {
		t.Fatalf("screening = %+v", rowsSaved)
	}
	current, err := r.Store.GetJob(context.Background(), Topic, "2026-10-09")
	if err != nil || current.Status != "ready" || !strings.Contains(current.Message, "未发现") {
		t.Fatalf("job = %+v, %v", current, err)
	}

	r = testRunner(t)
	r.Fetch = func(context.Context, papers.Batch) ([]papers.Paper, error) { return fetched, nil }
	r.Screen = screenFunc(func(_ context.Context, list []papers.Paper) ([]digest.Decision, error) {
		out := make([]digest.Decision, len(list))
		for i, paper := range list {
			out[i] = digest.Decision{ID: paper.ID, Relevant: paper.ID == "arxiv:2610.10483", Score: 0.7, Reason: "保留粗筛正例", Skill: "paper-screening@test"}
		}
		return out, nil
	})
	var summarized []string
	r.Analyzer = analyzerFunc(func(_ context.Context, paper papers.Paper) (digest.Summary, error) {
		summarized = append(summarized, paper.ID)
		return digest.Summary{Text: "基于摘要的中文要点", Model: "fixture", PromptVersion: "test"}, nil
	})
	if err := r.Generate(context.Background(), "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	if len(summarized) != 1 || summarized[0] != "arxiv:2610.10483" {
		t.Fatalf("summarized %v", summarized)
	}
	for _, id := range negatives {
		seen, err := r.Store.Seen(context.Background(), Topic, id)
		if err != nil || seen {
			t.Fatalf("%s seen=%v err=%v", id, seen, err)
		}
	}
}
