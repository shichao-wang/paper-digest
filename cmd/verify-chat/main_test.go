package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/modelchat"
)

func TestCLIRequiresLiveBeforeConfigRead(t *testing.T) {
	var out bytes.Buffer
	if got := run([]string{"--config", "/does-not-exist"}, strings.NewReader(""), &out, &out); got != 2 || !strings.Contains(out.String(), "--live") || strings.Contains(out.String(), "无法读取") {
		t.Fatalf("must gate before configuration: %d %s", got, out.String())
	}
}
func TestSettingsOfficialOnlyAndStdin(t *testing.T) {
	for _, base := range []string{"https://api.deepseek.com/anthropic", "https://api.deepseek.com/v1"} {
		input := `{"anthropic":{"api_key":"synthetic-key","model":"deepseek-flash","base_url":"` + base + `"},"database":{"path":"ignored"}}`
		cfg, err := loadSettings(strings.NewReader(input))
		if err != nil || cfg.Anthropic.APIKey != "synthetic-key" {
			t.Fatalf("stdin settings: %v", err)
		}
	}
	for _, input := range []string{
		`{"anthropic":{"api_key":"key","model":"deepseek-flash","base_url":"https://evil.example/v1"}}`,
		`{"anthropic":{"api_key":"key","model":"other","base_url":"https://api.deepseek.com/v1"}}`,
		`{"anthropic":{"api_key":"key","model":"deepseek-flash","base_url":"https://api.deepseek.com/v1?x=secret"}}`,
		`{"anthropic":{"api_key":"key","model":"deepseek-flash","base_url":"https://api.deepseek.com/v1"},"anthropic":{}}`,
	} {
		if _, err := loadSettings(strings.NewReader(input)); err == nil {
			t.Fatal("unsafe settings accepted")
		}
	}
	var stdout, stderr bytes.Buffer
	got := run([]string{"--live", "--config", "-"}, strings.NewReader(`{"anthropic":{"api_key":"sensitive","model":"deepseek-flash","base_url":"http://localhost/v1"}}`), &stdout, &stderr)
	if got != 2 || strings.Contains(stderr.String(), "sensitive") {
		t.Fatalf("stdin CLI leaked: %d %s", got, stderr.String())
	}
}
func TestReportPermissionsAndNoRawInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if os.WriteFile(path, []byte("old"), 0644) != nil {
		t.Fatal("fixture write")
	}
	r := report{MaxRequests: 30, Requests: 2, Results: []result{{Name: "case", Status: "failed", Detail: "chat transport failed"}}}
	if err := saveReport(path, r); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "api_key") || strings.Contains(string(data), "messages") {
		t.Fatal("raw input in report")
	}
	symlink := filepath.Join(t.TempDir(), "linked.json")
	if os.Symlink(path, symlink) != nil {
		t.Fatal("symlink fixture")
	}
	if saveReport(symlink, r) == nil {
		t.Fatal("symlink accepted")
	}
}

