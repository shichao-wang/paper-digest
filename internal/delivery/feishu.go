package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type Sender interface {
	Send(context.Context, string) error
}

type Feishu struct {
	WebhookURL string
	Client     *http.Client
}

func (f Feishu) Send(ctx context.Context, text string) error {
	u, err := url.Parse(f.WebhookURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("飞书 Webhook 地址无效，必须使用 HTTPS")
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
