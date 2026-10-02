// Package arxivclient shares public arXiv request pacing across all collectors.
package arxivclient

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const RequestInterval = 3 * time.Second

var publicRequests = startLimiter{interval: RequestInterval}

// Do paces every public arXiv request, including redirects. Client.Timeout is a
// shared budget for HTTP transfer through response body EOF/Close across all
// redirect hops; public pacing waits do not consume it. The request context still
// bounds the whole operation, including waits. The supplied client is not changed.
// Injected loopback servers and unrelated services are not delayed.
func Do(client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	transport := copy.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	// net/http starts Client.Timeout before RoundTrip (and therefore before
	// pacing). Move that budget into the transport, after each pacing grant.
	paced := &pacedTransport{base: transport, limiter: &publicRequests, timed: copy.Timeout > 0}
	paced.remaining.Store(int64(copy.Timeout))
	copy.Transport = paced
	copy.Timeout = 0
	return copy.Do(req)
}

type pacedTransport struct {
	base      http.RoundTripper
	limiter   *startLimiter
	timed     bool
	remaining atomic.Int64
}

func (t *pacedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if t.timed && t.remaining.Load() <= 0 {
		return nil, context.DeadlineExceeded
	}
	host := strings.ToLower(strings.TrimSuffix(req.URL.Hostname(), "."))
	if host == "arxiv.org" || strings.HasSuffix(host, ".arxiv.org") {
		if err := t.limiter.wait(req.Context()); err != nil {
			return nil, err
		}
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if !t.timed {
		return t.base.RoundTrip(req)
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(req.Context(), time.Duration(t.remaining.Load()))
	finish := func() {
		t.remaining.Add(-int64(time.Since(started)))
		cancel()
	}
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		finish()
		return resp, err
	}
	if ctx.Err() != nil {
		err = ctx.Err()
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		finish()
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		finish()
		return resp, nil // Leave invalid transport responses to net/http.
	}
	resp.Body = &transferBody{ReadCloser: resp.Body, ctx: ctx, finish: finish}
	return resp, nil
}

// Keep the transfer deadline alive after headers arrive. Redirect bodies are
// drained/closed by net/http before its next RoundTrip, restoring the unused
// budget before the next pacing wait. EOF and early Close both release the timer.
type transferBody struct {
	io.ReadCloser
	ctx      context.Context
	finish   func()
	once     sync.Once
	finished atomic.Bool
}

func (b *transferBody) release() {
	b.once.Do(func() {
		// Repeated reads after EOF must not report our own cleanup cancellation.
		b.finished.Store(true)
		b.finish()
	})
}

func (b *transferBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if !b.finished.Load() {
		if ctxErr := b.ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
	}
	if err != nil {
		b.release()
	}
	return n, err
}

func (b *transferBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

type startLimiter struct {
	mu       sync.Mutex
	last     time.Time
	interval time.Duration
}

// Waiters recheck the last actual grant after waking, rather than reserving
// future slots; cancelling a waiter does not leave a hole in the schedule.
func (l *startLimiter) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		if err := ctx.Err(); err != nil {
			l.mu.Unlock()
			return err
		}
		now := time.Now()
		delay := l.last.Add(l.interval).Sub(now)
		if l.last.IsZero() || delay <= 0 {
			l.last = now
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// AnnouncementCategories returns the categories supported by the announcement
// adapter. A fresh slice keeps callers from changing the process-wide defaults.
func AnnouncementCategories() []string {
	return []string{"cs.IR", "cs.LG", "cs.AI", "cs.CL", "stat.ML"}
}

func SupportsAnnouncementCategory(category string) bool {
	for _, supported := range AnnouncementCategories() {
		if category == supported {
			return true
		}
	}
	return false
}
