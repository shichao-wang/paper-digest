package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const currentTopic = "recommendation-advertising-search"

func loadFixture(t *testing.T, text string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestModelGatewayConfig(t *testing.T) {
	cfg, err := loadFixture(t, `{"anthropic":{"base_url":" http://host.docker.internal:3425 ","model":"group/deepseek-v4-1-flash"},"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Anthropic.BaseURL != "http://host.docker.internal:3425" || cfg.Anthropic.Model != "group/deepseek-v4-1-flash" {
		t.Fatal("未保留模型网关配置")
	}
	for _, value := range []string{"ftp://example.com/secret", "http:///secret", "https://user:secret@example.com", "https://example.com?key=secret", "https://example.com#secret"} {
		_, err := loadFixture(t, `{"anthropic":{"base_url":"`+value+`"},"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search"}]}`)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("应拒绝无效网关地址且不泄漏凭据: %v", err)
		}
	}
}

func TestWebhookSelectsDistinctRobots(t *testing.T) {
	cfg, err := loadFixture(t, `{"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search","webhook_url":"https://example.com/ras"},{"id":"new-topic","webhook_url":"https://example.com/new"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{currentTopic: "https://example.com/ras", "new-topic": "https://example.com/new"} {
		got, err := cfg.Webhook(id)
		if err != nil || got != want {
			t.Fatalf("主题 %s: got %q, err %v", id, got, err)
		}
	}
	for _, id := range []string{"unknown", ""} {
		if _, err := cfg.Webhook(id); err == nil {
			t.Fatalf("未知主题 %q 被接受", id)
		}
	}
}

func TestInvalidConfigsDoNotEchoSecrets(t *testing.T) {
	for name, text := range map[string]string{
		"empty":           `{"arxiv":{"lookback_days":7},"topics":[]}`,
		"missing-id":      `{"arxiv":{"lookback_days":7},"topics":[{"webhook_url":"https://example.com/secret"}]}`,
		"invalid-id":      `{"arxiv":{"lookback_days":7},"topics":[{"id":"UPPER","webhook_url":"https://example.com/secret"}]}`,
		"duplicate-topic": `{"arxiv":{"lookback_days":7},"topics":[{"id":"new-topic"},{"id":"new-topic"}]}`,
		"unknown-field":   `{"arxiv":{"lookback_days":7},"topics":[{"id":"new-topic","https://example.com/secret":"value"}]}`,
		"trailing":        `{"arxiv":{"lookback_days":7},"topics":[{"id":"new-topic"}]} {}`,
		"lookback-zero":   `{"arxiv":{"lookback_days":0},"topics":[{"id":"new-topic"}]}`,
		"lookback-large":  `{"arxiv":{"lookback_days":31},"topics":[{"id":"new-topic"}]}`,
		"invalid-type":    `{"arxiv":{"lookback_days":"secret"},"topics":[{"id":"new-topic"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadFixture(t, text)
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("应拒绝配置且不泄漏凭据: %v", err)
			}
		})
	}
}

func TestWebhookRequiresValidHTTPSWithoutEchoingSecret(t *testing.T) {
	for _, value := range []string{"", "http://example.com/secret", "https://example.com/secret#fragment", "://secret"} {
		cfg := Config{Topics: []Topic{{ID: currentTopic, WebhookURL: value}}}
		_, err := cfg.Webhook(currentTopic)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("应拒绝无效 Webhook 且不泄漏 URL: %v", err)
		}
	}
}

func TestDeliveryRequiresFileCredentialsOnlyWhenEnabled(t *testing.T) {
	cfg, err := loadFixture(t, `{"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search","webhook_url":""}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Delivery.Enabled {
		t.Fatal("未配置的投递必须默认关闭")
	}
	if err := cfg.ValidateDelivery(); err == nil || !strings.Contains(err.Error(), "anthropic.api_key") {
		t.Fatalf("缺少密钥应拒绝: %v", err)
	}
	cfg.Anthropic.APIKey = "private-secret"
	if err := cfg.ValidateDelivery(); err != nil {
		t.Fatalf("有 API 密钥即可启动，Webhook 允许页面配置: %v", err)
	}
	if _, err := cfg.DatabasePath(); err == nil {
		t.Fatal("缺少数据库路径应拒绝")
	}
}
