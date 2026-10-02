package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/state"
)

func TestServeLibrarySwitchesAreIndependentOfDelivery(t *testing.T) {
	for _, mode := range []string{"collect", "process", "both"} {
		t.Run(mode, func(t *testing.T) {
			path := sendingDatabase(t)
			cfg := serveConfig(path, false)
			cfg.Library.CollectEnabled = mode != "process"
			cfg.Library.ProcessEnabled = mode != "collect"
			cfg.Topics = nil
			if mode == "collect" {
				cfg.Anthropic.APIKey = ""
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			result := make(chan error, 1)
			go func() {
				result <- serve(ctx, cfg, serveOptions{Listen: "127.0.0.1:0"}, serveDependencies{
					StaticFS: fixtureFS(),
					NewWorker: func(config.Config, *state.Store, string) (worker, error) {
						t.Error("disabled delivery constructed worker")
						return nil, errors.New("unexpected delivery")
					},
					NewLibraryWorker: func(_ config.Config, store *state.Store) (worker, error) {
						return workerFunc(func(ctx context.Context, _ *slog.Logger) error {
							saved, err := store.GetJob(ctx, job.Topic, "2026-09-29")
							if err != nil || saved.Status != "sending" {
								return errors.New("library recovered delivery send")
							}
							close(started)
							<-ctx.Done()
							return ctx.Err()
						}), nil
					},
				})
			}()
			select {
			case <-started:
			case err := <-result:
				t.Fatalf("startup=%v", err)
			case <-time.After(time.Second):
				t.Fatal("library worker did not start")
			}
			cancel()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("library worker did not stop")
			}
		})
	}
}

func TestDisabledLibraryDoesNotConstructWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := serve(ctx, serveConfig(filepath.Join(t.TempDir(), "library.db"), false), serveOptions{Listen: "127.0.0.1:0"}, serveDependencies{
		StaticFS: fixtureFS(),
		NewLibraryWorker: func(config.Config, *state.Store) (worker, error) {
			t.Error("disabled library constructed worker")
			return nil, nil
		},
		Listen: func(network, address string) (net.Listener, error) {
			listener, err := net.Listen(network, address)
			cancel()
			return listener, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLibraryServeStartupValidationRunsBeforeAnyWorkerOrRecovery(t *testing.T) {
	for _, kind := range []string{"library-config", "api-key", "delivery-config", "index", "delivery-factory", "library-factory", "library-nil", "listen"} {
		t.Run(kind, func(t *testing.T) {
			path := sendingDatabase(t)
			cfg := serveConfig(path, true)
			cfg.Library.ProcessEnabled = true
			deps := serveDependencies{
				StaticFS: fixtureFS(),
				NewWorker: func(config.Config, *state.Store, string) (worker, error) {
					if kind == "delivery-factory" {
						return nil, errors.New("delivery factory failed")
					}
					return workerFunc(func(context.Context, *slog.Logger) error { t.Error("failed startup ran delivery"); return nil }), nil
				},
				NewLibraryWorker: func(config.Config, *state.Store) (worker, error) {
					if kind == "library-factory" {
						return nil, errors.New("library factory failed")
					}
					if kind == "library-nil" {
						return nil, nil
					}
					return workerFunc(func(context.Context, *slog.Logger) error { t.Error("failed startup ran library"); return nil }), nil
				},
				Listen: func(string, string) (net.Listener, error) {
					if kind != "listen" {
						t.Error("failed startup reached listener")
					}
					return nil, errors.New("listen failed")
				},
			}
			switch kind {
			case "library-config":
				cfg.Library.Concurrency = 99
			case "api-key":
				cfg.Delivery.Enabled = false
				cfg.Anthropic.APIKey = ""
			case "delivery-config":
				cfg.Topics = nil
			case "index":
				deps.StaticFS = nil
			}
			err := serve(context.Background(), cfg, serveOptions{WebDir: filepath.Join(t.TempDir(), "missing")}, deps)
			if err == nil {
				t.Fatal("startup failure accepted")
			}
			if kind == "api-key" && !strings.Contains(err.Error(), "anthropic.api_key") {
				t.Fatalf("missing key=%v", err)
			}
			store, err := state.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := store.GetJob(context.Background(), job.Topic, "2026-09-29")
			store.Close()
			if err != nil || saved.Status != "sending" {
				t.Fatalf("startup failure recovered send: %+v %v", saved, err)
			}
		})
	}
}

func TestEitherWorkerExitCancelsSiblingAndWaitsBeforeClosingStore(t *testing.T) {
	for _, trigger := range []string{"delivery-error", "library-error", "library-return", "HTTP", "parent"} {
		t.Run(trigger, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "library.db")
			cfg := serveConfig(path, true)
			cfg.Library.ProcessEnabled = true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan string, 2)
			canceled := make(chan string, 2)
			fire, release := make(chan struct{}), make(chan struct{})
			result := make(chan error, 1)
			var shared *state.Store
			expected := errors.New("fixture worker failure")
			factory := func(name string, store *state.Store) (worker, error) {
				shared = store
				return workerFunc(func(ctx context.Context, _ *slog.Logger) error {
					started <- name
					if strings.HasPrefix(trigger, name+"-") {
						<-fire
						if trigger == "library-return" {
							return nil
						}
						return expected
					}
					<-ctx.Done()
					canceled <- name
					<-release
					if _, err := store.ClaimDay(context.WithoutCancel(ctx), name, "2026-09-30"); err != nil {
						return err
					}
					return ctx.Err()
				}), nil
			}
			var listener net.Listener
			go func() {
				result <- serve(ctx, cfg, serveOptions{Listen: "127.0.0.1:0"}, serveDependencies{
					StaticFS:         fixtureFS(),
					NewWorker:        func(_ config.Config, store *state.Store, _ string) (worker, error) { return factory("delivery", store) },
					NewLibraryWorker: func(_ config.Config, store *state.Store) (worker, error) { return factory("library", store) },
					Listen: func(network, address string) (net.Listener, error) {
						var err error
						listener, err = net.Listen(network, address)
						return listener, err
					},
				})
			}()
			for i := 0; i < 2; i++ {
				select {
				case <-started:
				case err := <-result:
					t.Fatalf("startup=%v", err)
				case <-time.After(time.Second):
					t.Fatal("workers did not start")
				}
			}
			switch trigger {
			case "HTTP":
				listener.Close()
			case "parent":
				cancel()
			default:
				close(fire)
			}
			waiting := 1
			if trigger == "HTTP" || trigger == "parent" {
				waiting = 2
			}
			for i := 0; i < waiting; i++ {
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("sibling worker not canceled")
				}
			}
			if err := shared.Health(context.Background()); err != nil {
				t.Fatalf("store closed before workers: %v", err)
			}
			select {
			case err := <-result:
				t.Fatalf("serve returned before join: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			select {
			case err := <-result:
				if strings.HasSuffix(trigger, "-error") && !errors.Is(err, expected) {
					t.Fatalf("worker error lost: %v", err)
				}
				if trigger == "library-return" && (err == nil || !strings.Contains(err.Error(), "library worker stopped unexpectedly")) {
					t.Fatalf("early worker return ignored: %v", err)
				}
				if trigger == "parent" && err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("serve failed to join both workers")
			}
			if err := shared.Health(context.Background()); err == nil {
				t.Fatal("store still open after join")
			}
		})
	}
}

