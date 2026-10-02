package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/shichao-wang/paper-digest/internal/library"

	"github.com/shichao-wang/paper-digest/internal/modelchat"
)

func localCall(name string, args any) modelchat.ToolCall {
	raw, _ := json.Marshal(args)
	var c modelchat.ToolCall
	c.Function.Name = name
	c.Function.Arguments = string(raw)
	return c
}
func TestLocalVerificationToolsStayInsideBlock(t *testing.T) {
	d := document(1, "Other page prefix. ", "中文 table\nTable 1 Ranking\nMethod | NDCG\nA | 0.9\n")
	b := d.Blocks[1]
	tests := []struct {
		name  string
		args  any
		valid bool
	}{
		{"search", searchArgs{d.ID, b.ID, "NDCG"}, true},
		{"search", searchArgs{"other", b.ID, "NDCG"}, false},
		{"search", map[string]any{"document_id": d.ID, "block_id": b.ID, "query": "NDCG", "shell": "cat"}, false},
		{"read_range", rangeArgs{d.ID, b.ID, b.Start, b.Start + len("中文")}, true},
		{"read_range", rangeArgs{d.ID, b.ID, b.Start, b.Start + 1}, false},
		{"read_range", rangeArgs{d.ID, b.ID, 0, b.End}, false},
		{"table", readArgs{d.ID, b.ID}, true},
		{"table", readArgs{d.ID, d.Blocks[0].ID}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, read := executeLocal(d, b, localCall(tt.name, tt.args))
			raw, _ := json.Marshal(value)
			var object map[string]any
			_ = json.Unmarshal(raw, &object)
			_, failed := object["error"]
			if failed == tt.valid || read {
				t.Fatalf("scope/read incorrect: %s fullRead=%v", raw, read)
			}
		})
	}
	value, read := executeLocal(d, b, localCall("read_block", readArgs{d.ID, b.ID}))
	if !read {
		t.Fatal("full read not marked")
	}
	raw, _ := json.Marshal(value)
	var object map[string]any
	_ = json.Unmarshal(raw, &object)
	if object["text"] != b.Text {
		t.Fatal("read_block omitted original text")
	}
	definitions := localDefinitions(d, b)
	if len(definitions) != 4 {
		t.Fatal("missing local tool")
	}
	for _, def := range definitions {
		raw, _ := json.Marshal(def.Function.Parameters)
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		if obj["additionalProperties"] != false {
			t.Fatal("tool accepts arbitrary arguments")
		}
	}
}

func TestCriticalExtractionContextBlocksQuality(t *testing.T) {
	d := document(1, "Search ranking method.")
	c := chunkFor(d, d.Blocks[0])
	g := grounding{docs: map[string]library.Document{d.ID: d}, read: map[string]bool{blockKey(d, d.Blocks[0]): true}}
	for _, field := range []string{"table_context", "formula_context", "critical_table_context", "critical_formula_context"} {
		content := analysisFor([]library.Chunk{c})
		content.MissingFields = append(content.MissingFields, field)
		if !errors.Is(g.analysis(content), library.ErrQuality) {
			t.Fatalf("critical %s accepted", field)
		}
		if !errors.Is(g.chunk(chunkContent{Notes: c.Notes, Evidence: c.Evidence, MissingFields: []string{field}}), library.ErrQuality) {
			t.Fatalf("critical chunk %s accepted", field)
		}
	}
	content := analysisFor([]library.Chunk{c})
	content.MissingFields = append(content.MissingFields, "limitations")
	if err := g.analysis(content); err != nil {
		t.Fatalf("ordinary missing information blocked: %v", err)
	}
	calls := 0
	e := fakeEngine(t, func(w wireRequest) (*http.Response, error) {
		calls++
		content.MissingFields = append(content.MissingFields, "critical_table_context")
		return response(content, nil), nil
	})
	var cp Checkpoint
	_, _, err := e.Analyze(context.Background(), version(1), d, []library.Chunk{c}, cp, saver(&cp), func(library.Chunk) error { return nil })
	if !errors.Is(err, library.ErrQuality) || calls != 1 {
		t.Fatalf("quality incorrectly repaired away: %v calls=%d", err, calls)
	}
}

func TestCheckpointMalformedJSONCannotResetBudget(t *testing.T) {
	e := fakeEngine(t, func(wireRequest) (*http.Response, error) { t.Fatal("malformed checkpoint sent HTTP"); return nil, nil })
	cp := Checkpoint{Run: library.Run{Requests: 100}, Sessions: map[string]SessionCheckpoint{"bad": {History: []json.RawMessage{json.RawMessage(`{`)}}}}
	_, _, err := e.Screen(context.Background(), version(1), cp, func(Checkpoint) error { return nil })
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid checkpoint reset: %v", err)
	}
}
