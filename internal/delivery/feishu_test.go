package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFeishuErrorsNeverExposeWebhook(t *testing.T) {
	for _, webhook := range []string{"http://example.invalid/secret", "https://user:secret@example.invalid", "https://example.invalid/#secret"} {
		if err := (Feishu{WebhookURL: webhook}).Send(context.Background(), "x"); err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), webhook) {
			t.Fatalf("无效URL泄漏: %v", err)
		}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"code":19001,"msg":"secret"}`)) }))
	defer server.Close()
	webhook := server.URL + "/secret"
	for _, client := range []*http.Client{server.Client(), {Transport: transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New(webhook) })}} {
		err := (Feishu{WebhookURL: webhook, Client: client}).Send(context.Background(), "x")
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) {
			t.Fatalf("失败响应或网络错误泄漏: %v", err)
		}
	}
}

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
