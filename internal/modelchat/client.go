// Package modelchat 使用标准库实现非流式 Chat 协议。
package modelchat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	ErrBudget            = errors.New("request budget exhausted")
	ErrProtocol          = errors.New("invalid chat response")
	ErrToolRounds        = errors.New("tool stage did not finish within its request limit")
	ErrMigrationRequired = errors.New("chat settings require explicit migration: configure a DeepSeek model and key, or an explicit compatible Chat base URL with its model and key")
)

const defaultBaseURL = "https://api.deepseek.com/v1"

// Budget 由所有 case 共享，在 HTTP Do 前消费一次；失败同样计数，不自动重试。
type Budget struct {
	mu        sync.Mutex
	max, used int
}

func NewBudget(max int) *Budget { return &Budget{max: max} }
func (b *Budget) Used() int     { b.mu.Lock(); defer b.mu.Unlock(); return b.used }
func (b *Budget) take() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used >= b.max {
		return ErrBudget
	}
	b.used++
	return nil
}

type Options struct {
	APIKey     string
	BaseURL    string
	Model      string
	Timeout    time.Duration
	HTTPClient *http.Client // 可注入离线测试 transport；始终禁用重定向。
	Budget     *Budget
}
type Client struct {
	key, endpoint, model string
	http                 *http.Client
	budget               *Budget
}

func NewClient(o Options) (*Client, error) {
	o.BaseURL = strings.TrimSpace(o.BaseURL)
	o.Model = strings.TrimSpace(o.Model)
	if strings.TrimSpace(o.APIKey) == "" || o.Model == "" {
		return nil, errors.New("invalid explicit chat settings")
	}
	deepSeekModel := strings.HasPrefix(strings.ToLower(o.Model), "deepseek-")
	if o.BaseURL == "" {
		// 旧 Anthropic 配置的空地址不能被默认为另一家服务商。
		if !deepSeekModel {
			return nil, ErrMigrationRequired
		}
		o.BaseURL = defaultBaseURL
	}
	u, err := url.Parse(o.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("invalid explicit chat settings")
	}
	if strings.EqualFold(u.Hostname(), "api.deepseek.com") {
		// 同时覆盖调用方已补入官方地址的旧配置；密钥不写入错误。
		if !deepSeekModel || strings.HasPrefix(strings.ToLower(strings.TrimSpace(o.APIKey)), "sk-ant-") {
			return nil, ErrMigrationRequired
		}
	}
	if u.Path == "/anthropic" || u.Path == "/anthropic/" {
		if u.Scheme != "https" || u.Host != "api.deepseek.com" {
			return nil, errors.New("only official DeepSeek anthropic base is supported")
		}
	} else if u.Path != "" && u.Path != "/" && u.Path != "/v1" && u.Path != "/v1/" {
		return nil, errors.New("unsupported chat base path")
	}
	u.Path, u.RawPath = "/v1/chat/completions", ""
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	h := &http.Client{}
	if o.HTTPClient != nil {
		*h = *o.HTTPClient
	}
	h.Timeout = timeout
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("chat redirects disabled") }
	return &Client{key: o.APIKey, endpoint: u.String(), model: o.Model, http: h, budget: o.Budget}, nil
}

// 使用原始 JSON 消息保留 assistant reasoning_content、完整 tool_calls 和其他字段，
// 工具轮与最终 JSON 请求都不会丢失这些历史。
func Message(role, content string) json.RawMessage {
	b, _ := json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{role, content})
	return b
}
func ToolResult(id string, value any) json.RawMessage {
	content, err := json.Marshal(value)
	if err != nil {
		content = []byte(`{"error":"tool result unavailable"}`)
	}
	b, _ := json.Marshal(struct {
		Role    string `json:"role"`
		ID      string `json:"tool_call_id"`
		Content string `json:"content"`
	}{"tool", id, string(content)})
	return b
}

type Function struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters"`
}
type ToolDefinition struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type Request struct {
	Messages   []json.RawMessage
	Tools      []ToolDefinition
	JSONObject bool
	MaxTokens  int
}

