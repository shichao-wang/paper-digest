// verify-chat 仅使用虚构证据验证 DeepSeek 官方 Chat 协议。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/modelchat"
)

const maxRequests = 30

type settings struct {
	Anthropic struct {
		APIKey  string `json:"api_key"`
		BaseURL string `json:"base_url"`
		Model   string `json:"model"`
	} `json:"anthropic"`
}
type result struct {
	Name             string                `json:"name"`
	Status           string                `json:"status"`
	Detail           string                `json:"detail"`
	Requests         int                   `json:"requests"`
	DurationMS       int64                 `json:"duration_ms"`
	Turns            []modelchat.Turn      `json:"turns"`
	InvalidObserved  bool                  `json:"invalid_observed,omitempty"`
	RepairedObserved bool                  `json:"repaired_observed,omitempty"`
	InputBytes       int                   `json:"input_bytes,omitempty"`
	Calls            []modelchat.CallEvent `json:"calls,omitempty"`
	ValidationErrors []string              `json:"validation_errors,omitempty"`
	RelevanceLabels  map[string]bool       `json:"relevance_labels,omitempty"`
}
type report struct {
	CheckedAt                       string   `json:"checked_at"`
	Model                           string   `json:"model"`
	Endpoint                        string   `json:"endpoint"`
	Synthetic                       bool     `json:"synthetic_input"`
	MaxRequests                     int      `json:"max_requests"`
	Requests                        int      `json:"requests"`
	MaxOutputTokensPerRequest       int      `json:"max_output_tokens_per_request"`
	AggregateTokenBudgetImplemented bool     `json:"aggregate_token_budget_implemented"`
	RequestTimeoutSeconds           int      `json:"request_timeout_seconds"`
	CaseTimeoutSeconds              int      `json:"case_timeout_seconds"`
	MaxToolStageRequests            int      `json:"max_tool_stage_requests"`
	MaxFinalRequests                int      `json:"max_final_requests"`
	MaxRepairRequests               int      `json:"max_repair_requests"`
	Scope                           string   `json:"scope"`
	Results                         []result `json:"results"`
}

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("verify-chat", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config/config.json", "现有 JSON 配置；- 从 stdin 读取")
	out := flags.String("out", "data/validation/chat.json", "0600 报告路径")
	live := flags.Bool("live", false, "允许官方 Chat 真实请求；全套硬预算30次，无重试")
	only := flags.String("only", "", "固定 case 名称，逗号分隔；空为全套")
	limit := flags.Int("request-limit", maxRequests, "共享请求硬预算，1..30")
	requestTimeout := flags.Int("request-timeout-seconds", 60, "每请求 timeout，1..180 秒")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return 2
	}
	if !*live {
		fmt.Fprintln(stderr, "真实请求必须显式 --live；离线验证使用 go test ./internal/modelchat ./cmd/verify-chat")
		return 2
	}
	if *requestTimeout < 1 || *requestTimeout > 180 {
		fmt.Fprintln(stderr, "request-timeout-seconds must be between 1 and 180")
		return 2
	}
	selection, err := selectCases(*only, *limit)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var reader io.Reader = stdin
	var file *os.File
	if *configPath != "-" {
		var err error
		file, err = os.Open(*configPath)
		if err != nil {
			fmt.Fprintln(stderr, "无法读取模型配置")
			return 2
		}
		defer file.Close()
		reader = file
	}
	cfg, err := loadSettings(reader)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	budget := modelchat.NewBudget(*limit)
	client, err := modelchat.NewClient(modelchat.Options{APIKey: cfg.Anthropic.APIKey, BaseURL: cfg.Anthropic.BaseURL, Model: cfg.Anthropic.Model, Timeout: time.Duration(*requestTimeout) * time.Second, Budget: budget})
	if err != nil {
		fmt.Fprintln(stderr, "无法初始化 Chat 客户端")
		return 2
	}
	r := runSelectedChecks(context.Background(), client, budget, selection, *limit, func(res result) {
		fmt.Fprintf(stdout, "%s: %s requests=%d\n", res.Name, res.Status, res.Requests)
		if f, ok := stdout.(interface{ Flush() error }); ok {
			_ = f.Flush()
		}
	})
	r.RequestTimeoutSeconds = *requestTimeout
	r.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	r.Model = cfg.Anthropic.Model
	r.Endpoint = "https://api.deepseek.com/v1/chat/completions"
	r.Synthetic = true
	if saveReport(*out, r) != nil {
		fmt.Fprintln(stderr, "无法保存验证报告")
		return 2
	}
	fmt.Fprintf(stdout, "报告已保存；实际请求 %d/%d\n", r.Requests, r.MaxRequests)
	code := 0
	for _, res := range r.Results {
		fmt.Fprintf(stdout, "%s: %s — %s\n", res.Name, res.Status, res.Detail)
		if res.Status != "passed" {
			code = 1
		}
	}
	return code
}
func loadSettings(reader io.Reader) (settings, error) {
	var cfg settings
	data, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || modelchat.UniqueJSON(data) != nil || json.Unmarshal(data, &cfg) != nil {
		return cfg, errors.New("模型配置 JSON 不合法")
	}
	if cfg.Anthropic.APIKey == "" || cfg.Anthropic.Model != "deepseek-flash" {
		return settings{}, errors.New("需要显式 api_key 和现有 deepseek-flash model")
	}
	if cfg.Anthropic.BaseURL != "https://api.deepseek.com/anthropic" && cfg.Anthropic.BaseURL != "https://api.deepseek.com/v1" {
		return settings{}, errors.New("仅支持 DeepSeek 官方 anthropic 或 v1 base")
	}
	return cfg, nil
}
func saveReport(path string, r report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return errors.New("report encode failed")
	}
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return errors.New("report directory failed")
	}
	// 拒绝符号链接；已有普通文件同样收紧为 0600。
	if stat, err := os.Lstat(path); err == nil && !stat.Mode().IsRegular() {
		return errors.New("report target must be regular file")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return errors.New("report open failed")
	}
	defer f.Close()
	if f.Chmod(0600) != nil {
		return errors.New("report permissions failed")
	}
	if _, err = f.Write(append(data, '\n')); err != nil {
		return errors.New("report write failed")
	}
	return f.Close()
}

