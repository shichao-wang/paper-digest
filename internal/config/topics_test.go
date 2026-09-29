package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const currentTopic = "recommendation-advertising-search"

func loadFixture(t *testing.T, text string) (Topics, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "topics.json")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(path, currentTopic)
}

func TestWebhookSelectsDistinctRobots(t *testing.T) {
	topics, err := loadFixture(t, `{"topics":[{"id":"recommendation-advertising-search","webhook_env":"FEISHU_WEBHOOK_RAS"},{"id":"new-topic","webhook_env":"FEISHU_WEBHOOK_NEW"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"FEISHU_WEBHOOK_RAS": "https://example.com/ras", "FEISHU_WEBHOOK_NEW": "https://example.com/new"}
	for id, want := range map[string]string{currentTopic: values["FEISHU_WEBHOOK_RAS"], "new-topic": values["FEISHU_WEBHOOK_NEW"]} {
		got, err := topics.Webhook(id, func(name string) string { return values[name] })
		if err != nil || got != want {
			t.Fatalf("主题 %s: got %q, err %v", id, got, err)
		}
	}
	for _, id := range []string{"unknown", ""} {
		if _, err := topics.Webhook(id, func(string) string { t.Fatal("不应读取环境变量"); return "" }); err == nil {
			t.Fatalf("未知主题 %q 被接受", id)
		}
	}
}

func TestInvalidConfigsDoNotEchoSecrets(t *testing.T) {
	for name, text := range map[string]string{
		"empty":           `{ "topics": [] }`,
		"missing-id":      `{"topics":[{"webhook_env":"FEISHU_WEBHOOK_RAS"}]}`,
		"invalid-id":      `{"topics":[{"id":"UPPER","webhook_env":"FEISHU_WEBHOOK_RAS"}]}`,
		"missing-env":     `{"topics":[{"id":"recommendation-advertising-search"}]}`,
		"url-in-config":   `{"topics":[{"id":"recommendation-advertising-search","webhook_env":"https://example.com/secret"}]}`,
		"duplicate-topic": `{"topics":[{"id":"recommendation-advertising-search","webhook_env":"FEISHU_WEBHOOK_RAS"},{"id":"recommendation-advertising-search","webhook_env":"FEISHU_WEBHOOK_OTHER"}]}`,
		"duplicate-env":   `{"topics":[{"id":"recommendation-advertising-search","webhook_env":"FEISHU_WEBHOOK_RAS"},{"id":"another","webhook_env":"FEISHU_WEBHOOK_RAS"}]}`,
		"missing-current": `{"topics":[{"id":"another","webhook_env":"FEISHU_WEBHOOK_OTHER"}]}`,
		"unknown-field":   `{"topics":[{"id":"recommendation-advertising-search","webhook_env":"FEISHU_WEBHOOK_RAS","https://example.com/secret":"value"}]}`,
		"trailing":        `{"topics":[{"id":"recommendation-advertising-search","webhook_env":"FEISHU_WEBHOOK_RAS"}]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadFixture(t, text)
			if err == nil || strings.Contains(err.Error(), "https://example.com/secret") {
				t.Fatalf("应拒绝配置且不泄漏 URL: %v", err)
			}
		})
	}
}

func TestWebhookRequiresValidHTTPSWithoutEchoingSecret(t *testing.T) {
	topics, err := loadFixture(t, `{"topics":[{"id":"recommendation-advertising-search","webhook_env":"FEISHU_WEBHOOK_RAS"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "http://example.com/secret", "https://example.com/secret#fragment", "://secret"} {
		_, err := topics.Webhook(currentTopic, func(string) string { return value })
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("应拒绝无效 Webhook 且不泄漏 URL: %v", err)
		}
	}
}
