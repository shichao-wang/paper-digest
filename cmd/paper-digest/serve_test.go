package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/modelchat"
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
		Concurrency: 1, PollSeconds: 60, MaxRequests: 20, MaxTokens: 500000, TaskTimeoutSeconds: 60,
	}
	return cfg
}

func fixtureFS() fstest.MapFS {
	return fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("fixture")}}
}

func TestDefaultWorkerValidatesChatSettingsWithoutHTTP(t *testing.T) {
	transport := &demoRejectNetwork{}
	original := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, tc := range []struct {
		name, key, model, base string
		valid, migration       bool
	}{
		{"legacy-model", "opaque-private-secret", "claude-opus-5", "", false, true},
		{"legacy-key", "sk-ant-private-secret", "deepseek-flash", "", false, true},
		{"legacy-key-default-model", "sk-ant-private-secret", "", "", false, true},
		{"official-legacy-model", "opaque-private-secret", "claude-opus-5", "https://api.deepseek.com/v1", false, true},
		{"invalid-base", "synthetic-private-secret", "deepseek-flash", "https://user:private-secret@example.invalid", false, false},
		{"missing-key", "", "deepseek-flash", "", false, false},
		{"deepseek-default", "synthetic-private-secret", "", "", true, false},
		{"deepseek-trimmed-default", "synthetic-private-secret", " \n", " \n", true, false},
		{"deepseek-official", "synthetic-private-secret", "deepseek-flash", "https://api.deepseek.com/anthropic", true, false},
		{"explicit-gateway", "opaque-private-secret", "group/custom-model", "http://127.0.0.1:3425/v1", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := serveConfig("", true)
			cfg.Anthropic.APIKey, cfg.Anthropic.Model, cfg.Anthropic.BaseURL = tc.key, tc.model, tc.base
			worker, err := defaultWorker(cfg, nil)
			if (err == nil) != tc.valid || (worker != nil) != tc.valid || errors.Is(err, modelchat.ErrMigrationRequired) != tc.migration || transport.calls != 0 {
				t.Fatalf("startup validation: valid=%v migration=%v err=%v calls=%d", tc.valid, tc.migration, err, transport.calls)
			}
			if err != nil && strings.Contains(err.Error(), "private-secret") {
				t.Fatal("startup validation exposed a credential")
			}
		})
	}
}

func TestEnabledServeRejectsLegacyChatBeforeListenAndRecovery(t *testing.T) {
	path := sendingDatabase(t)
	cfg := serveConfig(path, true)
	cfg.Anthropic.Model = "claude-opus-5"
	cfg.Anthropic.APIKey = "opaque-private-secret"
	transport := &demoRejectNetwork{}
	original := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original })
	listenCalled := false
	err := serve(context.Background(), cfg, serveOptions{}, serveDependencies{
		StaticFS: fixtureFS(),
		Listen: func(string, string) (net.Listener, error) {
			listenCalled = true
			return nil, errors.New("unexpected listen")
		},
	})
	if !errors.Is(err, modelchat.ErrMigrationRequired) || listenCalled || transport.calls != 0 || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("legacy Chat startup accepted: listen=%v HTTP=%d err=%v", listenCalled, transport.calls, err)
	}
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	saved, err := store.GetJob(context.Background(), job.Topic, "2026-09-29")
	if err != nil || saved.Status != "sending" {
		t.Fatalf("invalid Chat startup recovered interrupted send: %+v %v", saved, err)
	}
}

