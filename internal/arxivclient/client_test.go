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

func testGet(client *http.Client, endpoint string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	return Do(client, req)
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
				client := &http.Client{Transport: base}
				resp, err := testGet(client, endpoint)
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

func TestDoCallerContextBoundsQueue(t *testing.T) {
	for _, cause := range []string{"deadline", "cancellation"} {
		t.Run(cause, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client := &http.Client{Timeout: 100 * time.Millisecond, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					return testResponse(req), nil
				})}
				resp, err := testGet(client, "https://rss.arxiv.org/atom/cs.IR")
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := context.Canceled
				if cause == "deadline" {
					var deadlineCancel context.CancelFunc
					ctx, deadlineCancel = context.WithTimeout(ctx, time.Second)
					defer deadlineCancel()
					want = context.DeadlineExceeded
				} else {
					go func() { time.Sleep(time.Second); cancel() }()
				}
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://export.arxiv.org/api/query", nil)
				start := time.Now()
				_, err = Do(client, req)
				if !errors.Is(err, want) || time.Since(start) != time.Second || calls != 1 {
					t.Fatalf("queue context: err=%v elapsed=%s calls=%d", err, time.Since(start), calls)
				}
			})
		})
	}
}

func TestTransferTimeoutExcludesPublicQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Simulate a queue that cannot grant a slot until after 36 seconds.
		limiter := &startLimiter{interval: RequestInterval, last: time.Now().Add(33 * time.Second)}
		transport := &pacedTransport{limiter: limiter, timed: true}
		transport.remaining.Store(int64(30 * time.Second))
		transport.base = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			deadline, ok := req.Context().Deadline()
			if !ok || time.Until(deadline) != 30*time.Second {
				t.Fatalf("transfer started with timeout %s, has deadline %v", time.Until(deadline), ok)
			}
			time.Sleep(29 * time.Second)
			return testResponse(req), nil
		})
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://export.arxiv.org/api/query", nil)
		start := time.Now()
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || string(body) != "ok" || time.Since(start) != 65*time.Second {
			t.Fatalf("queued transfer: body=%q err=%v elapsed=%s", body, err, time.Since(start))
		}
		if transport.remaining.Load() != int64(time.Second) {
			t.Fatalf("queue consumed transfer budget: %s", time.Duration(transport.remaining.Load()))
		}
	})
}

type contextBody struct {
	ctx    context.Context
	prefix string
	closed bool
}

func (b *contextBody) Read(p []byte) (int, error) {
	if b.prefix != "" {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		return n, nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *contextBody) Close() error { b.closed = true; return nil }

func TestTransferTimeoutAndCallerCancellation(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		for _, cause := range []string{"transfer timeout", "caller deadline", "caller cancellation"} {
			t.Run(phase+"/"+cause, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					want, elapsed := context.DeadlineExceeded, 30*time.Second
					switch cause {
					case "caller deadline":
						var deadlineCancel context.CancelFunc
						ctx, deadlineCancel = context.WithTimeout(ctx, time.Second)
						defer deadlineCancel()
						elapsed = time.Second
					case "caller cancellation":
						want, elapsed = context.Canceled, time.Second
						go func() { time.Sleep(time.Second); cancel() }()
					}
					var body *contextBody
					client := &http.Client{Timeout: 30 * time.Second, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						if phase == "headers" {
							<-req.Context().Done()
							return nil, req.Context().Err()
						}
						body = &contextBody{ctx: req.Context(), prefix: "partial"}
						resp := testResponse(req)
						resp.Body = body
						return resp, nil
					})}
					// Loopback exercises timeout behavior without any limiter wait.
					req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/query", nil)
					start := time.Now()
					resp, err := Do(client, req)
					if phase == "body" {
						if err != nil {
							t.Fatal(err)
						}
						data, readErr := io.ReadAll(resp.Body)
						err = readErr
						resp.Body.Close()
						if string(data) != "partial" || !body.closed {
							t.Fatalf("partial body/close: %q %v", data, body.closed)
						}
					}
					if !errors.Is(err, want) || time.Since(start) != elapsed {
						t.Fatalf("err=%v elapsed=%s, want %v/%s", err, time.Since(start), want, elapsed)
					}
					if client.Timeout != 30*time.Second {
						t.Fatal("caller client was changed")
					}
				})
			})
		}
	}
}

func TestRedirectsShareTransferBudgetWithoutPacingCharge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var starts []time.Time
		var firstContext context.Context
		client := &http.Client{Timeout: 2 * time.Second, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			starts = append(starts, time.Now())
			deadline, ok := req.Context().Deadline()
			remaining := 2*time.Second - time.Duration(len(starts)-1)*time.Second
			if !ok || time.Until(deadline) != remaining {
				t.Fatalf("hop %d budget=%s want %s", len(starts), time.Until(deadline), remaining)
			}
			if len(starts) == 2 {
				if !errors.Is(firstContext.Err(), context.Canceled) {
					t.Fatalf("redirect body left first timer alive: %v", firstContext.Err())
				}
				<-req.Context().Done()
				return nil, req.Context().Err()
			}
			firstContext = req.Context()
			time.Sleep(time.Second)
			resp := testResponse(req)
			resp.StatusCode = http.StatusFound
			resp.Header.Set("Location", "https://static.arxiv.org/pdf/2610.00001v1.pdf")
			// EOF must release this hop's context before the redirect queues.
			return resp, nil
		})}
		resp, err := testGet(client, "https://arxiv.org/pdf/2610.00001v1")
		if resp != nil {
			resp.Body.Close()
		}
		if !errors.Is(err, context.DeadlineExceeded) || len(starts) != 2 || starts[1].Sub(starts[0]) != RequestInterval || time.Since(starts[1]) != time.Second {
			t.Fatalf("redirect timeout=%v starts=%v", err, starts)
		}
	})
}

func TestTransferBodyEOFAndCloseReleaseContext(t *testing.T) {
	for _, action := range []string{"EOF", "Close"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var transferContext context.Context
				client := &http.Client{Timeout: 30 * time.Second, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					transferContext = req.Context()
					return testResponse(req), nil
				})}
				resp, err := testGet(client, "http://localhost/query")
				if err != nil {
					t.Fatal(err)
				}
				if action == "EOF" {
					if _, err := io.ReadAll(resp.Body); err != nil {
						t.Fatal(err)
					}
					if _, err := resp.Body.Read(make([]byte, 1)); err != io.EOF {
						t.Fatalf("repeated EOF became %v", err)
					}
				} else {
					resp.Body.Close()
				}
				if !errors.Is(transferContext.Err(), context.Canceled) {
					t.Fatalf("%s left transfer timer alive: %v", action, transferContext.Err())
				}
				resp.Body.Close()
			})
		})
	}
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
