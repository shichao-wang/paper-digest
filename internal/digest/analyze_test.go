package digest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
		Model: "claude-test-model",
		request: func(_ context.Context, model, prompt string) (string, error) {
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
		request: func(_ context.Context, model, _ string) (string, error) {
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

func TestClaudeAnalyzerWrapsRequestError(t *testing.T) {
	wantErr := errors.New("temporary API failure")
	analyzer := ClaudeAnalyzer{
		request: func(context.Context, string, string) (string, error) {
			return "", wantErr
		},
	}
	_, err := analyzer.Analyze(context.Background(), papers.Paper{ID: "2609.12345"})
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "2609.12345") {
		t.Fatalf("Analyze() error = %v, want wrapped request error", err)
	}
}
