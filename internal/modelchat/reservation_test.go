package modelchat

import (
	"encoding/json"
	"testing"
)

func TestReservationTokensIncludesPromptToolsOutputAndModel(t *testing.T) {
	for _, request := range []Request{
		{Messages: []json.RawMessage{Message("system", "可信规则"), Message("user", "paper metadata")}, JSONObject: true, MaxTokens: AnalysisOutputTokens},
		{Messages: []json.RawMessage{Message("user", "read the paper"), ToolResult("call-1", map[string]string{"text": "研究正文"})}, Tools: []ToolDefinition{{Type: "function", Function: Function{Name: "read_block", Parameters: map[string]any{"type": "object"}}}}, MaxTokens: 1024},
	} {
		for _, model := range []string{"", "deepseek-flash", "gateway/模型"} {
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ReservationTokens(request, model)
			want := int64(len(raw) + 1024 + 6*len(model) + request.MaxTokens)
			if err != nil || got != want {
				t.Fatalf("reservation=%d want=%d err=%v", got, want, err)
			}
		}
	}
}

func TestReservationTokensRejectsUnspecifiedOutputOrInvalidJSON(t *testing.T) {
	for _, request := range []Request{
		{MaxTokens: 0},
		{MaxTokens: -1},
		{MaxTokens: AnalysisOutputTokens, Messages: []json.RawMessage{json.RawMessage(`{"private-secret"`)}},
		{MaxTokens: AnalysisOutputTokens, Tools: []ToolDefinition{{Function: Function{Parameters: make(chan int)}}}},
	} {
		if got, err := ReservationTokens(request, "deepseek-flash"); err == nil || got != 0 {
			t.Fatalf("invalid reservation accepted: tokens=%d err=%v", got, err)
		}
	}
}
