package digest

import (
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/papers"
)

func TestRenderEmptyDigest(t *testing.T) {
	date := time.Date(2026, time.September, 27, 15, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	got := Render(date, nil)
	if !strings.Contains(got, "# arXiv 论文日报 · 2026-09-27") || !strings.Contains(got, "今日未发现符合筛选条件的论文。") {
		t.Errorf("Render() = %q", got)
	}
}

func TestRenderIncludesTraceablePaperMetadata(t *testing.T) {
	published := time.Date(2026, time.September, 26, 10, 0, 0, 0, time.UTC)
	got := Render(time.Date(2026, time.September, 27, 0, 0, 0, 0, time.UTC), []Item{{
		Paper: papers.Paper{
			ID:        "arxiv:2609.12345",
			Version:   "v2",
			Title:     "A \n title with # marker",
			Authors:   []string{"Alice", "Bob"},
			Published: published,
			Updated:   published.Add(24 * time.Hour),
			URL:       "https://arxiv.org/abs/2609.12345v2",
		},
		Summary: Summary{
			Text:          "Key finding: \n- result",
			Model:         "claude-opus-5",
			PromptVersion: "arxiv-summary-v1",
		},
	}})

	for _, want := range []string{
		"## 1. A title with \\# marker",
		"[arxiv:2609.12345v2](https://arxiv.org/abs/2609.12345v2)",
		"基于论文公开摘要整理，非全文解读。",
		"作者：Alice、Bob",
		"发布：2026-09-26",
		"更新：2026-09-27",
		"摘要生成：claude-opus-5（提示版本：arxiv-summary-v1）",
		"Key finding:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Render() missing %q in:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("Render() should end with a newline")
	}
}

func TestRenderBuildsFallbackVersionURL(t *testing.T) {
	got := Render(time.Now(), []Item{{
		Paper:   papers.Paper{ID: "arxiv:hep-th/9901001", Version: "v3"},
		Summary: Summary{Text: "summary"},
	}})
	if !strings.Contains(got, "https://arxiv.org/abs/hep-th/9901001v3") {
		t.Errorf("Render() missing fallback arXiv URL:\n%s", got)
	}
}

func TestRenderFormatsStoredPlainLabels(t *testing.T) {
	got := Render(time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), []Item{{Paper: papers.Paper{ID: "arxiv:2610.01767", Title: "MatRAG"}, Summary: Summary{Text: "- 研究问题：多跳问答\n\n- 方法：分层检索"}}})
	if !strings.Contains(got, "- **研究问题**：多跳问答") || !strings.Contains(got, "- **方法**：分层检索") {
		t.Fatal(got)
	}
}