func answer(w http.ResponseWriter, content any) {
	encoded, _ := json.Marshal(content)
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": string(encoded)}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
}
func testClient(t *testing.T, server *httptest.Server, b *modelchat.Budget) *modelchat.Client {
	t.Helper()
	c, err := modelchat.NewClient(modelchat.Options{APIKey: "synthetic-only", BaseURL: server.URL + "/v1", Model: "deepseek-flash", Budget: b, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestRealSchemaRejectThenRepair(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var wire struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Format struct {
				Type string `json:"type"`
			} `json:"response_format"`
			Tools json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&wire)
		if wire.Format.Type != "json_object" || len(wire.Tools) != 0 {
			t.Error("repair must use isolated JSON without tools")
		}
		if calls == 1 {
			answer(w, map[string]any{"version": "v2", "code": "random-test-code", "debug": true})
			return
		}
		if len(wire.Messages) != 4 || wire.Messages[2].Role != "assistant" || !strings.Contains(wire.Messages[3].Content, "Local validator rejected") || !strings.Contains(wire.Messages[3].Content, "unexpected extra field") {
			t.Error("missing invalid assistant and local error repair history")
		}
		answer(w, map[string]any{"version": "v2", "code": "random-test-code"})
	}))
	defer server.Close()
	budget := modelchat.NewBudget(3)
	res := result{}
	err := finalJSON(context.Background(), testClient(t, server, budget), []json.RawMessage{modelchat.Message("user", "version v2 code random-test-code")}, map[string]modelchat.Field{"version": stringField("v2"), "code": stringField("random-test-code")}, &res, true, 2, nil)
	if err != nil || calls != 2 || !res.InvalidObserved || !res.RepairedObserved {
		t.Fatalf("repair transition missing: %v %+v calls %d", err, res, calls)
	}
}
func TestRepairCannotPassWithoutInvalidOrWithWrongEvidence(t *testing.T) {
	for _, mode := range []string{"first_already_valid", "never_repaired"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if mode == "first_already_valid" {
					answer(w, map[string]string{"version": "v2", "code": "expected"})
				} else {
					answer(w, map[string]any{"version": "v999", "code": "wrong", "debug": true})
				}
			}))
			defer server.Close()
			res := result{}
			err := finalJSON(context.Background(), testClient(t, server, modelchat.NewBudget(5)), []json.RawMessage{modelchat.Message("user", "evidence")}, map[string]modelchat.Field{"version": stringField("v2"), "code": stringField("expected")}, &res, true, 2, nil)
			if err == nil || res.RepairedObserved || calls > 3 {
				t.Fatalf("false repair success: %v %+v calls %d", err, res, calls)
			}
		})
	}
}
func TestLongInputSingleRequestBothBookends(t *testing.T) {
	calls := 0
	size := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var wire struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&wire)
		text := wire.Messages[0].Content
		size = len(text)
		first := regexp.MustCompile(`HEAD_CODE=([a-f0-9]+)`).FindStringSubmatch(text)
		last := regexp.MustCompile(`TAIL_CODE=([a-f0-9]+)`).FindStringSubmatch(text)
		if len(first) != 2 || len(last) != 2 {
			t.Error("bookend evidence missing")
			w.WriteHeader(500)
			return
		}
		answer(w, map[string]string{"version": "v2", "head_code": first[1], "tail_code": last[1]})
	}))
	defer server.Close()
	res := result{}
	err := capacityCase(context.Background(), testClient(t, server, modelchat.NewBudget(5)), &res)
	if err != nil || calls != 1 || size < 50000 || size > 55000 {
		t.Fatalf("capacity invalid: %v calls %d bytes %d", err, calls, size)
	}
}
func TestCapacityWrongCodeDoesNotRepair(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		answer(w, map[string]string{"version": "v2", "head_code": "wrong", "tail_code": "wrong"})
	}))
	defer server.Close()
	res := result{}
	err := capacityCase(context.Background(), testClient(t, server, modelchat.NewBudget(5)), &res)
	if err == nil || calls != 1 {
		t.Fatalf("capacity false pass or retried: %v calls %d", err, calls)
	}
}
func TestBudgetPausesUnfinishedAndContinuesFailures(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "untrusted body and secret")
	}))
	defer server.Close()
	budget := modelchat.NewBudget(2)
	r := runChecks(context.Background(), testClient(t, server, budget), budget)
	if requests != 2 || r.Requests != 2 || len(r.Results) != len(caseNames) {
		t.Fatalf("budget or continuation: requests %d report %+v", requests, r)
	}
	if r.Results[0].Status != "failed" || r.Results[1].Status != "failed" {
		t.Fatal("failure stopped next case or falsely passed")
	}
	for _, res := range r.Results[2:] {
		if res.Status != "inconclusive" || res.Requests != 0 {
			t.Errorf("budget pause false pass: %+v", res)
		}
	}
}
func TestOfficialRunDoesNotSendToUnapprovedBase(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer server.Close()
	var out bytes.Buffer
	cfg := `{"anthropic":{"api_key":"test","model":"deepseek-flash","base_url":"` + server.URL + `/v1"}}`
	if code := run([]string{"--live", "--config", "-"}, strings.NewReader(cfg), &out, &out); code != 2 || requests != 0 {
		t.Fatalf("CLI sent to unapproved base: %d requests %d", code, requests)
	}
}

