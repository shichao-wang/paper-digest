package modelchat

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
)

func clientFor(t *testing.T, server *httptest.Server, budget *Budget, timeout time.Duration) *Client {
	t.Helper()
	c, err := NewClient(Options{APIKey: "test-secret-never-log", BaseURL: server.URL + "/v1", Model: "synthetic-model", Budget: budget, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func respond(w http.ResponseWriter, message any, stop string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": stop}}, "usage": map[string]int{"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}})
}
func toolCall(id, name, args string) any {
	return map[string]any{"id": id, "type": "function", "function": map[string]string{"name": name, "arguments": args}}
}
func TestHTTPToolHistoryAndFinalJSON(t *testing.T) {
	requests := 0
	executed := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-secret-never-log" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("incorrect request routing/authentication")
		}
		data, _ := io.ReadAll(r.Body)
		var wire struct {
			Model    string `json:"model"`
			Thinking struct {
				Type string `json:"type"`
			} `json:"thinking"`
			Messages       []json.RawMessage `json:"messages"`
			Tools          []any             `json:"tools"`
			ResponseFormat struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		if json.Unmarshal(data, &wire) != nil {
			t.Error("invalid request JSON")
		}
		if wire.Model != "synthetic-model" || wire.Thinking.Type != "disabled" {
			t.Error("model/thinking wrong")
		}
		if requests == 1 {
			if len(wire.Tools) != 1 {
				t.Error("missing tool definition")
			}
			respond(w, map[string]any{"role": "assistant", "content": nil, "reasoning_content": "retain reasoning exactly", "tool_calls": []any{toolCall("call-a", "read", `{"section":"method"}`), toolCall("call-b", "read", `{"section":"results"}`)}, "future_field": "keep-me"}, "tool_calls")
			return
		}
		if len(wire.Messages) < 4 {
			t.Error("lost history")
			return
		}
		var assistant map[string]any
		_ = json.Unmarshal(wire.Messages[1], &assistant)
		if assistant["reasoning_content"] != "retain reasoning exactly" || assistant["future_field"] != "keep-me" || len(assistant["tool_calls"].([]any)) != 2 {
			t.Error("assistant fields lost")
		}
		for i, want := range []string{"call-a", "call-b"} {
			var tool struct {
				Role    string `json:"role"`
				ID      string `json:"tool_call_id"`
				Content string `json:"content"`
			}
			_ = json.Unmarshal(wire.Messages[2+i], &tool)
			if tool.Role != "tool" || tool.ID != want || !strings.Contains(tool.Content, "random-code-") {
				t.Errorf("missing complete result for %s", want)
			}
		}
		if requests == 2 {
			if wire.ResponseFormat.Type != "" {
				t.Error("JSON before tool stop")
			}
			respond(w, map[string]string{"role": "assistant", "content": "evidence read"}, "stop")
			return
		}
		if len(wire.Messages) != 6 || wire.ResponseFormat.Type != "json_object" || len(wire.Tools) != 0 {
			t.Error("final JSON must preserve complete history with no tools")
		}
		respond(w, map[string]string{"role": "assistant", "content": `{"ok":true}`}, "stop")
	}))
	defer server.Close()
	c := clientFor(t, server, NewBudget(3), time.Second)
	tool := Tool{Definition: ToolDefinition{Type: "function", Function: Function{Name: "read", Parameters: map[string]any{"type": "object"}}}, Validate: func(raw json.RawMessage) error {
		return ValidateObject(raw, map[string]Field{"section": {Type: "string"}})
	}, Execute: func(_ context.Context, raw json.RawMessage) (any, error) {
		executed++
		return map[string]string{"code": "random-code-" + string(raw)}, nil
	}}
	s, err := c.RunTools(context.Background(), []json.RawMessage{Message("user", "read synthetic method and results")}, []Tool{tool}, 4)
	if err != nil || executed != 2 {
		t.Fatalf("tool flow: %v executed %d", err, executed)
	}
	history := append(s.History, Message("user", "Return JSON"))
	response, err := c.Chat(context.Background(), Request{Messages: history, JSONObject: true})
	if err != nil || response.Content != `{"ok":true}` || response.Usage.TotalTokens != 10 || requests != 3 {
		t.Fatalf("final response: %+v %v requests %d", response, err, requests)
	}
}
func TestRejectedToolsAlwaysHaveResultsWithoutExecution(t *testing.T) {
	requests := 0
	executed := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			respond(w, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{toolCall("unknown", "shell", `{"cmd":"steal"}`), toolCall("malformed", "read", `{"section":`), toolCall("null", "read", `{"section":null}`), toolCall("extra", "read", `{"section":"method","key":"secret"}`), toolCall("duplicate", "read", `{"section":"method","section":"results"}`)}}, "tool_calls")
			return
		}
		var wire struct {
			Messages []json.RawMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&wire)
		if len(wire.Messages) != 7 {
			t.Errorf("all five calls must have results, got %d messages", len(wire.Messages))
		}
		for i := 2; i < len(wire.Messages); i++ {
			var tool struct {
				ID      string `json:"tool_call_id"`
				Content string `json:"content"`
			}
			_ = json.Unmarshal(wire.Messages[i], &tool)
			if tool.ID == "" || !strings.Contains(tool.Content, "error") {
				t.Error("rejected call missing error result")
			}
		}
		respond(w, map[string]string{"role": "assistant", "content": "done"}, "stop")
	}))
	defer server.Close()
	c := clientFor(t, server, NewBudget(3), time.Second)
	tool := Tool{Definition: ToolDefinition{Type: "function", Function: Function{Name: "read"}}, Validate: func(raw json.RawMessage) error {
		return ValidateObject(raw, map[string]Field{"section": {Type: "string"}})
	}, Execute: func(context.Context, json.RawMessage) (any, error) { executed++; return nil, nil }}
	_, err := c.RunTools(context.Background(), []json.RawMessage{Message("user", "test")}, []Tool{tool}, 2)
	if err != nil || executed != 0 {
		t.Fatalf("unsafe execution: %d %v", executed, err)
	}
}
func TestUnsuccessfulResponsesAreNeverSuccess(t *testing.T) {
	cases := map[string]string{
		"length":          `{"choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}]}`,
		"content_filter":  `{"choices":[{"message":{"role":"assistant","content":"filtered"},"finish_reason":"content_filter"}]}`,
		"refusal":         `{"choices":[{"message":{"role":"assistant","content":"text","refusal":"secret body"},"finish_reason":"stop"}]}`,
		"empty_choices":   `{"choices":[]}`,
		"empty_body":      "",
		"null_content":    `{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"stop"}]}`,
		"unknown_stop":    `{"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"refusal"}]}`,
		"stop_with_tools": `{"choices":[{"message":{"role":"assistant","content":"x","tool_calls":[{"id":"a","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"stop"}]}`,
		"missing_id":      `{"choices":[{"message":{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		"duplicates":      `{"choices":[],"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			count := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count++; _, _ = io.WriteString(w, body) }))
			defer server.Close()
			_, err := clientFor(t, server, NewBudget(5), time.Second).Chat(context.Background(), Request{Messages: []json.RawMessage{Message("user", "test")}})
			if err == nil || count != 1 || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid response succeeded or leaked: %v count %d", err, count)
			}
		})
	}
}
func TestTimeoutCancelHTTPAndRedirectAreSafe(t *testing.T) {
	t.Run("http_error", func(t *testing.T) {
		count := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count++
			w.WriteHeader(429)
			_, _ = io.WriteString(w, "test-secret-never-log raw provider body")
		}))
		defer server.Close()
		_, err := clientFor(t, server, NewBudget(4), time.Second).Chat(context.Background(), Request{Messages: []json.RawMessage{Message("user", "x")}})
		if err == nil || count != 1 || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) {
			t.Fatalf("unsafe error %v, count %d", err, count)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-time.After(100 * time.Millisecond):
			}
		}))
		defer server.Close()
		_, err := clientFor(t, server, NewBudget(2), 30*time.Millisecond).Chat(context.Background(), Request{Messages: []json.RawMessage{Message("user", "x")}})
		if err == nil || strings.Contains(err.Error(), server.URL) {
			t.Fatalf("unsafe timeout %v", err)
		}
	})
	t.Run("context", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-time.After(100 * time.Millisecond):
			}
		}))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err := clientFor(t, server, NewBudget(2), time.Second).Chat(ctx, Request{Messages: []json.RawMessage{Message("user", "x")}})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lost context deadline: %v", err)
		}
		ctx, cancel = context.WithCancel(context.Background())
		cancel()
		_, err = clientFor(t, server, NewBudget(2), time.Second).Chat(ctx, Request{Messages: []json.RawMessage{Message("user", "x")}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lost cancel: %v", err)
		}
	})
	t.Run("redirect", func(t *testing.T) {
		destinationCalls := 0
		destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls++ }))
		defer destination.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
		defer server.Close()
		_, err := clientFor(t, server, NewBudget(2), time.Second).Chat(context.Background(), Request{Messages: []json.RawMessage{Message("user", "x")}})
		if err == nil || destinationCalls != 0 || strings.Contains(err.Error(), server.URL) {
			t.Fatalf("redirect followed or leaked: %v calls %d", err, destinationCalls)
		}
	})
}
func TestBudgetAndUnfinishedToolStage(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		respond(w, map[string]any{"role": "assistant", "tool_calls": []any{toolCall("call", "unknown", "{}")}}, "tool_calls")
	}))
	defer server.Close()
	budget := NewBudget(2)
	c := clientFor(t, server, budget, time.Second)
	tool := Tool{Definition: ToolDefinition{Type: "function", Function: Function{Name: "read"}}, Validate: func(json.RawMessage) error { return nil }, Execute: func(context.Context, json.RawMessage) (any, error) { return nil, nil }}
	_, err := c.RunTools(context.Background(), []json.RawMessage{Message("user", "loop")}, []Tool{tool}, 2)
	if !errors.Is(err, ErrToolRounds) || calls != 2 || budget.Used() != 2 {
		t.Fatalf("unfinished tool stage passed: %v %d", err, calls)
	}
	_, err = c.Chat(context.Background(), Request{Messages: []json.RawMessage{Message("user", "x")}})
	if !errors.Is(err, ErrBudget) || calls != 2 {
		t.Fatalf("budget exceeded: %v %d", err, calls)
	}
}