type blockingIndexFS struct {
	fs.FS
	reads   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (f *blockingIndexFS) ReadFile(name string) ([]byte, error) {
	if name == "index.html" && f.reads.Add(1) > 1 {
		close(f.entered)
		<-f.release
	}
	return fs.ReadFile(f.FS, name)
}

func TestServeWaitsForActiveHTTPHandlerBeforeClosingStore(t *testing.T) {
	cfg := serveConfig(filepath.Join(t.TempDir(), "library.db"), false)
	cfg.Library.CollectEnabled = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	static := &blockingIndexFS{FS: fixtureFS(), entered: make(chan struct{}), release: make(chan struct{})}
	address := make(chan string, 1)
	started := make(chan struct{})
	result := make(chan error, 1)
	var shared *state.Store
	go func() {
		result <- serve(ctx, cfg, serveOptions{Listen: "127.0.0.1:0"}, serveDependencies{
			StaticFS: static,
			NewLibraryWorker: func(_ config.Config, store *state.Store) (worker, error) {
				shared = store
				return workerFunc(func(ctx context.Context, _ *slog.Logger) error { close(started); <-ctx.Done(); return ctx.Err() }), nil
			},
			Listen: func(network, listenAddress string) (net.Listener, error) {
				listener, err := net.Listen(network, listenAddress)
				if err == nil {
					address <- listener.Addr().String()
				}
				return listener, err
			},
		})
	}()
	var listenAddress string
	select {
	case listenAddress = <-address:
	case err := <-result:
		t.Fatalf("startup=%v", err)
	case <-time.After(time.Second):
		t.Fatal("listener did not start")
	}
	<-started
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + listenAddress + "/")
		if err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-static.entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not enter blocked read")
	}
	cancel()
	select {
	case err := <-result:
		t.Fatalf("serve returned before handler: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := shared.Health(context.Background()); err != nil {
		t.Fatalf("store closed during active handler: %v", err)
	}
	close(static.release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve did not finish after handler returned")
	}
	<-requestDone
	if err := shared.Health(context.Background()); err == nil {
		t.Fatal("store left open after handlers returned")
	}
}