func TestRelevanceUsesRecommendationAdvertisingSearchUnion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wire struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&wire)
		prompt := wire.Messages[0].Content
		if !strings.Contains(prompt, "Recommendation, Advertising, Search") || !strings.Contains(prompt, "NEVER mandatory or sufficient") || strings.Contains(prompt, "quantitative investment") {
			t.Error("wrong topics or keyword rule")
		}
		if !strings.Contains(prompt, "historical_abstract_relevant for B") || !strings.Contains(prompt, "Each boolean means whether that abstract's research problem is directly relevant") || strings.Contains(prompt, "misleading_keywords") {
			t.Error("相关性输出字段仍有语义歧义")
		}
		abstractA := strings.Split(strings.Split(prompt, "A: ")[1], "\nB:")[0]
		for _, keyword := range []string{"recommendation", "advertising", "search"} {
			if strings.Contains(strings.ToLower(abstractA), keyword) {
				t.Error("no-keyword abstract contains topic keyword")
			}
		}
		evidence := regexp.MustCompile(`evidence_code ([a-f0-9]+)`).FindStringSubmatch(prompt)
		if len(evidence) != 2 {
			t.Error("missing evidence")
			w.WriteHeader(500)
			return
		}
		answer(w, map[string]any{"version": "v2", "evidence_code": evidence[1], "recommendation_relevant": true, "historical_abstract_relevant": false, "advertising_relevant": true, "search_relevant": true})
	}))
	defer server.Close()
	res := result{}
	if err := runCase(context.Background(), testClient(t, server, modelchat.NewBudget(5)), "relevance_union", &res); err != nil {
		t.Fatal(err)
	}
}
func TestVersionComparisonReadsBothVersionsAndActualChanges(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var wire struct {
			Messages []json.RawMessage `json:"messages"`
			Format   struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		_ = json.NewDecoder(r.Body).Decode(&wire)
		if requests == 1 {
			calls := []any{}
			for _, version := range []string{"v2", "v1"} {
				for _, section := range []string{"method", "results"} {
					args, _ := json.Marshal(map[string]string{"version": version, "section": section})
					calls = append(calls, map[string]any{"id": version + "-" + section, "type": "function", "function": map[string]string{"name": "read_section", "arguments": string(args)}})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": nil, "reasoning_content": "preserve this", "tool_calls": calls}, "finish_reason": "tool_calls"}}})
			return
		}
		if requests == 2 {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "both versions read"}, "finish_reason": "stop"}}})
			return
		}
		if wire.Format.Type != "json_object" || len(wire.Messages) != 8 {
			t.Error("invalid final JSON history")
		}
		out := map[string]string{"version": "v2", "previous_version": "v1"}
		labels := map[string]string{}
		for _, raw := range wire.Messages {
			var message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			_ = json.Unmarshal(raw, &message)
			if message.Role != "tool" {
				continue
			}
			var evidence struct {
				Version string `json:"version"`
				Section string `json:"section"`
				Code    string `json:"code"`
				Text    string `json:"text"`
			}
			_ = json.Unmarshal([]byte(message.Content), &evidence)
			key := evidence.Section + "_" + evidence.Version
			out[key] = evidence.Code
			expression := `METHOD_LABEL=([a-z_]+)`
			if evidence.Section == "results" {
				expression = `RESULT_LABEL=([a-z_0-9.]+)`
			}
			matches := regexp.MustCompile(expression).FindStringSubmatch(evidence.Text)
			if len(matches) != 2 {
				t.Error("missing actual change facts")
				continue
			}
			labels[key] = strings.TrimSuffix(matches[1], ".")
		}
		if len(labels) != 4 {
			t.Error("missing version section evidence")
		}
		out["method_change"] = labels["method_v1"] + "->" + labels["method_v2"]
		out["result_change"] = labels["results_v1"] + "->" + labels["results_v2"]
		answer(w, out)
	}))
	defer server.Close()
	res := result{}
	if err := runCase(context.Background(), testClient(t, server, modelchat.NewBudget(7)), "version_comparison", &res); err != nil || requests != 3 {
		t.Fatalf("version comparison failed: %v requests %d", err, requests)
	}
}
func TestTextSummaryNormalChatAndEvidence(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var wire struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
			MaxTokens int             `json:"max_tokens"`
			Tools     json.RawMessage `json:"tools"`
			Format    json.RawMessage `json:"response_format"`
		}
		_ = json.NewDecoder(r.Body).Decode(&wire)
		if wire.MaxTokens != 1200 || len(wire.Tools) != 0 || len(wire.Format) != 0 {
			t.Error("text summary must be ordinary Chat")
		}
		code := regexp.MustCompile(`[a-f0-9]{24}`).FindString(wire.Messages[0].Content)
		content := "1. v2方法使用成对对比学习，证据 " + code + "\n2. NDCG@10 从0.31提升至0.38。\n3. 局限为单一离线数据集。"
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
	}))
	defer server.Close()
	res := result{}
	if err := runCase(context.Background(), testClient(t, server, modelchat.NewBudget(2)), "text_summary", &res); err != nil || requests != 1 {
		t.Fatalf("text summary: %v requests %d", err, requests)
	}
}