type routingTransport func(*http.Request) (*http.Response, error)

func (f routingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLegacySettingsRejectedBeforeHTTP(t *testing.T) {
	for _, test := range []struct {
		name, base, model, key string
	}{
		{"old_sample", "", "claude-opus-5", "sk-ant-synthetic-only"},
		{"opaque_legacy_key", "", "claude-opus-5", "opaque-synthetic-only"},
		{"whitespace_base", " \n\t", " claude-opus-5 ", "opaque-synthetic-only"},
		{"unspecified_provider", "", "custom-model", "opaque-synthetic-only"},
		{"caller_defaulted_base", "https://api.deepseek.com/v1", "claude-opus-5", "opaque-synthetic-only"},
		{"changed_model_only", "", "deepseek-flash", "sk-ant-synthetic-only"},
		{"official_explicit", "https://api.deepseek.com/v1", "deepseek-flash", "sk-ant-synthetic-only"},
		{"official_anthropic_alias", "https://api.deepseek.com/anthropic", "deepseek-flash", "sk-ant-synthetic-only"},
		{"official_case_and_port", "https://API.DEEPSEEK.COM:443/v1", "deepseek-flash", "sk-ant-synthetic-only"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			budget := NewBudget(1)
			client, err := NewClient(Options{APIKey: test.key, BaseURL: test.base, Model: test.model, Budget: budget,
				HTTPClient: &http.Client{Transport: routingTransport(func(*http.Request) (*http.Response, error) {
					calls++
					return nil, errors.New("unexpected HTTP request")
				})},
			})
			if err == nil {
				_, err = client.Chat(context.Background(), Request{Messages: []json.RawMessage{Message("user", "synthetic")}})
			}
			if !errors.Is(err, ErrMigrationRequired) || client != nil || calls != 0 || budget.Used() != 0 {
				t.Fatalf("legacy routing was not rejected before HTTP: err=%v calls=%d budget=%d", err, calls, budget.Used())
			}
			if strings.Contains(err.Error(), test.key) || strings.Contains(err.Error(), test.model) || strings.Contains(err.Error(), "api.deepseek.com") {
				t.Fatal("migration error exposed configured values")
			}
		})
	}
}

