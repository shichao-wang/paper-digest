package digest

import (
	"fmt"
	"strings"
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
	// ScreenAttempts is how many times one screening batch may call the model.
	// Attempts run one after another, each with its own timeout.
	ScreenAttempts = 3
	// SummaryAttempts is 1: each summary is a single request and is not retried
	// inside the same generate pass.
	SummaryAttempts = 1

	minMaxTokens      = 4096
	maxMaxTokens      = 65536
	minTimeout        = 30 * time.Second
	maxTimeout        = 10 * time.Minute
	maxConcurrency    = 4
	maxBatchSize      = 16
	minThinkingBudget = 1024
)

// CallInput is the raw per-stage agent config. Nil numeric fields mean “use the default”.
// A non-nil value, including 0, is explicit and must fall inside the supported range.
type CallInput struct {
	Model          string
	Skill          string
	MaxTokens      *int
	TimeoutSeconds *int
	BatchSize      *int
	Concurrency    *int
	BudgetTokens   *int
}

// CallSettings is one model call's output cap, timeout, and parallelism.
// Zero values are not used after Normalize; omitted config fields become the defaults.
type CallSettings struct {
	Model          string
	Skill          string
	MaxTokens      int
	Timeout        time.Duration
	BatchSize      int
	Concurrency    int
	ThinkingBudget int
}

func DefaultScreeningCall() CallSettings {
	return CallSettings{
		Skill:          "paper-screening",
		MaxTokens:      DefaultScreenMaxTokens,
		Timeout:        DefaultScreenTimeout,
		BatchSize:      DefaultScreenBatchSize,
		Concurrency:    DefaultScreenConcurrency,
		ThinkingBudget: ThinkingBudget,
	}
}

func DefaultSummaryCall() CallSettings {
	return CallSettings{
		Skill:          "paper-summary",
		MaxTokens:      DefaultSummaryMaxTokens,
		Timeout:        DefaultSummaryTimeout,
		Concurrency:    DefaultSummaryConcurrency,
		ThinkingBudget: ThinkingBudget,
	}
}

// NormalizeScreen fills omitted screening fields and rejects values outside the supported range.
// name is the config path used in errors, such as "agents.screening".
func NormalizeScreen(in CallInput, name string) (CallSettings, error) {
	out := DefaultScreeningCall()
	if err := applyCall(&out, in, name); err != nil {
		return CallSettings{}, err
	}
	if in.BatchSize != nil {
		if *in.BatchSize < 1 || *in.BatchSize > maxBatchSize {
			return CallSettings{}, fmt.Errorf("%s.batch_size 必须在 1 到 %d 之间", name, maxBatchSize)
		}
		out.BatchSize = *in.BatchSize
	}
	if err := applyThinking(&out, in.BudgetTokens, name); err != nil {
		return CallSettings{}, err
	}
	return out, nil
}

// NormalizeSummary fills omitted summary fields. Summaries are one paper per call, so there is no batch size.
func NormalizeSummary(in CallInput, name string) (CallSettings, error) {
	out := DefaultSummaryCall()
	if err := applyCall(&out, in, name); err != nil {
		return CallSettings{}, err
	}
	if err := applyThinking(&out, in.BudgetTokens, name); err != nil {
		return CallSettings{}, err
	}
	return out, nil
}

func applyCall(out *CallSettings, in CallInput, name string) error {
	out.Model = strings.TrimSpace(in.Model)
	if skill := strings.TrimSpace(in.Skill); skill != "" {
		out.Skill = skill
	}
	if in.MaxTokens != nil {
		if *in.MaxTokens < minMaxTokens || *in.MaxTokens > maxMaxTokens {
			return fmt.Errorf("%s.max_tokens 必须在 %d 到 %d 之间", name, minMaxTokens, maxMaxTokens)
		}
		out.MaxTokens = *in.MaxTokens
	}
	if in.TimeoutSeconds != nil {
		timeout := time.Duration(*in.TimeoutSeconds) * time.Second
		if timeout < minTimeout || timeout > maxTimeout {
			return fmt.Errorf("%s.timeout_seconds 必须在 %d 到 %d 之间", name, int(minTimeout/time.Second), int(maxTimeout/time.Second))
		}
		out.Timeout = timeout
	}
	if in.Concurrency != nil {
		if *in.Concurrency < 1 || *in.Concurrency > maxConcurrency {
			return fmt.Errorf("%s.concurrency 必须在 1 到 %d 之间", name, maxConcurrency)
		}
		out.Concurrency = *in.Concurrency
	}
	return nil
}

func applyThinking(out *CallSettings, budget *int, name string) error {
	value := ThinkingBudget
	if budget != nil {
		value = *budget
	}
	if value < minThinkingBudget || value >= out.MaxTokens {
		return fmt.Errorf("%s.thinking.budget_tokens 必须在 %d 到 %d 之间，并且小于 max_tokens（当前 %d）。DeepSeek 会忽略这个预算，思考计入 max_tokens", name, minThinkingBudget, out.MaxTokens-1, out.MaxTokens)
	}
	out.ThinkingBudget = value
	return nil
}

// PhaseDuration is the worst case for every batch to use every attempt.
// Attempts on one batch run one after another. Batches in one wave run
// together, so a wave takes attempts*timeout. Summaries pass SummaryAttempts
// because each paper is a single request. attempts below 1 counts as one pass.
// batchSize is 1 for per-paper summaries.
func PhaseDuration(items, batchSize, concurrency, attempts int, timeout time.Duration) time.Duration {
	if items <= 0 || timeout <= 0 {
		return 0
	}
	if batchSize < 1 {
		batchSize = 1
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if attempts < 1 {
		attempts = 1
	}
	batches := (items + batchSize - 1) / batchSize
	waves := (batches + concurrency - 1) / concurrency
	return time.Duration(waves*attempts) * timeout
}
