package digest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/shichao-wang/paper-digest/internal/papers"
)

func TestClaudeAnalyzerUsesConfiguredModelAndTraceablePrompt(t *testing.T) {
	published := time.Date(2026, time.September, 26, 10, 0, 0, 0, time.UTC)
	paper := papers.Paper{
		ID:        "arxiv:2609.12345",
		Version:   "v2",
		Title:     "A paper title",
		Authors:   []string{"Alice", "Bob"},
		Published: published,
		Updated:   published.Add(time.Hour),
		Abstract:  "The paper studies a specific problem.",
		URL:       "https://arxiv.org/abs/2609.12345v2",
	}
	called := false
	analyzer := ClaudeAnalyzer{
		Model:  "claude-test-model",
		APIKey: "file-key",
		request: func(_ context.Context, apiKey, model, prompt string) (string, error) {
			if apiKey != "file-key" {
				t.Errorf("未使用配置文件中的密钥")
			}
			called = true
			if model != "claude-test-model" {
				t.Errorf("model = %q, want configured model", model)
			}
			for _, part := range []string{"arxiv:2609.12345v2", paper.Title, paper.Abstract, paper.URL, "只能依据"} {
				if !strings.Contains(prompt, part) {
					t.Errorf("prompt does not contain %q", part)
				}
			}
			return "  摘要正文  ", nil
		},
	}

	got, err := analyzer.Analyze(context.Background(), paper)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if !called {
		t.Fatal("injected request was not called")
	}
	if got.Text != "摘要正文" || got.Model != "claude-test-model" || got.PromptVersion != promptVersion {
		t.Errorf("Analyze() = %#v", got)
	}
}

func TestClaudeAnalyzerDefaultsModelAndRejectsEmptyResult(t *testing.T) {
	analyzer := ClaudeAnalyzer{
		request: func(_ context.Context, _, model, _ string) (string, error) {
			if model != defaultModel {
				t.Errorf("model = %q, want default %q", model, defaultModel)
			}
			return " \n ", nil
		},
	}
	if _, err := analyzer.Analyze(context.Background(), papers.Paper{ID: "2609.12345"}); err == nil || !strings.Contains(err.Error(), "empty summary") {
		t.Fatalf("Analyze() error = %v, want empty-summary error", err)
	}
}

func TestClaudeClientUsesFileKeyInsteadOfEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "environment-key")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Api-Key") != "file-key" {
			t.Errorf("SDK 未使用配置中的密钥")
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"test"}}`))
	}))
	defer server.Close()
	client := newClaudeClient("file-key", option.WithBaseURL(server.URL))
	_, _ = client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model: "claude-test-model", MaxTokens: 1,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("test"))},
	})
	if calls != 1 {
		t.Fatalf("应向本地测试服务发送一次请求，实际 %d 次", calls)
	}
}

func TestClaudeAnalyzerWrapsRequestError(t *testing.T) {
	wantErr := errors.New("temporary API failure")
	analyzer := ClaudeAnalyzer{
		request: func(context.Context, string, string, string) (string, error) {
			return "", wantErr
		},
	}
	_, err := analyzer.Analyze(context.Background(), papers.Paper{ID: "2609.12345"})
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "2609.12345") {
		t.Fatalf("Analyze() error = %v, want wrapped request error", err)
	}
}
