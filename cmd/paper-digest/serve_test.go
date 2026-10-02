package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/state"
)

type workerFunc func(context.Context, *slog.Logger) error

func (f workerFunc) Serve(ctx context.Context, logger *slog.Logger) error { return f(ctx, logger) }

func sendingDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.db")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.ClaimDay(ctx, job.Topic, "2026-09-29"); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(ctx, job.Topic, "2026-09-29", "saved intent"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimSend(ctx, job.Topic, "2026-09-29"); err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	return path
}

func serveConfig(path string, enabled bool) config.Config {
	var cfg config.Config
	cfg.Database.Path = path
	cfg.Delivery.Enabled = enabled
	cfg.Arxiv.LookbackDays = 7
	cfg.Anthropic.APIKey = "fixture-key"
	cfg.Topics = []config.Topic{{ID: job.Topic, WebhookURL: "https://example.invalid/fixture"}}
	cfg.Library = config.Library{
		Categories: []string{"cs.IR"}, DocumentDir: filepath.Join(filepath.Dir(path), "documents"),
		Concurrency: 1, PollSeconds: 60, MaxRequests: 20, MaxTokens: 4096, TaskTimeoutSeconds: 60,
	}
	return cfg
}

func fixtureFS() fstest.MapFS {
	return fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("fixture")}}
}

func TestDisabledServeUsesHTTPWithoutWorkerOrRecovery(t *testing.T) {
	path := sendingDatabase(t)
	cfg := serveConfig(path, false)
	cfg.Anthropic.APIKey = ""
	cfg.Topics[0].WebhookURL = ""
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	result := make(chan error, 1)
	go func() {
		result <- serve(ctx, cfg, serveOptions{Listen: "127.0.0.1:0"}, serveDependencies{
			StaticFS: fixtureFS(),
			NewWorker: func(config.Config, *state.Store, string) (worker, error) {
				return nil, errors.New("disabled unexpectedly constructed worker")
			},
			Listen: func(network, address string) (net.Listener, error) {
				listener, err := net.Listen(network, address)
				if err == nil {
					ready <- listener.Addr().String()
				}
				return listener, err
			},
		})
	}()
	var address string
	select {
	case address = <-ready:
	case err := <-result:
		t.Fatalf("serve failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("HTTP not initialized")
	}
	client := &http.Client{Timeout: time.Second}
	// Serve 前已通知绑定完成；net.Listener 将请求排队等待 handler 启动。
	response, err := client.Get("http://" + address + "/api/digests/2026-09-29")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(body), `"status":"sending"`) {
		t.Fatalf("HTTP read=%d %s %v", response.StatusCode, body, err)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve did not close")
	}
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	saved, err := store.GetJob(context.Background(), job.Topic, "2026-09-29")
	if err != nil || saved.Status != "sending" {
		t.Fatalf("disabled changed intent: %+v %v", saved, err)
	}
}

func TestEnabledServeRecoversOnceAndKeepsStoreOpenUntilWorkerFinishes(t *testing.T) {
	path := sendingDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	var shared *state.Store
	go func() {
		result <- serve(ctx, serveConfig(path, true), serveOptions{Listen: "127.0.0.1:0"}, serveDependencies{
			StaticFS: fixtureFS(), NewWorker: func(_ config.Config, store *state.Store, _ string) (worker, error) {
				shared = store
				return workerFunc(func(ctx context.Context, _ *slog.Logger) error {
					saved, err := store.GetJob(ctx, job.Topic, "2026-09-29")
					if err != nil || saved.Status != "unknown" {
						return errors.New("worker did not recover interrupted intent")
					}
					close(entered)
					<-ctx.Done()
					close(canceled)
					<-release
					writeCtx := context.WithoutCancel(ctx)
					if _, err := store.ClaimDay(writeCtx, job.Topic, "2026-09-30"); err != nil {
						return err
					}
					return ctx.Err()
				}), nil
			},
		})
	}()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("worker startup: %v", err)
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("worker not canceled")
	}
	select {
	case err := <-result:
		t.Fatalf("serve returned before worker: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve failed to join worker")
	}
	if err := shared.Health(context.Background()); err == nil {
		t.Fatal("Store was not closed after worker returned")
	}
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.GetJob(context.Background(), job.Topic, "2026-09-30"); err != nil {
		t.Fatalf("final worker write failed: %v", err)
	}
}

