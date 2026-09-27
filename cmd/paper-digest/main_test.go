package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendTestRequiresConfirmationAndWebhookWithoutOpeningDatabase(t *testing.T) {
	t.Setenv("DB_PATH", t.TempDir()+"/missing/digest.db")
	t.Setenv("FEISHU_WEBHOOK_URL", "")
	for _, args := range [][]string{{"send-test"}, {"send-test", "yes"}, {"send-test", "--confirm", "extra"}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "--confirm") {
			t.Fatalf("未确认的试发未被拒绝：args=%v err=%v", args, err)
		}
	}
	if err := run([]string{"send-test", "--confirm"}); err == nil || !strings.Contains(err.Error(), "FEISHU_WEBHOOK_URL") {
		t.Fatalf("缺少 Webhook 时应明确拒绝且不打开数据库：%v", err)
	}
}

func TestSendTestUsesFixedMessageWithoutModelOrDatabase(t *testing.T) {
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
	if err := sendTest(context.Background(), server.URL, server.Client()); err != nil {
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
