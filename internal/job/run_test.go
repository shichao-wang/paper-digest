package job

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/delivery"
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

func TestDatabaseSenderResolvesBeforeClaimAndUpdatesDynamically(t *testing.T) {
	r := testRunner(t)
	ctx := context.Background()
	paths := make([]string, 0)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	r.Sender = delivery.StoredFeishu{Store: r.Store, Topic: Topic, Client: server.Client()}
	date := BeijingDate(r.Now())
	if err := r.Generate(ctx, date); err != nil {
		t.Fatal(err)
	}
	if err := r.Deliver(ctx, date); err == nil || jobStatus(t, r, date) != "ready" || len(paths) != 0 {
		t.Fatal("缺少地址必须在 ClaimSend 前失败")
	}
	if err := r.Store.SetWebhook(ctx, Topic, server.URL+"/first-secret"); err != nil {
		t.Fatal(err)
	}
	if err := r.Deliver(ctx, date); err != nil {
		t.Fatal(err)
	}
	if err := r.Deliver(ctx, date); err == nil || len(paths) != 1 {
		t.Fatal("动态sender破坏防重发")
	}
	const nextDate = "2026-09-28"
	if _, err := r.Store.ClaimDay(ctx, Topic, nextDate); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.Ready(ctx, Topic, nextDate, "persisted message"); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.SetWebhook(ctx, Topic, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.Deliver(ctx, nextDate); err == nil || jobStatus(t, r, nextDate) != "ready" || len(paths) != 1 {
		t.Fatal("清除后不得进入sending或重用旧地址")
	}
	if err := r.Store.SetWebhook(ctx, Topic, server.URL+"/second-secret"); err != nil {
		t.Fatal(err)
	}
	if err := r.Deliver(ctx, nextDate); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "/first-secret" || paths[1] != "/second-secret" || jobStatus(t, r, nextDate) != "sent" {
		t.Fatal("未使用更新后的数据库地址")
	}
	const failedDate = "2026-09-29"
	if _, err := r.Store.ClaimDay(ctx, Topic, failedDate); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.Ready(ctx, Topic, failedDate, "persisted message"); err != nil {
		t.Fatal(err)
	}
	server.Close()
	err := r.Deliver(ctx, failedDate)
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) || jobStatus(t, r, failedDate) != "unknown" {
		t.Fatalf("动态发送失败必须保持防重发且错误不泄漏: %v", err)
	}
	if err := r.Deliver(ctx, failedDate); err == nil || jobStatus(t, r, failedDate) != "unknown" {
		t.Fatal("unknown不得重试")
	}
}

type webhookReadStore struct {
	store *state.Store
	read  chan struct{}
}

func (f webhookReadStore) Webhook(ctx context.Context, topic string) (string, error) {
	value, err := f.store.Webhook(ctx, topic)
	close(f.read)
	return value, err
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

func TestDeliverOneMessagePerPaperAndPreserveConfirmedPartialBatch(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		r := testRunner(t)
		ctx := context.Background()
		date := BeijingDate(r.Now())
		r.Fetch = func(context.Context) ([]papers.Paper, error) {
			return []papers.Paper{
				{ID: "arxiv:2610.00001", Version: "v1", Title: "First Recommendation", Abstract: "recommendation", Published: r.Now().Add(-time.Hour)},
				{ID: "arxiv:2610.00002", Version: "v1", Title: "Second Recommendation", Abstract: "recommendation", Published: r.Now().Add(-time.Hour)},
			}, nil
		}
		if err := r.Generate(ctx, date); err != nil {
			t.Fatal(err)
		}
		var messages []string
		r.Sender = senderFunc(func(_ context.Context, message string) error {
			messages = append(messages, message)
			if len(messages) == 2 && failSecond {
				return errors.New("timeout")
			}
			return nil
		})
		err := r.Deliver(ctx, date)
		if (err != nil) != failSecond || len(messages) != 2 {
			t.Fatalf("batch: %v, messages=%d", err, len(messages))
		}
		for i, message := range messages {
			for j, id := range []string{"arxiv:2610.00001", "arxiv:2610.00002"} {
				if strings.Contains(message, id) != (i == j) {
					t.Fatalf("message %d contains wrong paper: %s", i, message)
				}
			}
			if !strings.Contains(message, "2026-09-27") {
				t.Fatal("missing digest date")
			}
		}
		first, _ := r.Store.Seen(ctx, Topic, "arxiv:2610.00001")
		second, _ := r.Store.Seen(ctx, Topic, "arxiv:2610.00002")
		if !first || second == failSecond {
			t.Fatalf("confirmed delivery history lost: %v %v", first, second)
		}
		want := "sent"
		if failSecond {
			want = "unknown"
		}
		if jobStatus(t, r, date) != want {
			t.Fatal("incorrect batch status")
		}
		if err := r.Deliver(ctx, date); err == nil || len(messages) != 2 {
			t.Fatal("batch must not automatically replay")
		}
	}
}

func TestOversizedLaterCardRejectsWholeBatchBeforeClaim(t *testing.T) {
	r := testRunner(t)
	ctx := context.Background()
	date := BeijingDate(r.Now())
	r.Fetch = func(context.Context) ([]papers.Paper, error) {
		return []papers.Paper{
			{ID: "arxiv:2610.00001", Title: "First Recommendation", Abstract: "recommendation", Published: r.Now()},
			{ID: "arxiv:2610.00002", Title: "Second Recommendation", Abstract: "recommendation", Published: r.Now()},
		}, nil
	}
	r.Analyzer = analyzerFunc(func(_ context.Context, paper papers.Paper) (digest.Summary, error) {
		text := "方法：正常摘要"
		if paper.ID == "arxiv:2610.00002" {
			text = strings.Repeat("中", 7000)
		}
		return digest.Summary{Text: text}, nil
	})
	if err := r.Generate(ctx, date); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	if err := r.Store.SetWebhook(ctx, Topic, server.URL); err != nil {
		t.Fatal(err)
	}
	r.Sender = delivery.StoredFeishu{Store: r.Store, Topic: Topic, Client: server.Client()}
	for attempt := 0; attempt < 2; attempt++ {
		err := r.Deliver(ctx, date)
		if err == nil || !strings.Contains(err.Error(), "第 2/2") || !strings.Contains(err.Error(), "20 KB") {
			t.Fatalf("expected second card preflight rejection: %v", err)
		}
		if requests != 0 || jobStatus(t, r, date) != "ready" {
			t.Fatalf("local rejection must remain ready without any requests: requests=%d", requests)
		}
	}
	for _, id := range []string{"arxiv:2610.00001", "arxiv:2610.00002"} {
		seen, err := r.Store.Seen(ctx, Topic, id)
		if err != nil || seen {
			t.Fatalf("unsent paper recorded: seen=%v err=%v", seen, err)
		}
	}
}
