package delivery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFeishuSend(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("请求格式不正确: %s", r.Method)
		}
		var payload struct {
			Type    string            `json:"msg_type"`
			Content map[string]string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("解析请求: %v", err)
		}
		if payload.Type != "text" || payload.Content["text"] != "预览" {
			t.Errorf("消息不匹配: %+v", payload)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer server.Close()
	if err := (Feishu{WebhookURL: server.URL, Client: server.Client()}).Send(context.Background(), "预览"); err != nil {
		t.Fatal(err)
	}
}

func TestFeishuRejectsUnsafeURLAndErrors(t *testing.T) {
	if err := (Feishu{WebhookURL: "http://example.com/secret"}).Send(context.Background(), "x"); err == nil {
		t.Fatal("允许了明文 HTTP")
	}
	for _, response := range []string{`{"code":19001,"msg":"failure"}`, `{"msg":"missing code"}`, `not json`} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(response))
		}))
		err := (Feishu{WebhookURL: server.URL, Client: server.Client()}).Send(context.Background(), "x")
		server.Close()
		if err == nil {
			t.Fatalf("未确认成功的响应被当作成功：%s", response)
		}
	}
}