func TestDeepSeekDefaultAndExplicitCompatibleChatRouting(t *testing.T) {
	for _, test := range []struct {
		name, base, model, key, endpoint string
	}{
		{"official_default", "", "deepseek-flash", "synthetic-deepseek-key", "https://api.deepseek.com/v1/chat/completions"},
		{"official_whitespace_default", " \n", " deepseek-flash ", "synthetic-deepseek-key", "https://api.deepseek.com/v1/chat/completions"},
		{"official_model_alias", "", "deepseek-v4-1-flash", "synthetic-deepseek-key", "https://api.deepseek.com/v1/chat/completions"},
		{"official_anthropic_alias", "https://api.deepseek.com/anthropic", "deepseek-flash", "synthetic-deepseek-key", "https://api.deepseek.com/v1/chat/completions"},
		{"custom_root", "https://gateway.example", "claude-opus-5", "sk-ant-synthetic-only", "https://gateway.example/v1/chat/completions"},
		{"custom_v1", "https://gateway.example/v1/", "group/deepseek-v4-1-flash", "opaque-synthetic-only", "https://gateway.example/v1/chat/completions"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			budget := NewBudget(1)
			client, err := NewClient(Options{APIKey: test.key, BaseURL: test.base, Model: test.model, Budget: budget,
				HTTPClient: &http.Client{Transport: routingTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.String() != test.endpoint || r.Header.Get("Authorization") != "Bearer "+test.key {
						t.Error("request routed outside configured Chat endpoint")
					}
					var wire struct {
						Model string `json:"model"`
					}
					if json.NewDecoder(r.Body).Decode(&wire) != nil || wire.Model != strings.TrimSpace(test.model) {
						t.Error("configured model was not retained")
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"synthetic response"},"finish_reason":"stop"}]}`))}, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Chat(context.Background(), Request{Messages: []json.RawMessage{Message("user", "synthetic")}})
			if err != nil || calls != 1 || budget.Used() != 1 || response.Content != "synthetic response" {
				t.Fatalf("valid Chat routing failed: err=%v calls=%d budget=%d", err, calls, budget.Used())
			}
		})
	}
}

func TestBaseURLRules(t *testing.T) {
	for _, base := range []string{"https://old-gateway.example/anthropic", "http://api.deepseek.com/anthropic", "https://api.deepseek.com/anthropic?secret=x", "https://key@api.deepseek.com/v1", "https://api.deepseek.com/v1/#x"} {
		if _, err := NewClient(Options{APIKey: "x", Model: "deepseek-flash", BaseURL: base}); err == nil {
			t.Errorf("accepted unsafe base %s", base)
		}
	}
	for _, base := range []string{"https://api.deepseek.com/anthropic", "https://api.deepseek.com/anthropic/", "https://api.deepseek.com/v1", "https://gateway.example/v1"} {
		c, err := NewClient(Options{APIKey: "x", Model: "deepseek-flash", BaseURL: base})
		if err != nil || !strings.HasSuffix(c.endpoint, "/v1/chat/completions") {
			t.Errorf("valid base failed %s: %v", base, err)
		}
	}
}

func TestToolCallsWithoutOfferedToolsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]any{"role": "assistant", "tool_calls": []any{toolCall("a", "read", "{}")}}, "tool_calls")
	}))
	defer server.Close()
	c := clientFor(t, server, NewBudget(5), time.Second)
	for _, jsonMode := range []bool{false, true} {
		if _, err := c.Chat(context.Background(), Request{Messages: []json.RawMessage{Message("user", "x")}, JSONObject: jsonMode}); err == nil {
			t.Fatal("tool response accepted without offered tools")
		}
	}
}
func TestCanceledMultiToolDoesNotExecuteRemainingCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]any{"role": "assistant", "tool_calls": []any{toolCall("a", "read", "{}"), toolCall("b", "read", "{}")}}, "tool_calls")
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executed := 0
	tool := Tool{Definition: ToolDefinition{Type: "function", Function: Function{Name: "read"}}, Validate: func(json.RawMessage) error { return nil }, Execute: func(context.Context, json.RawMessage) (any, error) {
		executed++
		cancel()
		return map[string]string{"code": "first"}, nil
	}}
	_, err := clientFor(t, server, NewBudget(5), time.Second).RunTools(ctx, []json.RawMessage{Message("user", "x")}, []Tool{tool}, 4)
	if !errors.Is(err, context.Canceled) || executed != 1 {
		t.Fatalf("canceled tool flow continued: %v executed %d", err, executed)
	}
}

func TestHeadersThenBodyTimeoutRetainsHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer server.Close()
	response, err := clientFor(t, server, NewBudget(1), 30*time.Millisecond).Chat(context.Background(), Request{Messages: []json.RawMessage{Message("user", "x")}})
	if err == nil || err.Error() != "chat request timed out" || response.HTTPStatus != 200 || errors.Is(err, ErrProtocol) {
		t.Fatalf("body timeout mislabeled: %v status %d", err, response.HTTPStatus)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	response, err = clientFor(t, server, NewBudget(1), time.Second).Chat(ctx, Request{Messages: []json.RawMessage{Message("user", "x")}})
	if !errors.Is(err, context.DeadlineExceeded) || response.HTTPStatus != 200 {
		t.Fatalf("body context deadline lost: %v status %d", err, response.HTTPStatus)
	}
}
