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
	screen, err := NormalizeScreen(0, 0, 0, 0)
	if err != nil || screen != DefaultScreeningCall() {
		t.Fatalf("defaults = %+v, %v", screen, err)
	}
	summary, err := NormalizeSummary(0, 0, 0)
	if err != nil || summary != DefaultSummaryCall() {
		t.Fatalf("summary defaults = %+v, %v", summary, err)
	}
	if _, err := NormalizeScreen(1024, 0, 0, 0); err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("small max_tokens error = %v", err)
	}
	if _, err := NormalizeSummary(0, 1, 0); err == nil {
		t.Fatal("one-second timeout was accepted")
	}
	if _, err := NormalizeScreen(0, 0, 0, 8); err == nil {
		t.Fatal("unbounded concurrency was accepted")
	}
	custom, err := NormalizeScreen(32768, 240, 4, 3)
	if err != nil || custom.MaxTokens != 32768 || custom.Timeout != 240*time.Second || custom.BatchSize != 4 || custom.Concurrency != 3 {
		t.Fatalf("custom = %+v, %v", custom, err)
	}
}
