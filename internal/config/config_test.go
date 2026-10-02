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

func TestLibraryDefaultsAndIndependentSwitches(t *testing.T) {
	cfg, err := loadFixture(t, `{"database":{"path":"data/digest.db"},"library":{"collect_enabled":true,"process_enabled":true},"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Delivery.Enabled || !cfg.Library.CollectEnabled || !cfg.Library.ProcessEnabled || cfg.Library.DocumentDir != filepath.Join("data", "library") || len(cfg.Library.Categories) != 5 || cfg.Library.Concurrency != 1 || cfg.Library.MaxRequests != 120 || cfg.Library.MaxTokens != 500000 {
		t.Fatalf("library defaults: %+v", cfg.Library)
	}
	for _, fields := range []string{`"concurrency":9`, `"concurrency":-1`, `"poll_seconds":5`, `"max_requests":-1`, `"max_tokens":10`, `"task_timeout_seconds":10`, `"categories":["cs.IR","cs.IR"]`, `"categories":["../../secret"]`, `"document_dir":" "`} {
		_, err := loadFixture(t, `{"library":{`+fields+`},"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search"}]}`)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid library accepted/leaked: %s %v", fields, err)
		}
	}
}

func TestLibraryCategoriesRejectUnsupportedDuringLoad(t *testing.T) {
	for _, fields := range []string{`"collect_enabled":true,"categories":["cs.CV"]`, `"categories":["cs.IR","cs.CV"]`, `"categories":["math"]`} {
		_, err := loadFixture(t, `{"library":{`+fields+`},"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search"}]}`)
		if err == nil || !strings.Contains(err.Error(), "library.categories") {
			t.Fatalf("unsupported startup configuration accepted: %s %v", fields, err)
		}
	}
	for _, category := range []string{"cs.IR", "cs.LG", "cs.AI", "cs.CL", "stat.ML"} {
		cfg, err := loadFixture(t, `{"library":{"collect_enabled":true,"categories":["`+category+`"]},"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search"}]}`)
		if err != nil || len(cfg.Library.Categories) != 1 || cfg.Library.Categories[0] != category {
			t.Fatalf("supported category rejected: %s %v", category, err)
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
	if _, err := cfg.ValidateDelivery(currentTopic); err == nil || !strings.Contains(err.Error(), "anthropic.api_key") {
		t.Fatalf("缺少密钥应拒绝: %v", err)
	}
	cfg.Anthropic.APIKey = "private-secret"
	if _, err := cfg.ValidateDelivery(currentTopic); err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("缺少 Webhook 应拒绝且不泄漏密钥: %v", err)
	}
	if _, err := cfg.DatabasePath(); err == nil {
		t.Fatal("缺少数据库路径应拒绝")
	}
}
