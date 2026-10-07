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

func TestFeishuMarkdownUsesWebhookCard(t *testing.T) {
	text := "# 论文日报\n\n基于论文公开摘要整理，非全文解读。\n\n## 1/2. Title\n\n- 原文：[arxiv:2610.01234](https://arxiv.org/abs/2610.01234)\n- 作者：Alice\n\n- 研究问题：测试\n\n- **结果**：有效\n"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Type string `json:"msg_type"`
			Card struct {
				Schema string `json:"schema"`
				Header struct {
					Title struct {
						Content string `json:"content"`
					} `json:"title"`
				} `json:"header"`
				Body struct {
					Elements []json.RawMessage `json:"elements"`
				} `json:"body"`
			} `json:"card"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Type != "interactive" || payload.Card.Schema != "2.0" || payload.Card.Header.Title.Content != "论文日报" || len(payload.Card.Body.Elements) != 4 {
			t.Fatalf("invalid webhook card: %+v", payload)
		}
		raw, _ := json.Marshal(payload)
		for _, want := range []string{"column_set", "https://arxiv.org/abs/2610.01234", "研究问题", "结果", "heading-2"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("card lost %s", want)
			}
		}
		var summary struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(payload.Card.Body.Elements[3], &summary); err != nil {
			t.Fatal(err)
		}
		if summary.Content != "- **研究问题**：测试\n\n- **结果**：有效" {
			t.Errorf("summary formatting lost: %q", summary.Content)
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	if err := (Feishu{WebhookURL: server.URL, Client: server.Client()}).SendMarkdown(context.Background(), text); err != nil {
		t.Fatal(err)
	}
}

func TestFeishuRejectsOversizedPayloadWithoutRequest(t *testing.T) {
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("oversized payload must not reach the robot")
		return nil, nil
	})}
	if err := (Feishu{WebhookURL: "https://example.invalid/secret", Client: client}).SendMarkdown(context.Background(), strings.Repeat("中", 7000)); err == nil {
		t.Fatal("expected size limit")
	}
}
