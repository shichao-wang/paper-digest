package job

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/delivery"
	"github.com/shichao-wang/paper-digest/internal/papers"
)

func TestServeWaitsForBackgroundWorkAndDeliveryOutcome(t *testing.T) {
	for _, kind := range []string{"generate", "deliver"} {
		t.Run(kind, func(t *testing.T) {
			r := testRunner(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			if kind == "generate" {
				r.Now = func() time.Time { return time.Date(2099, 9, 27, 8, 1, 0, 0, beijing()) }
				r.Fetch = func(ctx context.Context) ([]papers.Paper, error) {
					close(entered)
					<-ctx.Done()
					close(canceled)
					<-release
					return nil, ctx.Err()
				}
			} else {
				if err := r.Generate(ctx, "2026-09-27"); err != nil {
					t.Fatal(err)
				}
				r.Now = func() time.Time { return time.Date(2026, 9, 27, 9, 0, 0, 0, beijing()) }
				r.Sender = senderFunc(func(ctx context.Context, _ string) error {
					close(entered)
					<-ctx.Done()
					close(canceled)
					<-release
					return ctx.Err()
				})
			}
			done := make(chan error, 1)
			go func() { done <- r.Serve(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("background work did not start")
			}
			cancel()
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("work context not canceled")
			}
			select {
			case err := <-done:
				t.Fatalf("Serve returned before background work completed: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Serve returned %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Serve did not join work")
			}
			if kind == "deliver" && jobStatus(t, r, "2026-09-27") != "unknown" {
				t.Fatal("delivery final state not saved before Serve returned")
			}
		})
	}
}

func TestNoWebhookDoesNotBecomeUnknownOrSendAfterWindow(t *testing.T) {
	r := testRunner(t)
	ctx := context.Background()
	date := BeijingDate(r.Now())
	if err := r.Generate(ctx, date); err != nil {
		t.Fatal(err)
	}
	read := make(chan struct{})
	r.Sender = delivery.StoredFeishu{Store: webhookReadStore{store: r.Store, read: read}, Topic: Topic}
	r.Now = func() time.Time { return time.Date(2026, 9, 27, 9, 0, 0, 0, beijing()) }
	wait, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Serve(wait, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	select {
	case <-read:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("scheduler未读取地址")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler未停止")
	}
	if jobStatus(t, r, date) != "ready" {
		t.Fatal("缺地址不得标记unknown")
	}
	r.Now = func() time.Time { return time.Date(2026, 9, 27, 9, 1, 0, 0, beijing()) }
	calls := 0
	r.Sender = senderFunc(func(context.Context, string) error { calls++; return nil })
	wait, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_ = r.Serve(wait, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if calls != 0 || jobStatus(t, r, date) != "missed" {
		t.Fatal("错过窗口不得补发")
	}
}

// startSchedule 在同一个调度实例中推进时间，每次推进等待该轮检查结束。
func startSchedule(t *testing.T, r *Runner) func(time.Time) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	now := make(chan time.Time)
	ticks := make(chan time.Time)
	r.Now = func() time.Time {
		select {
		case at := <-now:
			return at
		case <-ctx.Done():
			return time.Date(2026, 9, 27, 9, 2, 0, 0, beijing())
		}
	}
	done := make(chan error, 1)
	go func() { done <- r.serve(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), ticks) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("调度退出错误: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("调度未停止")
		}
	})
	return func(at time.Time) {
		t.Helper()
		select {
		case now <- at:
		case <-time.After(time.Second):
			t.Fatal("调度未读取时间")
		}
		select {
		case ticks <- at:
		case <-time.After(time.Second):
			t.Fatal("调度未结束本轮检查")
		}
	}
}

type observedPreparer struct {
	delivery.StoredFeishu
	calls    atomic.Int32
	prepared chan error
}

func (s *observedPreparer) Prepare(ctx context.Context) (delivery.Sender, error) {
	s.calls.Add(1)
	sender, err := s.StoredFeishu.Prepare(ctx)
	s.prepared <- err
	return sender, err
}

