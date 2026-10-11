package digest

import (
	"strings"
	"testing"
	"time"
)

func TestPhaseDurationCountsEveryScreeningAttempt(t *testing.T) {
	// 30 papers, batches of 8, concurrency 2: 4 batches and 2 waves.
	// Each batch may use all 3 attempts, so a wave is 9 minutes and the phase is 18.
	got := PhaseDuration(30, 8, 2, ScreenAttempts, 180*time.Second)
	if got != 18*time.Minute {
		t.Fatalf("screening = %s, want 18m", got)
	}
	// Summaries are one request each: 5 papers at concurrency 2 is 3 waves, 6 minutes.
	if PhaseDuration(5, 1, 2, SummaryAttempts, 120*time.Second) != 6*time.Minute {
		t.Fatal("summaries")
	}
	if PhaseDuration(0, 8, 2, ScreenAttempts, time.Minute) != 0 {
		t.Fatal("empty work should take no time")
	}
	if PhaseDuration(8, 8, 2, 0, time.Minute) != time.Minute {
		t.Fatal("attempts below 1 should count as one pass")
	}
}

func TestNormalizeRejectsLimitsOutsideTheSupportedRange(t *testing.T) {
	screen, err := NormalizeScreen(CallInput{}, "agents.screening")
	if err != nil || screen != DefaultScreeningCall() {
		t.Fatalf("defaults = %+v, %v", screen, err)
	}
	summary, err := NormalizeSummary(CallInput{}, "agents.summary")
	if err != nil || summary != DefaultSummaryCall() {
		t.Fatalf("summary defaults = %+v, %v", summary, err)
	}
	if _, err := NormalizeScreen(CallInput{MaxTokens: pint(1024)}, "agents.screening"); err == nil || !strings.Contains(err.Error(), "agents.screening.max_tokens") {
		t.Fatalf("small max_tokens error = %v", err)
	}
	if _, err := NormalizeScreen(CallInput{MaxTokens: pint(0)}, "agents.screening"); err == nil || !strings.Contains(err.Error(), "agents.screening.max_tokens") {
		t.Fatalf("explicit zero max_tokens error = %v", err)
	}
	if _, err := NormalizeSummary(CallInput{TimeoutSeconds: pint(1)}, "agents.summary"); err == nil {
		t.Fatal("one-second timeout was accepted")
	}
	if _, err := NormalizeSummary(CallInput{TimeoutSeconds: pint(0)}, "agents.summary"); err == nil || !strings.Contains(err.Error(), "agents.summary.timeout_seconds") {
		t.Fatalf("explicit zero timeout error = %v", err)
	}
	if _, err := NormalizeScreen(CallInput{Concurrency: pint(8)}, "agents.screening"); err == nil {
		t.Fatal("unbounded concurrency was accepted")
	}
	if _, err := NormalizeScreen(CallInput{BudgetTokens: pint(16384)}, "agents.screening"); err == nil || !strings.Contains(err.Error(), "budget_tokens") {
		t.Fatalf("budget above max_tokens error = %v", err)
	}
	if _, err := NormalizeScreen(CallInput{BudgetTokens: pint(0)}, "agents.screening"); err == nil || !strings.Contains(err.Error(), "agents.screening.thinking.budget_tokens") {
		t.Fatalf("explicit zero budget error = %v", err)
	}
	custom, err := NormalizeScreen(CallInput{Model: " other-model ", Skill: "custom-screen", MaxTokens: pint(32768), TimeoutSeconds: pint(240), BatchSize: pint(4), Concurrency: pint(3), BudgetTokens: pint(4096)}, "agents.screening")
	if err != nil || custom.Model != "other-model" || custom.Skill != "custom-screen" || custom.MaxTokens != 32768 || custom.Timeout != 240*time.Second || custom.BatchSize != 4 || custom.Concurrency != 3 || custom.ThinkingBudget != 4096 {
		t.Fatalf("custom = %+v, %v", custom, err)
	}
}

func pint(v int) *int { return &v }
