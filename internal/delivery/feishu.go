package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shichao-wang/paper-digest/internal/config"
)

type Sender interface {
	Send(context.Context, string) error
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
	if err := config.ValidateWebhookURL(f.WebhookURL); err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		Type    string            `json:"msg_type"`
		Content map[string]string `json:"content"`
	}{Type: "text", Content: map[string]string{"text": text}})
	if err != nil {
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
