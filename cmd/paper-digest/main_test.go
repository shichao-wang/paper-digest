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

func TestSendTestRequiresExplicitTopicConfirmationAndWebhookWithoutOpeningDatabase(t *testing.T) {
	args := configArgs(t, filepath.Join(t.TempDir(), "missing", "digest.db"), "https://example.com/other", "", false)
	for _, command := range [][]string{{"send-test"}, {"send-test", "--confirm"}, {"send-test", "--topic", "another"}, {"send-test", "--topic", "another", "--confirm", "extra"}} {
		if err := run(append(append([]string{}, args...), command...)); err == nil || !strings.Contains(err.Error(), "--confirm") {
			t.Fatalf("未确认的试发未被拒绝：args=%v err=%v", command, err)
		}
	}
	if err := run(append(append([]string{}, args...), "send-test", "--topic", "missing", "--confirm")); err == nil || !strings.Contains(err.Error(), "未知主题") {
		t.Fatalf("未知主题应拒绝：%v", err)
	}
	if err := run(append(append([]string{}, args...), "send-test", "--topic", "recommendation-advertising-search", "--confirm")); err == nil || !strings.Contains(err.Error(), "Webhook") {
		t.Fatalf("缺少 Webhook 时应拒绝且不打开数据库：%v", err)
	}
}

func TestSendTestUsesSelectedRobotAndFixedMessageWithoutModelOrDatabase(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost {
			t.Errorf("方法：%s", r.Method)
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
	args := configArgs(t, filepath.Join(t.TempDir(), "missing", "digest.db"), server.URL, "", false, "send-test", "--topic", "another", "--confirm")
	oldTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = oldTransport }()
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("预期单次请求，实际 %d 次", calls)
	}
}

func TestSendTestDoesNotAcceptUnconfirmedResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":19001}`))
	}))
	defer server.Close()
	if err := sendTest(context.Background(), server.URL, server.Client()); err == nil {
		t.Fatal("业务失败响应不能视为试发成功")
	}
}

func TestHealthAndPreviewDoNotRequireConfigOrDatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	if err := run([]string{"--config", missing, "health"}); err != nil {
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
