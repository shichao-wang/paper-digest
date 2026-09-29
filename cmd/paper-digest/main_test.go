package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setTopics(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "topics.json")
	data := `{"topics":[{"id":"recommendation-advertising-search","webhook_env":"FEISHU_WEBHOOK_RAS"},{"id":"another","webhook_env":"FEISHU_WEBHOOK_OTHER"}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
}

func TestSendTestRequiresExplicitTopicConfirmationAndWebhookWithoutOpeningDatabase(t *testing.T) {
	setTopics(t)
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "missing", "digest.db"))
	t.Setenv("ANTHROPIC_API_KEY", "")
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	t.Setenv("FEISHU_WEBHOOK_RAS", "")
	t.Setenv("FEISHU_WEBHOOK_OTHER", server.URL)
	for _, args := range [][]string{{"send-test"}, {"send-test", "--confirm"}, {"send-test", "--topic", "another"}, {"send-test", "--topic", "another", "--confirm", "extra"}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "--confirm") {
			t.Fatalf("未确认的试发未被拒绝：args=%v err=%v", args, err)
		}
	}
	if err := run([]string{"send-test", "--topic", "missing", "--confirm"}); err == nil || !strings.Contains(err.Error(), "未知主题") {
		t.Fatalf("未知主题应拒绝：%v", err)
	}
	if err := run([]string{"send-test", "--topic", "recommendation-advertising-search", "--confirm"}); err == nil || !strings.Contains(err.Error(), "FEISHU_WEBHOOK_RAS") {
		t.Fatalf("缺少 Webhook 时应拒绝且不打开数据库：%v", err)
	}
	if calls != 0 {
		t.Fatalf("拒绝路径不应发送请求，实际 %d 次", calls)
	}
}

func TestSendTestUsesSelectedRobotAndFixedMessageWithoutModelOrDatabase(t *testing.T) {
	setTopics(t)
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "missing", "digest.db"))
	t.Setenv("ANTHROPIC_API_KEY", "")
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
	t.Setenv("FEISHU_WEBHOOK_RAS", "")
	t.Setenv("FEISHU_WEBHOOK_OTHER", server.URL)
	oldTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = oldTransport }()
	if err := run([]string{"send-test", "--topic", "another", "--confirm"}); err != nil {
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

func TestDisabledCommandsDoNotRequireTopicConfig(t *testing.T) {
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "digest.db"))
	t.Setenv("ENABLE_DELIVERY", "false")
	if err := run([]string{"health"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"status", "2026-09-29"}); err == nil || !strings.Contains(err.Error(), "job not found") {
		t.Fatalf("空数据库应返回任务不存在，而不是读取主题配置: %v", err)
	}
	if err := run([]string{"preview", "../../testdata/preview.json"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"backup", filepath.Join(t.TempDir(), "backup.db")}); err != nil {
		t.Fatal(err)
	}
}
