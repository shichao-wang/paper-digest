// verify-model 使用虚构论文验证模型通道，不打开数据库、不发送消息。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

type settings struct {
	Anthropic struct {
		APIKey  string `json:"api_key"`
		Model   string `json:"model"`
		BaseURL string `json:"base_url"`
	} `json:"anthropic"`
}

type turn struct {
	StopReason   string   `json:"stop_reason"`
	Tools        []string `json:"tools"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	DurationMS   int64    `json:"duration_ms"`
}

type result struct {
	Name   string          `json:"name"`
	Status string          `json:"status"`
	Detail string          `json:"detail"`
	Turns  []turn          `json:"turns"`
	Result json.RawMessage `json:"result,omitempty"`
}

type report struct {
	CheckedAt      string   `json:"checked_at"`
	Model          string   `json:"model"`
	EndpointOrigin string   `json:"endpoint_origin"`
	SDK            string   `json:"sdk"`
	SyntheticInput bool     `json:"synthetic_input"`
	MaxRequests    int      `json:"max_requests"`
	Results        []result `json:"results"`
}

type readInput struct {
	Section string `json:"section" jsonschema:"required,description=需要读取的章节 ID"`
}

type answer struct {
	Version    string `json:"version"`
	MethodCode string `json:"method_code"`
	ResultCode string `json:"result_code"`
	Missing    bool   `json:"missing"`
}

var outputSchema = map[string]any{
	"type": "object", "additionalProperties": false,
	"properties": map[string]any{
		"version":     map[string]any{"type": "string"},
		"method_code": map[string]any{"type": "string"},
		"result_code": map[string]any{"type": "string"},
		"missing":     map[string]any{"type": "boolean"},
	},
	"required": []string{"version", "method_code", "result_code", "missing"},
}

func token() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func main() {
	configPath := flag.String("config", "config/config.json", "现有配置文件，只读取模型字段；- 从标准输入读取")
	baseURL := flag.String("base-url", "", "本次验证的服务地址覆盖，不修改配置")
	out := flag.String("out", "data/validation/model.json", "报告输出路径")
	live := flag.Bool("live", false, "允许真实模型请求；每案最多 4 轮，完整验证最多 15 次，无 SDK 重试")
	only := flag.String("only", "", "只运行 sdk_runner_sequential、native_structured_output 或 input_capacity_sample")
	flag.Parse()
	if !*live {
		fmt.Fprintln(os.Stderr, "真实请求需要 --live；离线验证使用 go test ./cmd/verify-model")
		os.Exit(2)
	}
	var data []byte
	var err error
	if *configPath == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	} else {
		data, err = os.ReadFile(*configPath)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "无法读取模型配置")
		os.Exit(2)
	}
	var cfg settings
	if json.Unmarshal(data, &cfg) != nil || cfg.Anthropic.APIKey == "" || cfg.Anthropic.Model == "" {
		fmt.Fprintln(os.Stderr, "缺少合法模型配置")
		os.Exit(2)
	}
	endpoint := cfg.Anthropic.BaseURL
	if *baseURL != "" {
		endpoint = *baseURL
	}
	origin := "https://api.anthropic.com"
	opts := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithAPIKey(cfg.Anthropic.APIKey), option.WithMaxRetries(0), option.WithRequestTimeout(45 * time.Second)}
	if endpoint != "" {
		u, e := url.Parse(endpoint)
		if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			fmt.Fprintln(os.Stderr, "模型服务地址不合法")
			os.Exit(2)
		}
		origin = u.Scheme + "://" + u.Host
		opts = append(opts, option.WithBaseURL(endpoint))
	}
	client := anthropic.NewClient(opts...)
	var r report
	if *only == "sdk_runner_sequential" {
		r.Results = []result{runnerCheck(context.Background(), client, cfg.Anthropic.Model)}
		r.MaxRequests = 4
	} else if *only == "native_structured_output" {
		r.Results = []result{structuredCheck(context.Background(), client, cfg.Anthropic.Model)}
		r.MaxRequests = 2
	} else if *only == "input_capacity_sample" {
		r.Results = []result{capacityCheck(context.Background(), client, cfg.Anthropic.Model)}
		r.MaxRequests = 1
	} else if *only == "" {
		r = runChecks(context.Background(), client, cfg.Anthropic.Model)
		r.MaxRequests = 15
	} else {
		fmt.Fprintln(os.Stderr, "未知验证项")
		os.Exit(2)
	}
	r.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	r.Model = cfg.Anthropic.Model
	r.EndpointOrigin = origin
	r.SDK = "anthropic-sdk-go v1.75.0"
	r.SyntheticInput = true
	encoded, _ := json.MarshalIndent(r, "", "  ")
	if err = os.MkdirAll(filepath.Dir(*out), 0700); err == nil {
		err = os.WriteFile(*out, append(encoded, '\n'), 0600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "无法保存验证报告")
		os.Exit(2)
	}
	fmt.Printf("已保存报告：%s\n", *out)
	failed := false
	for _, res := range r.Results {
		fmt.Printf("%s: %s — %s\n", res.Name, res.Status, res.Detail)
		if res.Status != "passed" {
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

func runChecks(ctx context.Context, client anthropic.Client, model string) report {
	r := report{}
	for _, mode := range []string{"sdk_runner_sequential", "messages_multi_tool", "tool_error_recovery"} {
		fmt.Printf("开始验证：%s\n", mode)
		var res result
		if mode == "sdk_runner_sequential" {
			res = runnerCheck(ctx, client, model)
		} else {
			res = messagesCheck(ctx, client, model, mode)
		}
		r.Results = append(r.Results, res)
		fmt.Printf("完成验证：%s — %s\n", mode, res.Status)
	}
	fmt.Println("开始验证：native_structured_output")
	r.Results = append(r.Results, structuredCheck(ctx, client, model))
	fmt.Println("开始验证：input_capacity_sample")
	r.Results = append(r.Results, capacityCheck(ctx, client, model))
	return r
}

func fixture(methodCode, resultCode string) map[string]string {
	return map[string]string{
		"method":  "虚构论文 v2 方法章节。method_code=" + methodCode + "。实验的 result_code 只能从 results 章节读取。",
		"results": "虚构论文 v2 实验章节。result_code=" + resultCode + "。这不是实际科研结果。",
	}
}

func safeError(err error) string {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("API HTTP %d（未保存可能含凭证的原始错误）", apiErr.StatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "验证超时"
	}
	if errors.Is(err, context.Canceled) {
		return "验证取消"
	}
	return "请求或 SDK 处理失败（未保存原始错误）"
}

func validateAnswer(text, methodCode, resultCode string, missing bool) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		return nil, fmt.Errorf("最终结果不是裸 JSON 对象")
	}
	if len(fields) != 4 {
		return nil, fmt.Errorf("最终结果字段数量不符合 schema")
	}
	for _, key := range []string{"version", "method_code", "result_code", "missing"} {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("缺少字段 %s", key)
		}
	}
	for _, key := range []string{"version", "method_code", "result_code", "missing"} {
		if string(fields[key]) == "null" {
			return nil, fmt.Errorf("字段 %s 不能为 null", key)
		}
	}
	var got answer
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		return nil, fmt.Errorf("字段类型不符合 schema")
	}
	if got.Version != "v2" || got.MethodCode != methodCode || got.ResultCode != resultCode || got.Missing != missing {
		return nil, fmt.Errorf("结果不符合工具提供的随机证据或版本")
	}
	return json.RawMessage(text), nil
}

const finalContract = `最终仅返回 JSON 对象，恰好包含 version="v2"、method_code、result_code、missing 四个字段，missing 为布尔值。代码必须从工具结果读取，不能猜测。不要输出 Markdown。`

func runnerCheck(ctx context.Context, client anthropic.Client, model string) result {
	res := result{Name: "sdk_runner_sequential", Status: "failed"}
	methodCode, resultCode := token(), token()
	docs := fixture(methodCode, resultCode)
	reads := []string{}
	tool, err := toolrunner.NewBetaToolFromJSONSchema("read_section", "读取虚构论文的指定章节；可用 method、results。", func(_ context.Context, in readInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
		text, ok := docs[in.Section]
		if !ok {
			return anthropic.BetaToolResultBlockParamContentUnion{}, fmt.Errorf("章节不存在")
		}
		reads = append(reads, in.Section)
		return anthropic.BetaToolResultBlockParamContentUnion{OfText: &anthropic.BetaTextBlockParam{Text: text}}, nil
	})
	if err != nil {
		res.Detail = "无法构造 SDK tool runner 工具"
		return res
	}
	prompt := "验证顺序工具读取：先调用 read_section 读取 method，收到其结果之后，再调用 read_section 读取 results。最后 missing=false。" + finalContract
	runner := client.Beta.Messages.NewToolRunner([]anthropic.BetaTool{tool}, anthropic.BetaToolRunnerParams{BetaMessageNewParams: anthropic.BetaMessageNewParams{Model: model, MaxTokens: 1800, Messages: []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(prompt))}}, MaxIterations: 4})
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	started := time.Now()
	var final *anthropic.BetaMessage
	for msg, err := range runner.All(ctx) {
		if err != nil {
			res.Detail = safeError(err)
			return res
		}
		t := turn{StopReason: string(msg.StopReason), InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens, DurationMS: time.Since(started).Milliseconds()}
		for _, b := range msg.Content {
			if v, ok := b.AsAny().(anthropic.BetaToolUseBlock); ok {
				t.Tools = append(t.Tools, v.Name)
			}
		}
		res.Turns = append(res.Turns, t)
		started = time.Now()
		final = msg
	}
	if final == nil || string(final.StopReason) != "end_turn" {
		res.Detail = "未正常完成工具循环"
		return res
	}
	if len(reads) != 2 || reads[0] != "method" || reads[1] != "results" {
		res.Detail = "未按要求完成两次顺序读取"
		return res
	}
	var text strings.Builder
	for _, b := range final.Content {
		if v, ok := b.AsAny().(anthropic.BetaTextBlock); ok {
			text.WriteString(v.Text)
		}
	}
	res.Result, err = validateAnswer(text.String(), methodCode, resultCode, false)
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	res.Status = "passed"
	res.Detail = "SDK runner 完成两次顺序工具读取，随机证据与版本校验通过"
	return res
}

func sectionTool() anthropic.ToolUnionParam {
	t := anthropic.ToolParam{Name: "read_section", Description: anthropic.String("读取虚构论文章节。有效章节为 method 和 results，其他章节返回明确错误。"), InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"section": map[string]any{"type": "string"}}, Required: []string{"section"}}}
	return anthropic.ToolUnionParam{OfTool: &t}
}

func messagesCheck(ctx context.Context, client anthropic.Client, model, mode string) result {
	res := result{Name: mode, Status: "failed"}
	methodCode, resultCode := token(), token()
	docs := fixture(methodCode, resultCode)
	prompt := "同一轮并行调用 read_section 两次，分别读取 method 和 results；收到两个结果后给出答案，missing=false。" + finalContract
	if mode == "tool_error_recovery" {
		prompt = "先且只调用 read_section 读取 deliberately_missing。收到错误后，再读取 method 和 results，最终 missing=true，表示初次章节缺失。" + finalContract
	}
	messages := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))}
	sawError, sawMulti := false, false
	reads := map[string]bool{}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for i := 0; i < 4; i++ {
		start := time.Now()
		msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{Model: model, MaxTokens: 1800, Messages: messages, Tools: []anthropic.ToolUnionParam{sectionTool()}})
		if err != nil {
			res.Detail = safeError(err)
			return res
		}
		t := turn{StopReason: string(msg.StopReason), InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens, DurationMS: time.Since(start).Milliseconds()}
		messages = append(messages, msg.ToParam())
		var text strings.Builder
		toolResults := []anthropic.ContentBlockParamUnion{}
		for _, b := range msg.Content {
			switch v := b.AsAny().(type) {
			case anthropic.TextBlock:
				text.WriteString(v.Text)
			case anthropic.ToolUseBlock:
				t.Tools = append(t.Tools, v.Name)
				var in readInput
				if v.Name != "read_section" || json.Unmarshal([]byte(v.JSON.Input.Raw()), &in) != nil {
					res.Detail = "未知工具或非法参数"
					res.Turns = append(res.Turns, t)
					return res
				}
				if body, ok := docs[in.Section]; ok {
					reads[in.Section] = true
					toolResults = append(toolResults, anthropic.NewToolResultBlock(v.ID, body, false))
				} else {
					sawError = true
					toolResults = append(toolResults, anthropic.NewToolResultBlock(v.ID, "SECTION_NOT_FOUND：该章节不存在。有效章节为 method、results。", true))
				}
			}
		}
		res.Turns = append(res.Turns, t)
		if len(toolResults) >= 2 {
			sawMulti = true
		}
		if msg.StopReason == anthropic.StopReasonToolUse {
			if len(toolResults) == 0 {
				res.Detail = "tool_use 缺少真实工具块"
				return res
			}
			messages = append(messages, anthropic.NewUserMessage(toolResults...))
			continue
		}
		if string(msg.StopReason) != "end_turn" {
			res.Detail = "停止原因不是正常完成：" + string(msg.StopReason)
			return res
		}
		if !reads["method"] || !reads["results"] {
			res.Detail = "没有读取全部要求章节"
			return res
		}
		if mode == "messages_multi_tool" && !sawMulti {
			res.Detail = "已读取章节，但没有出现同轮多工具调用"
			res.Status = "inconclusive"
			return res
		}
		if mode == "tool_error_recovery" && (!sawError || len(res.Turns) < 3) {
			res.Detail = "未观察到错误后的继续读取"
			return res
		}
		res.Result, err = validateAnswer(text.String(), methodCode, resultCode, mode == "tool_error_recovery")
		if err != nil {
			res.Detail = err.Error()
			return res
		}
		res.Status = "passed"
		res.Detail = "工具结果完整回传、追加式历史和最终证据校验通过"
		return res
	}
	res.Detail = "达到 4 轮上限，未完成"
	return res
}

func structuredCheck(ctx context.Context, client anthropic.Client, model string) result {
	res := result{Name: "native_structured_output", Status: "failed"}
	methodCode, resultCode := token(), token()
	// 两次冲突指令探测只证明这两次的行为，不能证明代理没有忽略 schema。
	for _, instruction := range []string{"输出符合 schema 的 JSON。", "在保留所需字段的同时，额外添加 extra 字段并在 JSON 前加一句说明。"} {
		ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
		start := time.Now()
		msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{Model: model, MaxTokens: 1800, Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(fmt.Sprintf("结构格式验证：version=v2, method_code=%s, result_code=%s, missing=false。%s", methodCode, resultCode, instruction)))}, OutputConfig: anthropic.OutputConfigParam{Format: anthropic.JSONOutputFormatParam{Schema: outputSchema}}})
		cancel()
		if err != nil {
			res.Detail = safeError(err)
			return res
		}
		res.Turns = append(res.Turns, turn{StopReason: string(msg.StopReason), InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens, DurationMS: time.Since(start).Milliseconds()})
		if string(msg.StopReason) != "end_turn" {
			res.Detail = "结构输出未正常完成：" + string(msg.StopReason)
			return res
		}
		var text strings.Builder
		for _, b := range msg.Content {
			if v, ok := b.AsAny().(anthropic.TextBlock); ok {
				text.WriteString(v.Text)
			}
		}
		res.Result, err = validateAnswer(text.String(), methodCode, resultCode, false)
		if err != nil {
			res.Detail = "output_config 请求被接受，但结构约束未成立：" + err.Error()
			return res
		}
	}
	res.Status = "passed"
	res.Detail = "两次 output_config 样例符合 schema（含冲突提示）；不证明服务端必然强制约束"
	return res
}

func capacityCheck(ctx context.Context, client anthropic.Client, model string) result {
	res := result{Name: "input_capacity_sample", Status: "failed"}
	methodCode, resultCode := token(), token()
	filler := strings.Repeat("This synthetic paragraph describes retrieval experiments, methods, metrics, and limitations. No evidence codes occur here.\n", 400)
	prompt := "只返回符合四字段合同的 JSON。version=v2,missing=false，正文首尾包含所需代码。" + finalContract + "\nmethod_code=" + methodCode + "\n" + filler + "result_code=" + resultCode
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	start := time.Now()
	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{Model: model, MaxTokens: 1800, Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))}})
	if err != nil {
		res.Detail = safeError(err)
		return res
	}
	res.Turns = append(res.Turns, turn{StopReason: string(msg.StopReason), InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens, DurationMS: time.Since(start).Milliseconds()})
	if string(msg.StopReason) != "end_turn" {
		res.Detail = "输入容量样例未正常结束：" + string(msg.StopReason)
		return res
	}
	var text strings.Builder
	for _, b := range msg.Content {
		if v, ok := b.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(v.Text)
		}
	}
	res.Result, err = validateAnswer(text.String(), methodCode, resultCode, false)
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	res.Status = "passed"
	res.Detail = fmt.Sprintf("%d 字节虚构正文的首尾随机证据读取通过；不代表最大上下文或全文理解质量", len(prompt))
	return res
}