func TestServeStartupFailuresDoNotRecoverOrStartWorker(t *testing.T) {
	for _, kind := range []string{"index", "factory", "listen"} {
		t.Run(kind, func(t *testing.T) {
			path := sendingDatabase(t)
			deps := serveDependencies{StaticFS: fixtureFS(), NewWorker: func(config.Config, *state.Store, string) (worker, error) {
				if kind == "factory" {
					return nil, errors.New("factory failure")
				}
				return workerFunc(func(context.Context, *slog.Logger) error { t.Error("failed startup ran worker"); return nil }), nil
			}}
			if kind == "index" {
				deps.StaticFS = fstest.MapFS{}
			}
			deps.Listen = func(string, string) (net.Listener, error) { return nil, errors.New("listen failure") }
			if err := serve(context.Background(), serveConfig(path, true), serveOptions{}, deps); err == nil {
				t.Fatal("startup failure ignored")
			}
			store, err := state.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			saved, err := store.GetJob(context.Background(), job.Topic, "2026-09-29")
			if err != nil || saved.Status != "sending" {
				t.Fatalf("startup failure changed state: %+v %v", saved, err)
			}
		})
	}
}

type failingListener struct{ net.Listener }

func (f failingListener) Accept() (net.Conn, error) { return nil, errors.New("HTTP accept failure") }

func TestHTTPFailureCancelsWorkerAndJoinsFinalWrite(t *testing.T) {
	var shared *state.Store
	path := filepath.Join(t.TempDir(), "fixture.db")
	err := serve(context.Background(), serveConfig(path, true), serveOptions{Listen: "127.0.0.1:0"}, serveDependencies{
		StaticFS: fixtureFS(),
		Listen: func(network, address string) (net.Listener, error) {
			listener, err := net.Listen(network, address)
			return failingListener{listener}, err
		},
		NewWorker: func(_ config.Config, store *state.Store, _ string) (worker, error) {
			shared = store
			return workerFunc(func(ctx context.Context, _ *slog.Logger) error {
				<-ctx.Done()
				_, err := store.ClaimDay(context.WithoutCancel(ctx), job.Topic, "2026-09-30")
				if err != nil {
					return err
				}
				return ctx.Err()
			}), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP accept failure") {
		t.Fatalf("HTTP failure not surfaced: %v", err)
	}
	if err := shared.Health(context.Background()); err == nil {
		t.Fatal("Store left open after HTTP failure")
	}
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.GetJob(context.Background(), job.Topic, "2026-09-30"); err != nil {
		t.Fatalf("Store closed before worker final write: %v", err)
	}
}

func TestWorkerFailureStopsHTTPAndClosesStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	var shared *state.Store
	expected := errors.New("worker failure")
	err := serve(context.Background(), serveConfig(path, true), serveOptions{Listen: "127.0.0.1:0"}, serveDependencies{
		StaticFS: fixtureFS(), NewWorker: func(_ config.Config, store *state.Store, _ string) (worker, error) {
			shared = store
			return workerFunc(func(context.Context, *slog.Logger) error { return expected }), nil
		},
	})
	if !errors.Is(err, expected) {
		t.Fatalf("worker error=%v", err)
	}
	if err := shared.Health(context.Background()); err == nil {
		t.Fatal("worker failure left store open")
	}
}

func TestStatusAndBackupDoNotRecoverSending(t *testing.T) {
	path := sendingDatabase(t)
	args := configArgs(t, path, "", "", false)
	if err := run(append(append([]string{}, args...), "status", "2026-09-29")); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := run(append(append([]string{}, args...), "backup", backup)); err != nil {
		t.Fatal(err)
	}
	for _, database := range []string{path, backup} {
		store, err := state.Open(database)
		if err != nil {
			t.Fatal(err)
		}
		saved, err := store.GetJob(context.Background(), job.Topic, "2026-09-29")
		store.Close()
		if err != nil || saved.Status != "sending" {
			t.Fatalf("database=%s saved=%+v err=%v", database, saved, err)
		}
	}
}
