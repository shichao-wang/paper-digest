package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func response(w http.ResponseWriter, content any, stop string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "model": "fixture", "content": content, "stop_reason": stop, "usage": map[string]int{"input_tokens": 10, "output_tokens": 10}})
}

func call(id, section string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": "read_section", "input": map[string]any{"section": section}}
}

func TestToolProtocolsWithFakeServer(t *testing.T) {
	for _, mode := range []string{"sdk_runner_sequential", "messages_multi_tool", "tool_error_recovery"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/messages" {
					t.Errorf("请求路径 %s", r.URL.Path)
				}
				var body struct {
					Messages []struct {
						Role    string `json:"role"`
						Content []struct {
							Type    string          `json:"type"`
							Text    string          `json:"text"`
							Content json.RawMessage `json:"content"`
							IsError bool            `json:"is_error"`
						} `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if calls == 1 {
					if mode == "messages_multi_tool" {
						response(w, []any{call("tool1", "method"), call("tool2", "results")}, "tool_use")
					} else if mode == "tool_error_recovery" {
						response(w, []any{call("tool1", "deliberately_missing")}, "tool_use")
					} else {
						response(w, []any{call("tool1", "method")}, "tool_use")
					}
					return
				}
				if calls == 2 && mode != "messages_multi_tool" {
					if mode == "tool_error_recovery" {
						last := body.Messages[len(body.Messages)-1]
						if len(last.Content) != 1 || !last.Content[0].IsError {
							t.Error("未以 is_error 返回章节错误")
						}
						response(w, []any{call("tool2", "method"), call("tool3", "results")}, "tool_use")
					} else {
						response(w, []any{call("tool2", "results")}, "tool_use")
					}
					return
				}
				if mode == "messages_multi_tool" {
					last := body.Messages[len(body.Messages)-1]
					if last.Role != "user" || len(last.Content) != 2 {
						t.Error("多个工具结果必须一次完整回传")
					}
				}
				method, resultCode := "", ""
				for _, m := range body.Messages {
					for _, b := range m.Content {
						texts := string(b.Content)
						for _, pair := range []struct {
							key string
							out *string
						}{{"method_code=", &method}, {"result_code=", &resultCode}} {
							idx := strings.Index(texts, pair.key)
							if idx >= 0 && len(texts) >= idx+len(pair.key)+16 {
								*pair.out = texts[idx+len(pair.key) : idx+len(pair.key)+16]
							}
						}
					}
				}
				if method == "" || resultCode == "" {
					t.Error("追加式历史未保留工具提供的随机证据")
				}
				text := fmt.Sprintf(`{"version":"v2","method_code":%q,"result_code":%q,"missing":%t}`, method, resultCode, mode == "tool_error_recovery")
				response(w, []any{map[string]any{"type": "text", "text": text}}, "end_turn")
			}))
			defer server.Close()
			client := anthropic.NewClient(option.WithoutEnvironmentDefaults(), option.WithAPIKey("fixture"), option.WithBaseURL(server.URL), option.WithMaxRetries(0))
			var got result
			if mode == "sdk_runner_sequential" {
				got = runnerCheck(context.Background(), client, "fixture")
			} else {
				got = messagesCheck(context.Background(), client, "fixture", mode)
			}
			if got.Status != "passed" {
				t.Fatalf("验证失败：%+v", got)
			}
		})
	}
}

func TestStructuredOutputRejectionAndStopReasons(t *testing.T) {
	for _, stop := range []string{"end_turn", "max_tokens", "refusal"} {
		t.Run(stop, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				response(w, []any{map[string]any{"type": "text", "text": "说明：```json\n{}\n```"}}, stop)
			}))
			defer server.Close()
			client := anthropic.NewClient(option.WithoutEnvironmentDefaults(), option.WithAPIKey("fixture"), option.WithBaseURL(server.URL), option.WithMaxRetries(0))
			got := structuredCheck(context.Background(), client, "fixture")
			if got.Status == "passed" || calls != 1 {
				t.Fatalf("不合格响应不应通过或重试：%+v，calls=%d", got, calls)
			}
		})
	}
}

func TestAnswerRejectsNullUnknownAndWrongEvidence(t *testing.T) {
	for _, text := range []string{
		`{"version":"v2","method_code":"m","result_code":"r","missing":null}`,
		`{"version":"v2","method_code":"m","result_code":"r","missing":false,"extra":true}`,
		`{"version":"v1","method_code":"m","result_code":"r","missing":false}`,
		`{"version":"v2","method_code":"m","result_code":"wrong","missing":false}`,
	} {
		if _, err := validateAnswer(text, "m", "r", false); err == nil {
			t.Fatalf("无效结果被接受：%s", text)
		}
	}
}
