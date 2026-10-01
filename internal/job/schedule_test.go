package job

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

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
