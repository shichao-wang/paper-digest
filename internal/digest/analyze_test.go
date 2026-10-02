package digest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/modelchat"
	"github.com/shichao-wang/paper-digest/internal/papers"
)

func TestDeepSeekAnalyzerUsesConfiguredModelAndTraceablePrompt(t *testing.T) {
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
		Model:   "group/deepseek-v4-1-flash",
		APIKey:  "file-key",
		BaseURL: "http://127.0.0.1:3425",
		request: func(_ context.Context, apiKey, baseURL, model, prompt string) (string, error) {
			if apiKey != "file-key" {
				t.Errorf("未使用配置文件中的密钥")
			}
			if baseURL != "http://127.0.0.1:3425" {
				t.Errorf("未使用配置中的网关地址")
			}
			called = true
			if model != "group/deepseek-v4-1-flash" {
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
	if got.Text != "摘要正文" || got.Model != "group/deepseek-v4-1-flash" || got.PromptVersion != promptVersion {
		t.Errorf("Analyze() = %#v", got)
	}
}

func TestClaudeAnalyzerDefaultsModelAndRejectsEmptyResult(t *testing.T) {
	analyzer := ClaudeAnalyzer{
		request: func(_ context.Context, _, _, model, _ string) (string, error) {
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

type digestRoutingTransport func(*http.Request) (*http.Response, error)

func (f digestRoutingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLegacyDigestSettingsRejectedBeforeHTTP(t *testing.T) {
	// 截获默认 transport，防止回归测试意外访问真实服务或发送凭据。
	original := http.DefaultTransport
	calls := 0
	http.DefaultTransport = digestRoutingTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected HTTP request")
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, test := range []struct {
		name, key, model, base string
	}{
		{"old_sample", "sk-ant-synthetic-only", "claude-opus-5", ""},
		{"opaque_legacy_key", "opaque-synthetic-only", "claude-opus-5", " \n"},
		{"model_changed_without_key", "sk-ant-synthetic-only", "deepseek-flash", ""},
		{"model_defaulted_without_key", "sk-ant-synthetic-only", "", ""},
		{"explicit_official_without_migration", "opaque-synthetic-only", "claude-opus-5", "https://api.deepseek.com/v1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			analyzer := ClaudeAnalyzer{APIKey: test.key, Model: test.model, BaseURL: test.base}
			_, err := analyzer.Analyze(context.Background(), papers.Paper{ID: "arxiv:2609.12345"})
			if !errors.Is(err, modelchat.ErrMigrationRequired) || calls != 0 {
				t.Fatalf("legacy digest settings sent HTTP: err=%v calls=%d", err, calls)
			}
			if strings.Contains(err.Error(), test.key) {
				t.Fatal("migration error exposed key")
			}
		})
	}
}

func TestDigestDeepSeekDefaultsUseChat(t *testing.T) {
	original := http.DefaultTransport
	calls := 0
	http.DefaultTransport = digestRoutingTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://api.deepseek.com/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer synthetic-deepseek-key" {
			t.Error("default digest routing is incorrect")
		}
		var wire struct {
			Model string `json:"model"`
		}
		if json.NewDecoder(r.Body).Decode(&wire) != nil || wire.Model != "deepseek-flash" {
			t.Error("default digest model is incorrect")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"synthetic summary"},"finish_reason":"stop"}]}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	analyzer := DeepSeekAnalyzer{APIKey: "synthetic-deepseek-key"}
	summary, err := analyzer.Analyze(context.Background(), papers.Paper{ID: "arxiv:2609.12345"})
	if err != nil || calls != 1 || summary.Model != "deepseek-flash" || summary.Text != "synthetic summary" {
		t.Fatalf("valid DeepSeek defaults failed: summary=%+v err=%v calls=%d", summary, err, calls)
	}
}

func TestValidatedAnalyzerKeepsPerSummaryRequestBudget(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer file-key" {
			t.Error("validated analyzer routing is incorrect")
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != defaultModel {
			t.Error("validated analyzer did not use the digest default model")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"摘要正文"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	analyzer := DeepSeekAnalyzer{APIKey: "file-key", BaseURL: server.URL + "/v1", Model: " \n"}
	if err := analyzer.Validate(); err != nil || calls != 0 {
		t.Fatalf("validation must be offline: err=%v calls=%d", err, calls)
	}
	for _, id := range []string{"2609.12345", "2609.12346"} {
		summary, err := analyzer.Analyze(context.Background(), papers.Paper{ID: id})
		if err != nil || summary.Model != defaultModel || summary.Text != "摘要正文" {
			t.Fatalf("validated analyzer request failed: summary=%+v err=%v", summary, err)
		}
	}
	if calls != 2 {
		t.Fatalf("expected one request per summary: calls=%d", calls)
	}
}

func TestChatUsesFileSettingsInsteadOfEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "environment-key")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("OPENAI_API_KEY", "environment-key")
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:1")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer file-key" {
			t.Errorf("未使用配置中的密钥")
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"file-key must not leak"}}`))
	}))
	defer server.Close()
	_, err := requestChat(context.Background(), "file-key", server.URL, "deepseek-flash", "测试")
	if err == nil || strings.Contains(err.Error(), "file-key") || calls != 1 {
		t.Fatalf("错误应脱敏且不重试: err=%v, calls=%d", err, calls)
	}
}

func TestRequestChatRoutesToConfiguredGateway(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer file-key" {
			t.Error("模型请求路径或认证不符合预期")
		}
		var body struct {
			Model    string `json:"model"`
			Thinking struct {
				Type string `json:"type"`
			} `json:"thinking"`
			MaxTokens int                              `json:"max_tokens"`
			Stream    bool                             `json:"stream"`
			Messages  []struct{ Role, Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "deepseek-flash" || body.Thinking.Type != "disabled" || body.Stream || body.MaxTokens != 1200 || len(body.Messages) != 1 || body.Messages[0].Content != "测试" {
			t.Error("模型请求体不符合预期")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"测试摘要"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()
	text, err := requestChat(context.Background(), "file-key", server.URL+"/v1", "deepseek-flash", "测试")
	if err != nil || text != "测试摘要" || calls != 1 {
		t.Fatalf("Chat 调用失败: text=%q, err=%v, calls=%d", text, err, calls)
	}
}

func TestRequestChatRejectsUnfinishedResponses(t *testing.T) {
	for name, response := range map[string]string{
		"length":     `{"choices":[{"message":{"role":"assistant","content":"不完整摘要"},"finish_reason":"length"}]}`,
		"tool_calls": `{"choices":[{"message":{"role":"assistant","content":"摘要","tool_calls":[{"id":"a","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		"refusal":    `{"choices":[{"message":{"role":"assistant","content":"摘要","refusal":"refused"},"finish_reason":"stop"}]}`,
		"empty":      `{"choices":[{"message":{"role":"assistant","content":" "},"finish_reason":"stop"}]}`,
		"no_choices": `{"choices":[]}`,
		"bad_json":   `{`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(response))
			}))
			defer server.Close()
			if _, err := requestChat(context.Background(), "file-key", server.URL, "deepseek-flash", "测试"); err == nil {
				t.Fatal("未完成的响应被接受")
			}
		})
	}
}

func TestRequestChatHonorsCanceledContext(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := requestChat(ctx, "file-key", server.URL, "deepseek-flash", "测试"); err == nil || calls != 0 {
		t.Fatalf("已取消的请求应停止: err=%v, calls=%d", err, calls)
	}
}

func TestClaudeAnalyzerWrapsRequestError(t *testing.T) {
	wantErr := errors.New("temporary API failure")
	analyzer := ClaudeAnalyzer{
		request: func(context.Context, string, string, string, string) (string, error) {
			return "", wantErr
		},
	}
	_, err := analyzer.Analyze(context.Background(), papers.Paper{ID: "2609.12345"})
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "2609.12345") {
		t.Fatalf("Analyze() error = %v, want wrapped request error", err)
	}
}
