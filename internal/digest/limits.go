package digest

import (
	"fmt"
	"time"
)

const (
	// ThinkingBudget is sent as thinking.budget_tokens. DeepSeek's
	// Anthropic-compatible API accepts the field and ignores it, so reasoning
	// is counted against max_tokens instead. The budget stays below every
	// allowed max_tokens so Anthropic itself still accepts the request.
	ThinkingBudget = 2048

	DefaultScreenMaxTokens    = 16384
	DefaultSummaryMaxTokens   = 8192
	DefaultScreenTimeout      = 180 * time.Second
	DefaultSummaryTimeout     = 120 * time.Second
	DefaultScreenBatchSize    = 8
	DefaultScreenConcurrency  = 2
	DefaultSummaryConcurrency = 2
	ScreenAttempts            = 3

	minMaxTokens   = 4096
	maxMaxTokens   = 65536
	minTimeout     = 30 * time.Second
	maxTimeout     = 10 * time.Minute
	maxConcurrency = 4
	maxBatchSize   = 16
)

// CallSettings is one model call's output cap, timeout, and parallelism.
// Zero values are not used after Normalize; omitted config fields become the defaults.
type CallSettings struct {
	MaxTokens   int
	Timeout     time.Duration
	BatchSize   int
	Concurrency int
}

func DefaultScreeningCall() CallSettings {
	return CallSettings{
		MaxTokens:   DefaultScreenMaxTokens,
		Timeout:     DefaultScreenTimeout,
		BatchSize:   DefaultScreenBatchSize,
		Concurrency: DefaultScreenConcurrency,
	}
}

func DefaultSummaryCall() CallSettings {
	return CallSettings{
		MaxTokens:   DefaultSummaryMaxTokens,
		Timeout:     DefaultSummaryTimeout,
		Concurrency: DefaultSummaryConcurrency,
	}
}

// NormalizeScreen fills omitted screening fields and rejects values outside the supported range.
func NormalizeScreen(maxTokens, timeoutSeconds, batchSize, concurrency int) (CallSettings, error) {
	out := DefaultScreeningCall()
	if err := applyCall(&out, maxTokens, timeoutSeconds, concurrency, "anthropic.screening"); err != nil {
		return CallSettings{}, err
	}
	if batchSize != 0 {
		if batchSize < 1 || batchSize > maxBatchSize {
			return CallSettings{}, fmt.Errorf("anthropic.screening.batch_size 必须在 1 到 %d 之间", maxBatchSize)
		}
		out.BatchSize = batchSize
	}
	return out, nil
}

// NormalizeSummary fills omitted summary fields. Summaries are one paper per call, so there is no batch size.
func NormalizeSummary(maxTokens, timeoutSeconds, concurrency int) (CallSettings, error) {
	out := DefaultSummaryCall()
	if err := applyCall(&out, maxTokens, timeoutSeconds, concurrency, "anthropic.summary"); err != nil {
		return CallSettings{}, err
	}
	return out, nil
}

func applyCall(out *CallSettings, maxTokens, timeoutSeconds, concurrency int, name string) error {
	if maxTokens != 0 {
		if maxTokens < minMaxTokens || maxTokens > maxMaxTokens {
			return fmt.Errorf("%s.max_tokens 必须在 %d 到 %d 之间", name, minMaxTokens, maxMaxTokens)
		}
		out.MaxTokens = maxTokens
	}
	if timeoutSeconds != 0 {
		timeout := time.Duration(timeoutSeconds) * time.Second
		if timeout < minTimeout || timeout > maxTimeout {
			return fmt.Errorf("%s.timeout_seconds 必须在 %d 到 %d 之间", name, int(minTimeout/time.Second), int(maxTimeout/time.Second))
		}
		out.Timeout = timeout
	}
	if concurrency != 0 {
		if concurrency < 1 || concurrency > maxConcurrency {
			return fmt.Errorf("%s.concurrency 必须在 1 到 %d 之间", name, maxConcurrency)
		}
		out.Concurrency = concurrency
	}
	return nil
}

// PhaseDuration is the time for one pass over items, at the configured timeout,
// plus one spare attempt. batchSize is 1 for per-paper summaries.
func PhaseDuration(items, batchSize, concurrency int, timeout time.Duration) time.Duration {
	if items <= 0 || timeout <= 0 {
		return 0
	}
	if batchSize < 1 {
		batchSize = 1
	}
	if concurrency < 1 {
		concurrency = 1
	}
	batches := (items + batchSize - 1) / batchSize
	waves := (batches + concurrency - 1) / concurrency
	return time.Duration(waves)*timeout + timeout
}
