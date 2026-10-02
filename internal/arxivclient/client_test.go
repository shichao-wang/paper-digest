package arxivclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func testResponse(req *http.Request) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: req}
}

func TestPublicRequestsShareThreeSecondStartInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var starts []time.Time
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()
			return testResponse(req), nil
		})
		var wg sync.WaitGroup
		for _, endpoint := range []string{"https://rss.arxiv.org/atom/cs.IR", "https://arxiv.org/list/cs.IR/new", "https://export.arxiv.org/api/query", "https://static.arxiv.org/pdf/2610.00001v1"} {
			wg.Go(func() {
				// Separate clients and wrappers must still use one process limiter.
				client := Client(&http.Client{Transport: base})
				resp, err := client.Get(endpoint)
				if err != nil {
					t.Error(err)
					return
				}
				resp.Body.Close()
			})
		}
		wg.Wait()
		if len(starts) != 4 {
			t.Fatalf("got %d request starts", len(starts))
		}
		slices.SortFunc(starts, func(a, b time.Time) int { return a.Compare(b) })
		for n := 1; n < len(starts); n++ {
			if interval := starts[n].Sub(starts[n-1]); interval < 3*time.Second {
				t.Fatalf("request starts only %s apart", interval)
			}
		}
	})
}

func TestLimiterCancellationDoesNotReserveFutureSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := &startLimiter{interval: RequestInterval}
		if err := limiter.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- limiter.wait(ctx) }()
		synctest.Wait()
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled wait: %v", err)
		}
		if !time.Now().Equal(start) {
			t.Fatal("cancellation waited for rate limit timer")
		}
		if err := limiter.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != 3*time.Second {
			t.Fatalf("cancelled waiter consumed a slot: %s", elapsed)
		}
		deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), time.Second)
		defer deadlineCancel()
		if err := limiter.wait(deadlineCtx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline wait: %v", err)
		}
	})
}

func TestLoopbackAndOtherHostsBypassPublicLimiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := &startLimiter{interval: RequestInterval, last: time.Now()}
		calls := 0
		client := &http.Client{Transport: &pacedTransport{limiter: limiter, base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return testResponse(req), nil
		})}}
		start := time.Now()
		for _, endpoint := range []string{"http://127.0.0.1:1234/atom", "http://localhost:1234/query", "http://[::1]:1234/pdf", "https://arxiv.org.example.test/query"} {
			resp, err := client.Get(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		}
		if calls != 4 || !time.Now().Equal(start) || !limiter.last.Equal(start) {
			t.Fatal("non-public requests entered the public limiter")
		}
	})
}

func TestRedirectRequestsAreAlsoPaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var starts []time.Time
		limiter := &startLimiter{interval: RequestInterval}
		client := &http.Client{Transport: &pacedTransport{limiter: limiter, base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			starts = append(starts, time.Now())
			resp := testResponse(req)
			if len(starts) == 1 {
				resp.StatusCode = http.StatusFound
				resp.Header.Set("Location", "https://static.arxiv.org/pdf/2610.00001v1.pdf")
			}
			return resp, nil
		})}}
		resp, err := client.Get("https://arxiv.org/pdf/2610.00001v1")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if len(starts) != 2 || starts[1].Sub(starts[0]) != 3*time.Second {
			t.Fatalf("redirect starts: %v", starts)
		}
	})
}

func TestAnnouncementCategoryDefaultsAreIndependent(t *testing.T) {
	categories := AnnouncementCategories()
	for _, category := range categories {
		if !SupportsAnnouncementCategory(category) {
			t.Fatalf("default category %s unsupported", category)
		}
	}
	categories[0] = "cs.CV"
	if SupportsAnnouncementCategory("cs.CV") || AnnouncementCategories()[0] != "cs.IR" {
		t.Fatal("caller mutated supported categories")
	}
}
