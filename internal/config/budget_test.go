package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shichao-wang/paper-digest/internal/analysis"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/modelchat"
)

func TestLibraryTokenBudgetStartupBoundary(t *testing.T) {
	for _, model := range []string{"", "deepseek-flash", "group/deepseek-v4-1-flash", "custom-模型"} {
		t.Run(model, func(t *testing.T) {
			effective := model
			if effective == "" {
				effective = "deepseek-flash"
			}
			minimum, err := analysis.MinimumScreenReservation(effective)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("minimum screening reservation for %q: %d", effective, minimum)
			for _, tokens := range []int64{-1, 4096, 5120, minimum - 1, minimum, minimum + 1, 500000} {
				// Disabled automation can still be processed manually, so it has the same range.
				for _, enabled := range []bool{false, true} {
					text := fmt.Sprintf(`{"library":{"process_enabled":%t,"max_tokens":%d},"anthropic":{"model":%q,"api_key":"private-secret"},"arxiv":{"lookback_days":7},"topics":[{"id":"new-topic"}]}`, enabled, tokens, model)
					cfg, err := loadFixture(t, text)
					if tokens < minimum {
						if err == nil || !strings.Contains(err.Error(), "library.max_tokens") || strings.Contains(err.Error(), "private-secret") || strings.Contains(err.Error(), effective) {
							t.Fatalf("insufficient budget %d accepted or echoed settings: %v", tokens, err)
						}
					} else if err != nil || cfg.Library.MaxTokens != tokens {
						t.Fatalf("valid cumulative budget %d changed or rejected: %+v %v", tokens, cfg.Library, err)
					}
				}
			}
		})
	}
}

func TestLibraryValidateIncludesFixedScreenPrompt(t *testing.T) {
	minimum, err := analysis.MinimumScreenReservation("")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFixture(t, `{"arxiv":{"lookback_days":7},"topics":[{"id":"new-topic"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Library.MaxTokens = minimum - 1
	if err := cfg.Library.Validate(); err == nil || !strings.Contains(err.Error(), "library.max_tokens") {
		t.Fatalf("fixed prompt budget accepted: %v", err)
	}
	cfg.Library.MaxTokens = minimum
	if err := cfg.Library.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"", "deepseek-flash", "group/" + strings.Repeat("x", 100)} {
		cfg.Anthropic.Model = model
		cfg.Library.MaxTokens = minimum
		if err := cfg.ValidateLibrary(); err == nil {
			t.Fatalf("direct configuration ignored model overhead for %q", model)
		}
		effective := model
		if effective == "" {
			effective = "deepseek-flash"
		}
		cfg.Library.MaxTokens, err = analysis.MinimumScreenReservation(effective)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.ValidateLibrary(); err != nil {
			t.Fatalf("direct minimum configuration rejected: %v", err)
		}
	}
	// Load also accounts for the fallback model, which Library alone cannot know.
	_, err = loadFixture(t, fmt.Sprintf(`{"library":{"max_tokens":%d},"arxiv":{"lookback_days":7},"topics":[{"id":"new-topic"}]}`, minimum))
	if err == nil {
		t.Fatal("fallback model overhead ignored at startup")
	}
}

func TestMinimumLoadedBudgetFundsRealScreenRequest(t *testing.T) {
	const model = "deepseek-flash"
	minimum, err := analysis.MinimumScreenReservation(model)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, abstract, content string
		wantRequests            int
		wantPause               bool
	}{
		{"minimal-screen", "", `{"topics":[],"directly_related":false,"relevance_level":"uncertain","rationale":"No metadata available","extracted_keywords":[],"evidence_ids":[]}`, 1, false},
		{"longer-metadata", strings.Repeat("研究", 100), `{}`, 0, true},
		{"repair-keeps-cumulative-reservation", "", `{}`, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var cp analysis.Checkpoint
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if cp.Run.Requests != 1 || cp.Run.ReservedTokens != minimum || cp.MaxTokens != minimum {
					t.Error("HTTP sent without exact durable initial reservation")
				}
				var wire struct {
					MaxTokens int               `json:"max_tokens"`
					Messages  []json.RawMessage `json:"messages"`
				}
				if json.NewDecoder(r.Body).Decode(&wire) != nil || wire.MaxTokens != 4096 || len(wire.Messages) != 2 {
					t.Error("unexpected initial screening request")
				}
				// Missing usage deliberately keeps the full reservation, including on repair.
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": tc.content}, "finish_reason": "stop"}}})
			}))
			defer server.Close()
			cfg, err := loadFixture(t, fmt.Sprintf(`{"library":{"max_requests":20,"max_tokens":%d},"anthropic":{"model":%q,"base_url":%q},"arxiv":{"lookback_days":7},"topics":[{"id":"new-topic"}]}`, minimum, model, server.URL))
			if err != nil {
				t.Fatal(err)
			}
			e := analysis.Engine{Model: cfg.Anthropic.Model, MaxRequests: cfg.Library.MaxRequests, MaxTokens: cfg.Library.MaxTokens, NewClient: func(b *modelchat.Budget) (*modelchat.Client, error) {
				return modelchat.NewClient(modelchat.Options{APIKey: "offline-only", Model: model, BaseURL: server.URL, Budget: b})
			}}
			v := library.Version{Identity: library.Identity{Source: "arxiv", PaperID: "2610.00001", Version: "v1"}, Abstract: tc.abstract, Categories: []string{}, AuthorKeywords: []string{}}
			save := func(next analysis.Checkpoint) error { cp = next; return nil }
			_, run, err := e.Screen(context.Background(), v, analysis.Checkpoint{}, save)
			if calls != tc.wantRequests || run.Requests != tc.wantRequests || errors.Is(err, library.ErrPaused) != tc.wantPause || (!tc.wantPause && err != nil) {
				t.Fatalf("requests=%d run=%+v err=%v", calls, run, err)
			}
			if tc.wantRequests == 1 && run.ReservedTokens != minimum {
				t.Fatalf("unknown usage released reservation: %+v", run)
			}
			if tc.name == "repair-keeps-cumulative-reservation" {
				e.MaxTokens = minimum * 10
				_, run, err = e.Screen(context.Background(), v, cp, save)
				if !errors.Is(err, library.ErrPaused) || calls != 1 || run.ReservedTokens != minimum || cp.MaxTokens != minimum {
					t.Fatalf("restart replenished cumulative budget: calls=%d run=%+v err=%v", calls, run, err)
				}
			}
		})
	}
}
