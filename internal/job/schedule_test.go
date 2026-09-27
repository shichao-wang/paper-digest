package job

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

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