// AnalysisOutputTokens 是论文库 JSON 请求的单次最大输出，不是任务累计预算。
const AnalysisOutputTokens = 4096

// ReservationTokens 按输入 UTF-8 字节、wire 字段开销和输出上限保守预留。
// 调用方先显式确定 MaxTokens；配置预检和持久检查点必须使用同一公式。
func ReservationTokens(request Request, model string) (int64, error) {
	if request.MaxTokens <= 0 {
		return 0, errors.New("token reservation requires a positive output limit")
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return 0, errors.New("invalid chat request JSON")
	}
	return int64(len(raw)) + 1024 + 6*int64(len(model)) + int64(request.MaxTokens), nil
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}
type Response struct {
	Assistant    json.RawMessage
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
	Duration     time.Duration
	HTTPStatus   int
}

func (c *Client) Chat(ctx context.Context, request Request) (Response, error) {
	start := time.Now()
	var result Response
	if len(request.Messages) == 0 || (request.JSONObject && len(request.Tools) != 0) {
		return result, errors.New("invalid chat request")
	}
	max := request.MaxTokens
	if max <= 0 {
		max = 2048
	}
	wire := struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Tools    []ToolDefinition  `json:"tools,omitempty"`
		Thinking struct {
			Type string `json:"type"`
		} `json:"thinking"`
		ResponseFormat *struct {
			Type string `json:"type"`
		} `json:"response_format,omitempty"`
		MaxTokens int  `json:"max_tokens"`
		Stream    bool `json:"stream"`
	}{Model: c.model, Messages: request.Messages, Tools: request.Tools, MaxTokens: max}
	wire.Thinking.Type = "disabled"
	if request.JSONObject {
		wire.ResponseFormat = &struct {
			Type string `json:"type"`
		}{"json_object"}
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return result, errors.New("invalid chat request JSON")
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("unable to construct chat request")
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.key)
	httpRequest.Header.Set("Content-Type", "application/json")
	if c.budget != nil {
		if err := c.budget.take(); err != nil {
			return result, err
		}
	}
	resp, err := c.http.Do(httpRequest)
	result.Duration = time.Since(start)
	if err != nil {
		return result, safeHTTPError(ctx, err, "chat transport failed")
	}
	defer resp.Body.Close()
	result.HTTPStatus = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result, errors.New("chat HTTP status rejected")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	result.Duration = time.Since(start)
	if err != nil {
		return result, safeHTTPError(ctx, err, "chat response body read failed")
	}
	if len(data) > 4<<20 {
		return result, ErrProtocol
	}
	var envelope struct {
		Choices []struct {
			Message      json.RawMessage `json:"message"`
			FinishReason string          `json:"finish_reason"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if UniqueJSON(data) != nil || json.Unmarshal(data, &envelope) != nil || len(envelope.Choices) != 1 {
		return result, ErrProtocol
	}
	choice := envelope.Choices[0]
	result.Assistant = append(json.RawMessage(nil), choice.Message...)
	result.FinishReason, result.Usage = choice.FinishReason, envelope.Usage
	var assistant struct {
		Role      string          `json:"role"`
		Content   *string         `json:"content"`
		Refusal   json.RawMessage `json:"refusal"`
		ToolCalls []ToolCall      `json:"tool_calls"`
	}
	if json.Unmarshal(choice.Message, &assistant) != nil || assistant.Role != "assistant" {
		return result, ErrProtocol
	}
	if len(assistant.Refusal) != 0 && !bytes.Equal(assistant.Refusal, []byte("null")) && !bytes.Equal(assistant.Refusal, []byte(`""`)) {
		return result, errors.New("chat response refused")
	}
	result.ToolCalls = assistant.ToolCalls
	if assistant.Content != nil {
		result.Content = *assistant.Content
	}
	switch result.FinishReason {
	case "stop":
		if len(result.ToolCalls) != 0 || strings.TrimSpace(result.Content) == "" {
			return result, ErrProtocol
		}
	case "tool_calls":
		if request.JSONObject || len(request.Tools) == 0 || len(result.ToolCalls) == 0 {
			return result, ErrProtocol
		}
		ids := map[string]bool{}
		for _, call := range result.ToolCalls {
			if call.ID == "" || ids[call.ID] || call.Type != "function" || call.Function.Name == "" {
				return result, ErrProtocol
			}
			ids[call.ID] = true
		}
	default:
		return result, errors.New("chat did not finish successfully")
	}
	return result, nil
}

func safeHTTPError(ctx context.Context, err error, fallback string) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return errors.New("chat request timed out")
	}
	return errors.New(fallback)
}

type Tool struct {
	Definition ToolDefinition
	Validate   func(json.RawMessage) error
	Execute    func(context.Context, json.RawMessage) (any, error)
}
type Turn struct {
	Stop       string   `json:"stop"`
	Tools      []string `json:"tools"`
	Usage      Usage    `json:"usage"`
	DurationMS int64    `json:"duration_ms"`
	HTTPStatus int      `json:"http_status"`
}

// CallEvent 记录本地调用验收与执行轮次，不保存参数或工具结果。
// Round 从 1 开始；Executed 表示已进入本地处理函数。
type CallEvent struct {
	Round     int    `json:"round"`
	Name      string `json:"name"`
	ID        string `json:"id"`
	Validated bool   `json:"validated"`
	Executed  bool   `json:"executed"`
}
type Session struct {
	History  []json.RawMessage
	Turns    []Turn
	Executed []string
	Calls    []CallEvent
}

// RunTools 只有观察到正常 stop 才成功；每轮全部调用都有结果，
// 未知工具名和非法参数仅返回错误，不执行。
func (c *Client) RunTools(ctx context.Context, history []json.RawMessage, tools []Tool, maxRequests int) (Session, error) {
	s := Session{History: append([]json.RawMessage(nil), history...)}
	defs := make([]ToolDefinition, 0, len(tools))
	handlers := map[string]Tool{}
	for _, tool := range tools {
		name := tool.Definition.Function.Name
		if name == "" || tool.Validate == nil || tool.Execute == nil {
			return s, errors.New("invalid local tool")
		}
		if _, exists := handlers[name]; exists {
			return s, errors.New("duplicate local tool")
		}
		defs = append(defs, tool.Definition)
		handlers[name] = tool
	}
	for round := 0; round < maxRequests; round++ {
		response, err := c.Chat(ctx, Request{Messages: s.History, Tools: defs})
		turn := Turn{Stop: response.FinishReason, Usage: response.Usage, DurationMS: response.Duration.Milliseconds(), HTTPStatus: response.HTTPStatus, Tools: []string{}}
		for _, call := range response.ToolCalls {
			turn.Tools = append(turn.Tools, call.Function.Name)
		}
		s.Turns = append(s.Turns, turn)
		if err != nil {
			return s, err
		}
		s.History = append(s.History, response.Assistant)
		if response.FinishReason == "stop" {
			return s, nil
		}
		for _, call := range response.ToolCalls {
			s.Calls = append(s.Calls, CallEvent{Round: round + 1, Name: call.Function.Name, ID: call.ID})
			event := &s.Calls[len(s.Calls)-1]
			tool, known := handlers[call.Function.Name]
			args := json.RawMessage(call.Function.Arguments)
			var value any = map[string]string{"error": "unknown tool; select an available tool"}
			if known {
				if UniqueJSON(args) != nil || tool.Validate(args) != nil {
					value = map[string]string{"error": "invalid tool arguments; follow the declared schema"}
				} else {
					event.Validated = true
					if err := ctx.Err(); err != nil {
						return s, err
					}
					var executionErr error
					event.Executed = true
					value, executionErr = tool.Execute(ctx, args)
					s.Executed = append(s.Executed, call.Function.Name)
					if executionErr != nil {
						value = map[string]string{"error": "tool operation failed"}
					}
				}
			}
			s.History = append(s.History, ToolResult(call.ID, value))
		}
	}
	return s, ErrToolRounds
}
