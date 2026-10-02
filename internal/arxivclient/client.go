// Package arxivclient shares public arXiv request pacing across all collectors.
package arxivclient

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

const RequestInterval = 3 * time.Second

var publicRequests = startLimiter{interval: RequestInterval}

// Client copies the client and paces every public arXiv request, including
// redirects. Injected loopback servers and unrelated services are not delayed.
func Client(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	transport := copy.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if _, wrapped := transport.(*pacedTransport); !wrapped {
		copy.Transport = &pacedTransport{base: transport, limiter: &publicRequests}
	}
	return &copy
}

type pacedTransport struct {
	base    http.RoundTripper
	limiter *startLimiter
}

func (t *pacedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(strings.TrimSuffix(req.URL.Hostname(), "."))
	if host == "arxiv.org" || strings.HasSuffix(host, ".arxiv.org") {
		if err := t.limiter.wait(req.Context()); err != nil {
			return nil, err
		}
	}
	return t.base.RoundTrip(req)
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
