package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shichao-wang/paper-digest/internal/library"
)

type delayedEvidence struct {
	delay time.Duration
}

func (d delayedEvidence) BlockText(ctx context.Context, doc library.Document, blockID string) (library.Block, error) {
	timer := time.NewTimer(d.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return library.Block{}, ctx.Err()
	case <-timer.C:
		for _, block := range doc.Blocks {
			if block.ID == blockID {
				return block, nil
			}
		}
		return library.Block{}, library.ErrNotFound
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func TestEvidenceVerificationHasIndependentTimeout(t *testing.T) {
	for _, tc := range []struct {
		name          string
		delay         time.Duration
		callerTimeout time.Duration
		wantStatus    int
		wantElapsed   time.Duration
	}{
		{"past_generic_timeout", 6 * time.Second, 0, 200, 6 * time.Second},
		{"bounded_verification", evidenceTimeout + time.Second, 0, 503, evidenceTimeout},
		{"caller_deadline", 6 * time.Second, 2 * time.Second, 503, 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newLibraryFixture(t)
				s := &server{store: fixture.store, documents: delayedEvidence{delay: tc.delay}}
				ctx := context.Background()
				if tc.callerTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.callerTimeout)
					defer cancel()
				}
				w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
				r := httptest.NewRequest("GET", evidenceTarget(fixture.id, fixture.doc.ID, fixture.doc.Blocks[0].ID), nil).WithContext(ctx)
				start := time.Now()
				s.serveHTTP(w, r)
				if w.Code != tc.wantStatus || time.Since(start) != tc.wantElapsed {
					t.Fatalf("response=%d elapsed=%s body=%s", w.Code, time.Since(start), w.Body.String())
				}
				wantDeadline := start.Add(evidenceTimeout)
				if tc.callerTimeout > 0 {
					wantDeadline = start.Add(tc.callerTimeout)
				}
				if !w.deadline.Equal(wantDeadline) {
					t.Fatalf("write deadline=%s want=%s", w.deadline, wantDeadline)
				}
				if tc.wantStatus == 200 {
					var block library.Block
					if err := json.Unmarshal(w.Body.Bytes(), &block); err != nil || block != fixture.doc.Blocks[0] {
						t.Fatalf("block=%+v err=%v", block, err)
					}
				}
			})
		})
	}
}

func TestEvidenceExtendsActualServerWriteDeadline(t *testing.T) {
	fixture := newLibraryFixture(t)
	s := &server{store: fixture.store}
	verifiedDeadline := make(chan time.Time, 1)
	httpServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 保留真实 ResponseController，以检查 net/http 的写入期限覆盖。
		recorder := &forwardDeadlineWriter{ResponseWriter: w, deadline: verifiedDeadline}
		s.serveHTTP(recorder, r)
	}))
	httpServer.Config.WriteTimeout = 15 * time.Second
	httpServer.Start()
	defer httpServer.Close()
	start := time.Now()
	response, err := httpServer.Client().Get(httpServer.URL + evidenceTarget(fixture.id, fixture.doc.ID, fixture.doc.Blocks[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	deadline := <-verifiedDeadline
	if response.StatusCode != 200 || deadline.Before(start.Add(evidenceTimeout)) {
		t.Fatalf("response=%d deadline=%s", response.StatusCode, deadline)
	}
}

type forwardDeadlineWriter struct {
	http.ResponseWriter
	deadline chan<- time.Time
}

func (w *forwardDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline <- deadline
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
}

func TestEvidenceWriteDeadlineDoesNotAffectOtherAPIs(t *testing.T) {
	store, _ := testHandler(t)
	s := &server{store: store}
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.serveHTTP(w, httptest.NewRequest("GET", "/api/health", nil))
	if w.Code != 200 || !w.deadline.IsZero() {
		t.Fatalf("health response=%d write deadline=%s", w.Code, w.deadline)
	}
}