func TestFirstRoundRejectedCallsCannotPassLaterReads(t *testing.T) {
	for _, name := range []string{"multi_tool_results", "section_error_recovery", "sequential_method_results"} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				calls := []any{}
				makeCall := func(id, section string) any {
					args, _ := json.Marshal(map[string]string{"version": "v2", "section": section})
					return map[string]any{"id": id, "type": "function", "function": map[string]string{"name": "read_section", "arguments": string(args)}}
				}
				if requests == 1 {
					calls = append(calls, map[string]any{"id": "bad", "type": "function", "function": map[string]string{"name": "read_section", "arguments": "{broken"}})
					if name == "multi_tool_results" {
						calls = append(calls, map[string]any{"id": "unknown", "type": "function", "function": map[string]string{"name": "shell", "arguments": "{}"}})
					}
				} else if requests == 2 {
					if name == "section_error_recovery" {
						calls = append(calls, makeCall("late-missing", "conclusion"))
					}
					calls = append(calls, makeCall("late-method", "method"), makeCall("late-results", "results"))
				} else {
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "done"}, "finish_reason": "stop"}}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": calls}, "finish_reason": "tool_calls"}}})
			}))
			defer server.Close()
			res := result{}
			err := runCase(context.Background(), testClient(t, server, modelchat.NewBudget(7)), name, &res)
			if err == nil || requests != 3 || len(res.Calls) == 0 || res.Calls[0].Validated || res.Calls[0].Executed {
				t.Fatalf("late reads falsely passed: %v requests %d calls %+v", err, requests, res.Calls)
			}
		})
	}
}
func TestTextSummaryRejectsRepeatedNumbersAndMissingFacts(t *testing.T) {
	for _, mode := range []string{"repeated_number", "missing_facts"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var wire struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				_ = json.NewDecoder(r.Body).Decode(&wire)
				code := regexp.MustCompile(`[a-f0-9]{24}`).FindString(wire.Messages[0].Content)
				content := "1. v2 成对对比学习 " + code + "\n1. NDCG@10 0.31 0.38\n1. 单一离线数据集"
				if mode == "missing_facts" {
					content = "1. v2 无内容 " + code + "\n2. 无结果\n3. 无局限"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
			}))
			defer server.Close()
			res := result{}
			if err := textSummaryCase(context.Background(), testClient(t, server, modelchat.NewBudget(1)), &res); err == nil {
				t.Fatal("invalid summary passed")
			}
		})
	}
}
func TestSelectedCasesShareExactBudget(t *testing.T) {
	selection, err := selectCases("text_summary,sequential_method_results,multi_tool_results,section_error_recovery", 12)
	if err != nil || len(selection) != 4 {
		t.Fatal("selection failed")
	}
	for _, value := range []string{"unknown", "text_summary,text_summary", "text_summary,"} {
		if _, err := selectCases(value, 12); err == nil {
			t.Fatal("invalid selection accepted")
		}
	}
	for _, limit := range []int{0, 31} {
		if _, err := selectCases("", limit); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
	defer server.Close()
	completed := 0
	budget := modelchat.NewBudget(2)
	report := runSelectedChecks(context.Background(), testClient(t, server, budget), budget, selection, 2, func(result) { completed++ })
	if report.MaxRequests != 2 || report.Requests != 2 || calls != 2 || completed != 4 || report.Results[2].Status != "inconclusive" || report.Results[3].Status != "inconclusive" {
		t.Fatalf("bad shared budget: %+v calls %d completed %d", report, calls, completed)
	}
}