func runChecks(ctx context.Context, client *modelchat.Client, budget *modelchat.Budget) report {
	return runSelectedChecks(ctx, client, budget, caseNames, maxRequests, nil)
}
func selectCases(only string, limit int) ([]string, error) {
	if limit < 1 || limit > maxRequests {
		return nil, errors.New("request-limit must be between 1 and 30")
	}
	if only == "" {
		return append([]string(nil), caseNames...), nil
	}
	known := map[string]bool{}
	for _, name := range caseNames {
		known[name] = true
	}
	selection := strings.Split(only, ",")
	seen := map[string]bool{}
	for _, name := range selection {
		if !known[name] || seen[name] {
			return nil, errors.New("only must contain distinct fixed case names")
		}
		seen[name] = true
	}
	return selection, nil
}
func runSelectedChecks(ctx context.Context, client *modelchat.Client, budget *modelchat.Budget, selection []string, limit int, onResult func(result)) report {
	r := report{MaxRequests: limit, MaxOutputTokensPerRequest: 2048, RequestTimeoutSeconds: 60, CaseTimeoutSeconds: 180, MaxToolStageRequests: 4, MaxFinalRequests: 1, MaxRepairRequests: 2, Scope: "synthetic Chat protocol only; real PDF and full PaperAnalysis not validated", Results: []result{}}
	for _, name := range selection {
		before := budget.Used()
		start := time.Now()
		res := result{Name: name, Status: "failed", Turns: []modelchat.Turn{}}
		if before >= limit {
			res.Status = "inconclusive"
			res.Detail = "overall request budget exhausted; case not run"
		} else {
			caseCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			err := runCase(caseCtx, client, name, &res)
			cancel()
			if err != nil {
				if errors.Is(err, modelchat.ErrBudget) {
					res.Status = "inconclusive"
					res.Detail = "overall request budget exhausted before case completion"
				} else {
					res.Detail = err.Error()
				}
			} else {
				res.Status = "passed"
				res.Detail = "synthetic protocol and evidence checks passed"
			}
		}
		res.Requests = budget.Used() - before
		res.DurationMS = time.Since(start).Milliseconds()
		r.Results = append(r.Results, res)
		if onResult != nil {
			onResult(res)
		}
	}
	r.Requests = budget.Used()
	return r
}
