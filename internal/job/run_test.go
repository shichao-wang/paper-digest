package job

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/digest"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/internal/state"
)

type analyzerFunc func(context.Context, papers.Paper) (digest.Summary, error)

func (f analyzerFunc) Analyze(ctx context.Context, paper papers.Paper) (digest.Summary, error) {
	return f(ctx, paper)
}

type senderFunc func(context.Context, string) error

func (f senderFunc) Send(ctx context.Context, message string) error { return f(ctx, message) }

func testRunner(t *testing.T) *Runner {
	t.Helper()
	store, err := state.Open(t.TempDir() + "/digest.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, beijing())
	return &Runner{
		Store: store,
		Fetch: func(context.Context) ([]papers.Paper, error) {
			return []papers.Paper{{ID: "arxiv:2609.12345", Version: "v1", Title: "Neural Recommender Systems", Abstract: "A new recommendation and search retrieval method", Published: now.Add(-time.Hour), URL: "https://arxiv.org/abs/2609.12345v1"}}, nil
		},
		Analyzer: analyzerFunc(func(context.Context, papers.Paper) (digest.Summary, error) {
			return digest.Summary{Text: "基于摘要的中文要点", Model: "fixture", PromptVersion: "test"}, nil
		}),
		LookbackDays: 7,
		Now:          func() time.Time { return now },
	}
}

func jobStatus(t *testing.T, r *Runner, date string) string {
	t.Helper()
	current, err := r.Store.GetJob(context.Background(), Topic, date)
	if err != nil {
		t.Fatal(err)
	}
	return current.Status
}

func TestGenerateResumeAndDeliverOnce(t *testing.T) {
	r := testRunner(t)
	ctx := context.Background()
	date := BeijingDate(r.Now())
	calls := 0
	r.Analyzer = analyzerFunc(func(context.Context, papers.Paper) (digest.Summary, error) {
		calls++
		if calls == 1 {
			return digest.Summary{}, errors.New("temporary model failure")
		}
		return digest.Summary{Text: "基于摘要的中文要点", Model: "fixture", PromptVersion: "test"}, nil
	})
	if err := r.Generate(ctx, date); err == nil || jobStatus(t, r, date) != "processing" {
		t.Fatal("模型失败不能发半成品")
	}
	r.Fetch = func(context.Context) ([]papers.Paper, error) {
		t.Fatal("恢复任务不得重新抓取和改写候选")
		return nil, nil
	}
	if err := r.Generate(ctx, date); err != nil {
		t.Fatal(err)
	}
	if jobStatus(t, r, date) != "ready" || calls != 2 {
		t.Fatalf("恢复状态异常：status=%s calls=%d", jobStatus(t, r, date), calls)
	}
	var sends int
	r.Sender = senderFunc(func(_ context.Context, message string) error {
		sends++
		if !strings.Contains(message, "基于摘要的中文要点") {
			t.Errorf("没有使用已持久化的日报：%s", message)
		}
		return nil
	})
	if err := r.Deliver(ctx, date); err != nil {
		t.Fatal(err)
	}
	if err := r.Deliver(ctx, date); err == nil || sends != 1 || jobStatus(t, r, date) != "sent" {
		t.Fatal("同一天重复发送未被阻止")
	}
	seen, err := r.Store.Seen(ctx, Topic, "arxiv:2609.12345")
	if err != nil || !seen {
		t.Fatalf("成功发送后应记录推荐历史：seen=%v err=%v", seen, err)
	}
}

func TestDeliveryFailureBecomesUnknown(t *testing.T) {
	r := testRunner(t)
	ctx := context.Background()
	date := BeijingDate(r.Now())
	if err := r.Generate(ctx, date); err != nil {
		t.Fatal(err)
	}
	sends := 0
	r.Sender = senderFunc(func(context.Context, string) error {
		sends++
		return errors.New("uncertain response")
	})
	if err := r.Deliver(ctx, date); err == nil {
		t.Fatal("发送错误未报告")
	}
	if err := r.Deliver(ctx, date); err == nil || sends != 1 || jobStatus(t, r, date) != "unknown" {
		t.Fatal("发送结果未知时不得盲目重试")
	}
	seen, err := r.Store.Seen(ctx, Topic, "arxiv:2609.12345")
	if err != nil || seen {
		t.Fatalf("发送未确认时不得标记推荐：seen=%v err=%v", seen, err)
	}
}

func TestEmptyDigestIsReady(t *testing.T) {
	r := testRunner(t)
	r.Fetch = func(context.Context) ([]papers.Paper, error) { return nil, nil }
	r.Analyzer = analyzerFunc(func(context.Context, papers.Paper) (digest.Summary, error) {
		t.Fatal("空日报不应调用模型")
		return digest.Summary{}, nil
	})
	date := BeijingDate(r.Now())
	if err := r.Generate(context.Background(), date); err != nil {
		t.Fatal(err)
	}
	current, err := r.Store.GetJob(context.Background(), Topic, date)
	if err != nil || current.Status != "ready" || !strings.Contains(current.Message, "未发现") {
		t.Fatalf("空日报应完整就绪：job=%+v err=%v", current, err)
	}
}

func TestBeijingDateBoundary(t *testing.T) {
	if got := BeijingDate(time.Date(2026, 9, 26, 16, 0, 0, 0, time.UTC)); got != "2026-09-27" {
		t.Fatalf("北京时间日期错误：%s", got)
	}
}