func TestServeInjectsSettingsAuthorizationFromEnvironment(t *testing.T) {
	const token = "synthetic-serve-settings-authorization"
	t.Setenv("PAPER_DIGEST_SETTINGS_TOKEN", token)
	cfg := serveConfig(filepath.Join(t.TempDir(), "settings-auth.db"), false)
	cfg.Topics[0].WebhookURL = ""
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	address := make(chan string, 1)
	result := make(chan error, 1)
	go func() {
		result <- serve(ctx, cfg, serveOptions{Listen: "127.0.0.1:0"}, serveDependencies{
			StaticFS: fixtureFS(),
			Listen: func(network, addr string) (net.Listener, error) {
				listener, err := net.Listen(network, addr)
				if err == nil {
					address <- listener.Addr().String()
				}
				return listener, err
			},
		})
	}()
	var addr string
	select {
	case addr = <-address:
	case err := <-result:
		t.Fatalf("启动失败: %v", err)
	case <-time.After(time.Second):
		t.Fatal("未启动HTTP")
	}
	// 所有请求走真实回环 TCP：启用令牌后，代理呈现的本机来源也不能免授权。
	client := &http.Client{Timeout: time.Second}
	for _, authorization := range []string{"", "Bearer incorrect", "Bearer " + token} {
		req, err := http.NewRequest("PUT", "http://"+addr+"/api/settings/webhook", strings.NewReader(`{"webhookURL":"https://example.invalid/synthetic"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://"+addr)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		want := http.StatusForbidden
		if authorization == "Bearer "+token {
			want = http.StatusOK
		}
		if err != nil || response.StatusCode != want || strings.Contains(string(body), token) || strings.Contains(string(body), "https://example.invalid/synthetic") {
			t.Fatalf("settings authorization=%d %s %v", response.StatusCode, body, err)
		}
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("服务未停止")
	}
	store, err := state.Open(cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got, err := store.Webhook(context.Background(), job.Topic); err != nil || got != "https://example.invalid/synthetic" {
		t.Fatal("授权写入未保存")
	}
}

func TestServeSettingsMigratedBeforeStartupAndAllowMissingWebhook(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			cfg := serveConfig(filepath.Join(t.TempDir(), "settings.db"), true)
			robot := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("启动不应发送") }))
			defer robot.Close()
			cfg.Topics[0].WebhookURL = ""
			if configured {
				cfg.Topics[0].WebhookURL = "  " + robot.URL + "/secret  "
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			address := make(chan string, 1)
			result := make(chan error, 1)
			go func() {
				result <- serve(ctx, cfg, serveOptions{Listen: "127.0.0.1:0"}, serveDependencies{
					StaticFS: fixtureFS(),
					NewWorker: func(_ config.Config, store *state.Store) (worker, error) {
						webhook, err := store.Webhook(ctx, job.Topic)
						if err != nil || (webhook != "") != configured {
							return nil, errors.New("迁移未在worker初始化之前完成")
						}
						return workerFunc(func(ctx context.Context, _ *slog.Logger) error { <-ctx.Done(); return ctx.Err() }), nil
					},
					Listen: func(network, addr string) (net.Listener, error) {
						listener, err := net.Listen(network, addr)
						if err == nil {
							address <- listener.Addr().String()
						}
						return listener, err
					},
				})
			}()
			var addr string
			select {
			case addr = <-address:
			case err := <-result:
				t.Fatalf("启动失败: %v", err)
			case <-time.After(time.Second):
				t.Fatal("未启动HTTP")
			}
			client := &http.Client{Timeout: time.Second}
			response, err := client.Get("http://" + addr + "/api/settings/webhook")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != 200 || !strings.Contains(string(body), `"configured":`+fmt.Sprint(configured)) || !strings.Contains(string(body), `"deliveryEnabled":true`) || strings.Contains(string(body), "secret") {
				t.Fatalf("settings=%d %s %v", response.StatusCode, body, err)
			}
			cancel()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("服务未停止")
			}
		})
	}
}

func TestServePassesConfiguredTopicsAndReportsOnlyRASWorker(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			cfg := serveConfig(filepath.Join(t.TempDir(), "topics.db"), enabled)
			cfg.Topics = append([]config.Topic{{ID: "another", WebhookURL: "https://example.invalid/another-secret"}}, cfg.Topics...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			address := make(chan string, 1)
			result := make(chan error, 1)
			go func() {
				result <- serve(ctx, cfg, serveOptions{Listen: "127.0.0.1:0"}, serveDependencies{
					StaticFS: fixtureFS(),
					NewWorker: func(config.Config, *state.Store) (worker, error) {
						if !enabled {
							return nil, errors.New("disabled delivery constructed worker")
						}
						return workerFunc(func(ctx context.Context, _ *slog.Logger) error { <-ctx.Done(); return ctx.Err() }), nil
					},
					Listen: func(network, addr string) (net.Listener, error) {
						listener, err := net.Listen(network, addr)
						if err == nil {
							address <- listener.Addr().String()
						}
						return listener, err
					},
				})
			}()
			var addr string
			select {
			case addr = <-address:
			case err := <-result:
				t.Fatalf("serve startup: %v", err)
			case <-time.After(time.Second):
				t.Fatal("HTTP not initialized")
			}
			client := &http.Client{Timeout: time.Second}
			response, err := client.Get("http://" + addr + "/api/topics")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			var directory struct {
				Items []struct {
					ID              string `json:"id"`
					Name            string `json:"name"`
					DeliveryEnabled bool   `json:"deliveryEnabled"`
				} `json:"items"`
			}
			if err != nil || json.Unmarshal(body, &directory) != nil || response.StatusCode != 200 || len(directory.Items) != 2 || strings.Contains(string(body), "https://") || strings.Contains(string(body), "secret") {
				t.Fatalf("topics=%d %s err=%v", response.StatusCode, body, err)
			}
			if directory.Items[0].ID != "another" || directory.Items[0].Name != "another" || directory.Items[0].DeliveryEnabled || directory.Items[1].ID != job.Topic || directory.Items[1].Name != "推荐 / 广告 / 搜索" || directory.Items[1].DeliveryEnabled != enabled {
				t.Fatalf("wrong topics or automatic delivery status: %s", body)
			}
			response, err = client.Get("http://" + addr + "/api/settings/webhook?topic=another")
			if err != nil {
				t.Fatal(err)
			}
			body, err = io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != 200 || string(body) != "{\"topic\":\"another\",\"configured\":true,\"deliveryEnabled\":false}\n" {
				t.Fatalf("another settings=%d %s err=%v", response.StatusCode, body, err)
			}
			cancel()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("serve did not stop")
			}
		})
	}
}

func TestEnabledServeRejectsUnconfiguredWorkerTopicBeforeSideEffects(t *testing.T) {
	for _, topic := range []string{"another", "recommendation-advertising-seach"} {
		t.Run(topic, func(t *testing.T) {
			path := sendingDatabase(t)
			cfg := serveConfig(path, true)
			cfg.Topics = []config.Topic{{ID: topic, WebhookURL: "https://example.invalid/private-secret"}}
			factoryCalled, listenCalled := false, false
			err := serve(context.Background(), cfg, serveOptions{}, serveDependencies{
				StaticFS: fixtureFS(),
				NewWorker: func(config.Config, *state.Store) (worker, error) {
					factoryCalled = true
					return workerFunc(func(context.Context, *slog.Logger) error {
						t.Error("未登记主题不得启动付费 worker")
						return nil
					}), nil
				},
				Listen: func(string, string) (net.Listener, error) {
					listenCalled = true
					return nil, errors.New("不应绑定端口")
				},
			})
			if err == nil || !strings.Contains(err.Error(), "未知主题") || strings.Contains(err.Error(), "private-secret") || factoryCalled || listenCalled {
				t.Fatalf("未在创建 worker 前拒绝主题: factory=%v listen=%v err=%v", factoryCalled, listenCalled, err)
			}
			store, err := state.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			saved, err := store.GetJob(context.Background(), job.Topic, "2026-09-29")
			if err != nil || saved.Status != "sending" {
				t.Fatalf("校验失败不得恢复发送状态: %+v %v", saved, err)
			}
			webhook, err := store.Webhook(context.Background(), topic)
			if err != nil || webhook != "" {
				t.Fatal("校验失败不得迁移地址")
			}
		})
	}
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
			NewWorker: func(config.Config, *state.Store) (worker, error) {
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
			StaticFS: fixtureFS(), NewWorker: func(_ config.Config, store *state.Store) (worker, error) {
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
			deps := serveDependencies{StaticFS: fixtureFS(), NewWorker: func(config.Config, *state.Store) (worker, error) {
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
		NewWorker: func(_ config.Config, store *state.Store) (worker, error) {
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
		StaticFS: fixtureFS(), NewWorker: func(_ config.Config, store *state.Store) (worker, error) {
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

func TestStatusAndBackupDoNotMigrateWebhookSettings(t *testing.T) {
	path := sendingDatabase(t)
	robot := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("查询不得发送请求") }))
	defer robot.Close()
	args := configArgs(t, path, robot.URL+"/secret", "", false)
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
		webhook, err := store.Webhook(context.Background(), "another")
		store.Close()
		if err != nil || webhook != "" {
			t.Fatal("查询命令不应迁移JSON地址")
		}
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
