package digest

import (
	"strings"
	"testing"
	"time"
)

func TestPhaseDurationLeavesRoomForOneSpareAttempt(t *testing.T) {
	got := PhaseDuration(30, 8, 2, 180*time.Second)
	if got != 540*time.Second {
		t.Fatalf("30 papers = %s, want 9m", got)
	}
	if PhaseDuration(5, 1, 2, 120*time.Second) != 480*time.Second {
		t.Fatal("summaries")
	}
	if PhaseDuration(0, 8, 2, time.Minute) != 0 {
		t.Fatal("empty work should take no time")
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
	if _, err := NormalizeScreen(CallInput{MaxTokens: 1024}, "agents.screening"); err == nil || !strings.Contains(err.Error(), "agents.screening.max_tokens") {
		t.Fatalf("small max_tokens error = %v", err)
	}
	if _, err := NormalizeSummary(CallInput{TimeoutSeconds: 1}, "agents.summary"); err == nil {
		t.Fatal("one-second timeout was accepted")
	}
	if _, err := NormalizeScreen(CallInput{Concurrency: 8}, "agents.screening"); err == nil {
		t.Fatal("unbounded concurrency was accepted")
	}
	if _, err := NormalizeScreen(CallInput{BudgetTokens: 16384}, "agents.screening"); err == nil || !strings.Contains(err.Error(), "budget_tokens") {
		t.Fatalf("budget above max_tokens error = %v", err)
	}
	custom, err := NormalizeScreen(CallInput{Model: " other-model ", Skill: "custom-screen", MaxTokens: 32768, TimeoutSeconds: 240, BatchSize: 4, Concurrency: 3, BudgetTokens: 4096}, "agents.screening")
	if err != nil || custom.Model != "other-model" || custom.Skill != "custom-screen" || custom.MaxTokens != 32768 || custom.Timeout != 240*time.Second || custom.BatchSize != 4 || custom.Concurrency != 3 || custom.ThinkingBudget != 4096 {
		t.Fatalf("custom = %+v, %v", custom, err)
	}
}
