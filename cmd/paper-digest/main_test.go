package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/state"
)

func configArgs(t *testing.T, database, webhook, apiKey string, enabled bool, args ...string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	data := fmt.Sprintf(`{"database":{"path":%q},"delivery":{"enabled":%t},"anthropic":{"api_key":%q,"model":"claude-opus-5"},"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search","webhook_url":""},{"id":"another","webhook_url":%q}]}`, database, enabled, apiKey, webhook)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return append([]string{"--config", path}, args...)
}

func TestSendTestRequiresExplicitTopicConfirmationAndDatabaseWebhook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "digest.db")
	args := configArgs(t, path, "", "", false)
	for _, command := range [][]string{{"send-test"}, {"send-test", "--confirm"}, {"send-test", "--topic", job.Topic}, {"send-test", "--topic", job.Topic, "--confirm", "extra"}} {
		if err := run(append(append([]string{}, args...), command...)); err == nil || !strings.Contains(err.Error(), "--confirm") {
			t.Fatalf("未确认的试发未被拒绝：args=%v err=%v", command, err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("未确认不得打开数据库")
	}
	if err := run(append(append([]string{}, args...), "send-test", "--topic", "missing", "--confirm")); err == nil || !strings.Contains(err.Error(), "未知主题") {
		t.Fatalf("未知主题应拒绝：%v", err)
	}
	if err := run(append(append([]string{}, args...), "send-test", "--topic", job.Topic, "--confirm")); err == nil || !strings.Contains(err.Error(), "Webhook") {
		t.Fatalf("缺少 Webhook 时应拒绝: %v", err)
	}
}

func TestSendTestUsesDatabaseRobotAndFixedMessageWithoutModel(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/database-secret" {
			t.Errorf("请求没有使用数据库地址或方法错误")
		}
		var payload struct {
			Type    string            `json:"msg_type"`
			Content map[string]string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("解析试发请求：%v", err)
		}
		if payload.Type != "text" || payload.Content["text"] != testMessage || !strings.Contains(testMessage, "不是正式论文日报") {
			t.Errorf("试发消息不正确：%+v", payload)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer server.Close()
	database := filepath.Join(t.TempDir(), "digest.db")
	store, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetWebhook(context.Background(), job.Topic, server.URL+"/database-secret"); err != nil {
		t.Fatal(err)
	}
	store.Close()
	args := configArgs(t, database, "", "", true, "send-test", "--topic", job.Topic, "--confirm")
	oldTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = oldTransport }()
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("预期单次请求，实际 %d 次", calls)
	}
	store, err = state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetWebhook(context.Background(), "another", server.URL+"/database-secret"); err != nil {
		t.Fatal(err)
	}
	store.Close()
	args[len(args)-2] = "another"
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("其他已登记主题未使用数据库设置: calls=%d", calls)
	}
}

func TestSendTestMigratesLegacyURLButNeverRevivesClearedSetting(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	database := filepath.Join(t.TempDir(), "settings.db")
	configPath := filepath.Join(t.TempDir(), "legacy.json")
	text := fmt.Sprintf(`{"database":{"path":%q},"arxiv":{"lookback_days":7},"topics":[{"id":%q,"webhook_url":%q}]}`, database, job.Topic, "  "+server.URL+"/legacy-secret  ")
	if err := os.WriteFile(configPath, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	oldTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = oldTransport }()
	args := []string{"--config", configPath, "send-test", "--topic", job.Topic, "--confirm"}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetWebhook(context.Background(), job.Topic, ""); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if err := run(args); err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("清除设置应拒绝试发且不泄漏: %v", err)
	}
	if calls != 1 {
		t.Fatalf("清除后复活了JSON地址: calls=%d", calls)
	}
}

func TestHealthRequiresSuccessfulHTTPAndValidPayload(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"healthy", `{"status":"ok"}`, 200, true},
		{"unavailable", `{"error":"service unavailable"}`, 503, false},
		{"html", `<html>wrong service</html>`, 200, false},
		{"wrong status", `{"status":"down"}`, 200, false},
		{"trailing data", `{"status":"ok"}{}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("method=%s", r.Method)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			err := run([]string{"--config", filepath.Join(t.TempDir(), "missing.json"), "health", "--url", server.URL})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()
	if err := run([]string{"health", "--url", server.URL}); err == nil {
		t.Fatal("unreachable HTTP service accepted")
	}
	for _, args := range [][]string{{"health", "--url", "file:///tmp/health"}, {"health", "extra"}, {"serve", "--listen", ""}, {"serve", "--unknown"}} {
		if err := run(args); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
}

func TestSendTestDoesNotAcceptUnconfirmedResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":19001}`))
	}))
	defer server.Close()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetWebhook(context.Background(), job.Topic, server.URL); err != nil {
		t.Fatal(err)
	}
	if err := sendTest(context.Background(), store, job.Topic, server.Client()); err == nil {
		t.Fatal("业务失败响应不能视为试发成功")
	}
}

func TestHealthAndPreviewDoNotRequireConfigOrDatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	if err := run([]string{"--config", missing, "health", "--url", server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--config", missing, "preview", "../../testdata/preview.json"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--config", missing, "serve"}); err == nil || !strings.Contains(err.Error(), "读取配置文件失败") {
		t.Fatalf("缺失配置不能启动: %v", err)
	}
}

func TestDatabaseCommandsUseConfigWithoutDeliveryCredentials(t *testing.T) {
	database := filepath.Join(t.TempDir(), "digest.db")
	args := configArgs(t, database, "", "", false)
	if err := run(append(append([]string{}, args...), "status", "2026-09-29")); err == nil || !strings.Contains(err.Error(), "job not found") {
		t.Fatalf("空数据库应返回任务不存在: %v", err)
	}
	if err := run(append(append([]string{}, args...), "backup", filepath.Join(t.TempDir(), "backup.db"))); err != nil {
		t.Fatal(err)
	}
}

func TestEnabledServeDoesNotFallBackToEnvironmentCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "environment-secret")
	t.Setenv("ENABLE_DELIVERY", "true")
	t.Setenv("FEISHU_WEBHOOK_RAS", "https://example.com/environment-secret")
	args := configArgs(t, filepath.Join(t.TempDir(), "missing", "digest.db"), "", "", true, "serve")
	if err := run(args); err == nil || !strings.Contains(err.Error(), "anthropic.api_key") || strings.Contains(err.Error(), "environment-secret") {
		t.Fatalf("不能从旧环境变量补齐配置: %v", err)
	}
}
