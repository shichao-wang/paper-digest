package digest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/skills"
)

// DefaultModel is used when the config leaves anthropic.model empty.
const DefaultModel = "deepseek-flash"

const (
	maxOutputTokens = 4096
	thinkingBudget  = 2048
)

type Summary struct {
	Text          string
	Model         string
	PromptVersion string
}

type Analyzer interface {
	Analyze(ctx context.Context, paper papers.Paper) (Summary, error)
}

type ClaudeAnalyzer struct {
	Model       string
	APIKey      string
	BaseURL     string
	Instruction string
	Version     string
	request     func(context.Context, string, string, string, string) (string, error)
}

func (a ClaudeAnalyzer) Analyze(ctx context.Context, paper papers.Paper) (Summary, error) {
	model := strings.TrimSpace(a.Model)
	if model == "" {
		model = DefaultModel
	}
	instruction := strings.TrimSpace(a.Instruction)
	version := a.Version
	if instruction == "" {
		skill, err := defaultSkill(skills.Summary)
		if err != nil {
			return Summary{}, err
		}
		instruction = skill.Body
		if version == "" {
			version = skill.Version()
		}
	}
	if version == "" {
		version = skills.Version(skills.Summary, instruction)
	}

	request := a.request
	if request == nil {
		request = requestClaude
	}
	text, err := request(ctx, a.APIKey, a.BaseURL, model, instruction+"\n\n"+paperSection(paper))
	if err != nil {
		return Summary{}, fmt.Errorf("analyze arXiv paper %s: %w", paper.ID, err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Summary{}, fmt.Errorf("analyze arXiv paper %s: Claude returned an empty summary", paper.ID)
	}
	if !hasCompleteMath(text) {
		return Summary{}, fmt.Errorf("analyze arXiv paper %s: unclosed math delimiter", paper.ID)
	}
	return Summary{Text: FormatSummary(text), Model: model, PromptVersion: version}, nil
}

func newClaudeClient(apiKey, baseURL string, opts ...option.RequestOption) anthropic.Client {
	options := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithAPIKey(apiKey)}
	if baseURL = strings.TrimSpace(baseURL); baseURL != "" {
		options = append(options, option.WithBaseURL(baseURL))
	}
	return anthropic.NewClient(append(options, opts...)...)
}

func requestClaude(ctx context.Context, apiKey, baseURL, model, prompt string) (string, error) {
	client := newClaudeClient(apiKey, baseURL)
	response, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: maxOutputTokens,
		Thinking:  anthropic.ThinkingConfigParamOfEnabled(thinkingBudget),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", err
	}

	// 截断或非正常结束的文本不能作为完整摘要保存，留给任务恢复时重试。
	if response.StopReason != anthropic.StopReasonEndTurn {
		return "", fmt.Errorf("summary did not complete: stop_reason=%q, output_tokens=%d", response.StopReason, response.Usage.OutputTokens)
	}

	var text strings.Builder
	for _, block := range response.Content {
		if block, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(block.Text)
		}
	}
	return text.String(), nil
}

func paperSection(paper papers.Paper) string {
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "论文 ID：%s", paper.ID)
	if paper.Version != "" {
		fmt.Fprint(&prompt, paper.Version)
	}
	prompt.WriteByte('\n')
	fmt.Fprintf(&prompt, "标题：%s\n", paper.Title)
	fmt.Fprintf(&prompt, "作者：%s\n", strings.Join(paper.Authors, "、"))
	if !paper.Published.IsZero() {
		fmt.Fprintf(&prompt, "发布日期：%s\n", paper.Published.Format(time.RFC3339))
	}
	if !paper.Updated.IsZero() {
		fmt.Fprintf(&prompt, "更新时间：%s\n", paper.Updated.Format(time.RFC3339))
	}
	fmt.Fprintf(&prompt, "原文链接：%s\n", paper.URL)
	fmt.Fprintf(&prompt, "论文摘要：\n%s\n", paper.Abstract)
	return prompt.String()
}