func TestContinuousScheduleMissesUnconfiguredWebhookWithoutLateSend(t *testing.T) {
	r := testRunner(t)
	ctx := context.Background()
	date := BeijingDate(r.Now())
	if err := r.Generate(ctx, date); err != nil {
		t.Fatal(err)
	}
	var sends atomic.Int32
	robot := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer robot.Close()
	sender := &observedPreparer{
		StoredFeishu: delivery.StoredFeishu{Store: r.Store, Topic: Topic, Client: robot.Client()},
		prepared:     make(chan error, 10),
	}
	r.Sender = sender
	advance := startSchedule(t, r)
	at := time.Date(2026, 9, 27, 9, 0, 0, 0, beijing())
	advance(at)
	select {
	case err := <-sender.prepared:
		if err == nil {
			t.Fatal("缺地址未使 Prepare 失败")
		}
	case <-time.After(time.Second):
		t.Fatal("未准备发送")
	}
	if jobStatus(t, r, date) != "ready" {
		t.Fatal("准备失败不得记录发送意图")
	}
	advance(at.Add(30 * time.Second))
	if sender.calls.Load() != 1 || sends.Load() != 0 {
		t.Fatal("同一发送窗口重复尝试")
	}
	advance(at.Add(time.Minute))
	if jobStatus(t, r, date) != "missed" {
		t.Fatal("持续运行错过窗口未标记 missed")
	}
	if err := r.Store.SetWebhook(ctx, Topic, robot.URL+"/late-secret"); err != nil {
		t.Fatal(err)
	}
	advance(at.Add(2 * time.Minute))
	advance(at.Add(time.Hour))
	if sender.calls.Load() != 1 || sends.Load() != 0 || jobStatus(t, r, date) != "missed" {
		t.Fatal("晚配置地址不得补发或重复准备")
	}
}

func TestContinuousSchedulePreservesDeliveryOutcomeWithoutDuplicateSend(t *testing.T) {
	for _, outcome := range []string{"sent", "unknown", "sending"} {
		t.Run(outcome, func(t *testing.T) {
			r := testRunner(t)
			date := BeijingDate(r.Now())
			if err := r.Generate(context.Background(), date); err != nil {
				t.Fatal(err)
			}
			var sends atomic.Int32
			entered := make(chan struct{}, 10)
			r.Sender = senderFunc(func(ctx context.Context, _ string) error {
				sends.Add(1)
				entered <- struct{}{}
				if outcome == "sending" {
					<-ctx.Done()
					return ctx.Err()
				}
				if outcome == "unknown" {
					return errors.New("响应未确认")
				}
				return nil
			})
			advance := startSchedule(t, r)
			at := time.Date(2026, 9, 27, 9, 0, 0, 0, beijing())
			advance(at)
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("未开始发送")
			}
			deadline := time.Now().Add(time.Second)
			for jobStatus(t, r, date) != outcome {
				if time.Now().After(deadline) {
					t.Fatalf("发送状态未变为 %s", outcome)
				}
				time.Sleep(time.Millisecond)
			}
			advance(at.Add(30 * time.Second))
			advance(at.Add(time.Minute))
			advance(at.Add(2 * time.Minute))
			if sends.Load() != 1 || jobStatus(t, r, date) != outcome {
				t.Fatalf("重复发送或覆盖发送状态: sends=%d status=%s", sends.Load(), jobStatus(t, r, date))
			}
		})
	}
}

func TestStartupAfterSendWindowMissesReadyJob(t *testing.T) {
	r := testRunner(t)
	ctx := context.Background()
	date := BeijingDate(r.Now())
	if err := r.Generate(ctx, date); err != nil {
		t.Fatal(err)
	}
	r.Now = func() time.Time { return time.Date(2026, 9, 27, 9, 1, 0, 0, beijing()) }
	sent := false
	r.Sender = senderFunc(func(context.Context, string) error { sent = true; return nil })
	wait, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	_ = r.Serve(wait, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if sent || jobStatus(t, r, date) != "missed" {
		t.Fatalf("错过发送窗口仍发送或未标记 missed：sent=%v status=%s", sent, jobStatus(t, r, date))
	}
}
