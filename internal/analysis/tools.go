package analysis

import (
	"strings"
	"unicode/utf8"

	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/modelchat"
)

type searchArgs struct {
	DocumentID string `json:"document_id"`
	BlockID    string `json:"block_id"`
	Query      string `json:"query"`
}
type rangeArgs struct {
	DocumentID string `json:"document_id"`
	BlockID    string `json:"block_id"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
}

func localDefinitions(d library.Document, b library.Block) []modelchat.ToolDefinition {
	makeDef := func(name, description string, extra map[string]any, required ...string) modelchat.ToolDefinition {
		props := map[string]any{"document_id": map[string]any{"type": "string", "enum": []string{d.ID}}, "block_id": map[string]any{"type": "string", "enum": []string{b.ID}}}
		for key, value := range extra {
			props[key] = value
		}
		return modelchat.ToolDefinition{Type: "function", Function: modelchat.Function{Name: name, Description: description, Parameters: map[string]any{"type": "object", "additionalProperties": false, "required": append([]string{"document_id", "block_id"}, required...), "properties": props}}}
	}
	return []modelchat.ToolDefinition{
		readDefinition(d, b),
		makeDef("search", "Search a literal string within this fixed block only; never substitutes for reading the full block", map[string]any{"query": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}}, "query"),
		makeDef("read_range", "Read exact page UTF-8 byte offsets wholly inside this fixed block; never substitutes for read_block", map[string]any{"start": map[string]any{"type": "integer", "minimum": b.Start, "maximum": b.End - 1}, "end": map[string]any{"type": "integer", "minimum": b.Start + 1, "maximum": b.End}}, "start", "end"),
		makeDef("table", "Inspect table-like rows and caption context inside this fixed block; no inferred table semantics", nil),
	}
}
func executeLocal(d library.Document, b library.Block, call modelchat.ToolCall) (any, bool) {
	invalid := map[string]string{"error": "invalid tool or fixed block arguments"}
	bind := func(doc, block string) bool { return doc == d.ID && block == b.ID }
	result := func(start, end int) map[string]any {
		return map[string]any{"document_id": d.ID, "version": d.Version, "block_id": b.ID, "page": b.Page, "start": start, "end": end, "text": b.Text[start-b.Start : end-b.Start], "block_sha256": b.SHA256, "document_sha256": d.Text.SHA256}
	}
	switch call.Function.Name {
	case "read_block":
		var args readArgs
		if decode([]byte(call.Function.Arguments), &args) != nil || !bind(args.DocumentID, args.BlockID) {
			return invalid, false
		}
		return result(b.Start, b.End), true
	case "read_range":
		var args rangeArgs
		if decode([]byte(call.Function.Arguments), &args) != nil || !bind(args.DocumentID, args.BlockID) || args.Start < b.Start || args.End > b.End || args.End <= args.Start || !utf8.ValidString(b.Text[args.Start-b.Start:args.End-b.Start]) {
			return invalid, false
		}
		return result(args.Start, args.End), false
	case "search":
		var args searchArgs
		if decode([]byte(call.Function.Arguments), &args) != nil || !bind(args.DocumentID, args.BlockID) || len(args.Query) == 0 || len(args.Query) > 256 || !utf8.ValidString(args.Query) {
			return invalid, false
		}
		matches := []map[string]any{}
		for at := 0; at < len(b.Text) && len(matches) < 50; {
			index := strings.Index(b.Text[at:], args.Query)
			if index < 0 {
				break
			}
			index += at
			matches = append(matches, result(b.Start+index, b.Start+index+len(args.Query)))
			at = index + len(args.Query)
		}
		return map[string]any{"document_id": d.ID, "block_id": b.ID, "matches": matches, "limit": 50}, false
	case "table":
		var args readArgs
		if decode([]byte(call.Function.Arguments), &args) != nil || !bind(args.DocumentID, args.BlockID) {
			return invalid, false
		}
		// 仅检测原文行形态，返回连续原文上下文；不生成列标签或解释。
		lines := strings.SplitAfter(b.Text, "\n")
		at := b.Start
		rows := []map[string]any{}
		for _, line := range lines {
			lower := strings.ToLower(strings.TrimSpace(line))
			if strings.Contains(line, "|") || strings.Contains(line, "\t") || strings.Contains(strings.TrimSpace(line), "  ") || strings.HasPrefix(lower, "table ") {
				rows = append(rows, result(at, at+len(line)))
			}
			at += len(line)
		}
		return map[string]any{"document_id": d.ID, "block_id": b.ID, "rows": rows, "note": "literal table-like lines only; use read_range for surrounding caption/header context"}, false
	}
	return invalid, false
}
