package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/digest"
)

type Sender interface {
	Send(context.Context, string) error
}

type MarkdownSender interface {
	SendMarkdown(context.Context, string) error
}

// MarkdownValidator checks the exact card payload without making a request.
type MarkdownValidator interface {
	ValidateMarkdown(string) error
}

// Preparer 在写入发送意图之前解析当前配置，避免配置缺失被记录为未知结果。
type Preparer interface {
	Prepare(context.Context) (Sender, error)
}

type WebhookStore interface {
	Webhook(context.Context, string) (string, error)
}

// StoredFeishu 每次发送从数据库读取地址，Prepare 返回本次发送使用的快照。
type StoredFeishu struct {
	Store  WebhookStore
	Topic  string
	Client *http.Client
}

func (f StoredFeishu) Prepare(ctx context.Context) (Sender, error) {
	if f.Store == nil {
		return nil, errors.New("读取 Webhook 配置失败")
	}
	webhook, err := f.Store.Webhook(ctx, f.Topic)
	if err != nil {
		return nil, errors.New("读取 Webhook 配置失败")
	}
	if webhook == "" {
		return nil, errors.New("缺少主题 Webhook；请先在页面设置")
	}
	if err := config.ValidateWebhookURL(webhook); err != nil {
		return nil, err
	}
	return Feishu{WebhookURL: webhook, Client: f.Client}, nil
}

func (f StoredFeishu) Send(ctx context.Context, text string) error {
	sender, err := f.Prepare(ctx)
	if err != nil {
		return err
	}
	return sender.Send(ctx, text)
}

type Feishu struct {
	WebhookURL string
	Client     *http.Client
}

func (f Feishu) Send(ctx context.Context, text string) error {
	payload, err := json.Marshal(struct {
		Type    string            `json:"msg_type"`
		Content map[string]string `json:"content"`
	}{Type: "text", Content: map[string]string{"text": text}})
	if err != nil {
		return err
	}
	return f.send(ctx, payload)
}

// SendMarkdown uses a Markdown card: custom robot Webhooks do not accept the
// md element supported by the separate application message API.
func (f Feishu) SendMarkdown(ctx context.Context, text string) error {
	payload, err := json.Marshal(markdownCard(text))
	if err != nil {
		return err
	}
	return f.send(ctx, payload)
}

func (f Feishu) ValidateMarkdown(text string) error {
	payload, err := json.Marshal(markdownCard(text))
	if err != nil {
		return err
	}
	return f.validatePayload(payload)
}

var feishuMentionTag = regexp.MustCompile(`(?i)</?at\b[^>]*>`)

// Model and paper text must not activate Feishu-specific mention markup.
// Escape only mention tags so ordinary Markdown and card styling still work.
func escapeFeishuMentions(text string) string {
	return feishuMentionTag.ReplaceAllStringFunc(text, func(tag string) string {
		return strings.ReplaceAll(strings.ReplaceAll(tag, "<", "&lt;"), ">", "&gt;")
	})
}

func markdownCard(text string) map[string]any {
	blocks := strings.SplitN(strings.TrimSpace(text), "\n\n", 5)
	header := "arXiv 论文日报"
	if strings.HasPrefix(blocks[0], "# ") {
		header = strings.TrimPrefix(blocks[0], "# ")
		blocks = blocks[1:]
	}
	markdown := func(content, size string) map[string]any {
		return map[string]any{"tag": "markdown", "content": escapeFeishuMentions(content), "text_size": size}
	}
	elements := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		if strings.HasPrefix(block, "## ") {
			elements = append(elements, markdown("**"+strings.TrimPrefix(block, "## ")+"**", "heading-2"))
		} else if strings.HasPrefix(block, "- 原文：") {
			elements = append(elements, map[string]any{
				"tag": "column_set", "flex_mode": "none",
				"columns": []map[string]any{{
					"tag": "column", "width": "weighted", "weight": 1,
					"background_style": "grey-50", "padding": "12px",
					"elements": []map[string]any{markdown(block, "notation")},
				}},
			})
		} else if block == "基于论文公开摘要整理，非全文解读。" {
			elements = append(elements, markdown("<font color='grey'>"+block+"</font>", "notation"))
		} else {
			elements = append(elements, markdown(digest.FormatSummary(block), "normal"))
		}
	}
	return map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"schema": "2.0", "config": map[string]any{"width_mode": "default"},
			"header": map[string]any{
				"title":    map[string]string{"tag": "plain_text", "content": header},
				"template": "blue",
			},
			"body": map[string]any{"direction": "vertical", "padding": "12px 12px 20px 12px", "vertical_spacing": "12px", "elements": elements},
		},
	}
}

func (f Feishu) validatePayload(payload []byte) error {
	if err := config.ValidateWebhookURL(f.WebhookURL); err != nil {
		return err
	}
	if len(payload) > 20*1024 {
		return errors.New("飞书消息超过 20 KB 请求大小限制")
	}
	return nil
}

func (f Feishu) send(ctx context.Context, payload []byte) error {
	if err := f.validatePayload(payload); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		return errors.New("构造飞书请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		// 不包装底层网络错误：URL 中可能包含 Webhook 密钥。
		return errors.New("飞书响应未知，请核对群消息后处理")
	}
	defer resp.Body.Close()
	var result struct {
		Code *int `json:"code"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result) != nil || result.Code == nil || *result.Code != 0 {
		return fmt.Errorf("飞书未确认成功（HTTP %d），请人工核对", resp.StatusCode)
	}
	return nil
}
