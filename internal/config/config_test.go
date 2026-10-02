package config

import (
	"encoding/json"
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

func TestLibraryDocumentDirDefaultsFromSQLitePath(t *testing.T) {
	for _, tc := range []struct {
		name, path, want string
	}{
		{"plain-relative", "data/digest.db", filepath.Join("data", "library")},
		{"plain-special-characters", "data ?#/digest.db", filepath.Join("data ?#", "library")},
		{"absolute-uri", "file:/tmp/paper-digest/digest.db?mode=rwc", "/tmp/paper-digest/library"},
		{"localhost-uri", "file://localhost/tmp/paper-digest/digest.db?cache=shared", "/tmp/paper-digest/library"},
		{"relative-uri", "file:data/digest.db?mode=rwc&cache=shared", filepath.Join("data", "library")},
		{"escaped-relative-uri", "file:data%20%3F%23/digest.db?mode=rwc", filepath.Join("data ?#", "library")},
		{"escaped-absolute-uri", "file:///tmp/paper%20%3F%23/digest.db?mode=rwc", "/tmp/paper ?#/library"},
		{"escaped-once", "file:data%2520/digest.db", filepath.Join("data%20", "library")},
		{"query-slash", "file:digest.db?cache=shared&ignored=a/b", "library"},
		{"memory", ":memory:", "library"},
		{"memory-uri", "file::memory:?cache=shared", "library"},
		{"named-memory-uri", "file:data/cache.db?mode=memory&cache=shared", "library"},
		{"absolute-memory-uri", "file:///tmp/cache.db?mode=memory", "library"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quoted, _ := json.Marshal(tc.path)
			cfg, err := loadFixture(t, `{"database":{"path":`+string(quoted)+`},"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search"}]}`)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Library.DocumentDir != tc.want {
				t.Fatalf("document_dir = %q, want %q", cfg.Library.DocumentDir, tc.want)
			}
			if cfg.Database.Path != tc.path {
				t.Fatal("default document_dir changed the database DSN")
			}
		})
	}
}

func TestLibraryDocumentDirRejectsInvalidSQLiteURIDefaultWithoutEcho(t *testing.T) {
	for _, path := range []string{"file:data%xx/private-secret.db", "file:/tmp/%xx/private-secret.db", "file://private-secret/tmp/digest.db", "file:?cache=shared&private-secret=1"} {
		quoted, _ := json.Marshal(path)
		_, err := loadFixture(t, `{"database":{"path":`+string(quoted)+`},"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search"}]}`)
		if err == nil || !strings.Contains(err.Error(), "library.document_dir") || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("invalid SQLite URI default accepted or echoed: %v", err)
		}
	}
	cfg, err := loadFixture(t, `{"database":{"path":"file:data%20directory/digest.db?mode=rwc"},"library":{"document_dir":"custom/documents"},"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search"}]}`)
	if err != nil || cfg.Library.DocumentDir != "custom/documents" {
		t.Fatalf("explicit document_dir was not preserved: %q %v", cfg.Library.DocumentDir, err)
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

func TestDeliveryRequiresConfiguredTopicIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, topics string
		valid        bool
	}{
		{"other-topic", `[{"id":"another","webhook_url":"https://example.com/private-secret"}]`, false},
		{"misspelled-topic", `[{"id":"recommendation-advertising-seach"}]`, false},
		{"correct-topic-without-webhook", `[{"id":"recommendation-advertising-search"}]`, true},
		{"correct-topic-among-others", `[{"id":"another"},{"id":"recommendation-advertising-search"}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadFixture(t, `{"delivery":{"enabled":true},"anthropic":{"api_key":"private-secret"},"arxiv":{"lookback_days":7},"topics":`+tc.topics+`}`)
			if err != nil {
				t.Fatal(err)
			}
			err = cfg.ValidateDelivery(currentTopic)
			if (err == nil) != tc.valid || (err != nil && strings.Contains(err.Error(), "private-secret")) {
				t.Fatalf("主题校验错误: valid=%v err=%v", tc.valid, err)
			}
		})
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
	if err := cfg.ValidateDelivery(currentTopic); err == nil || !strings.Contains(err.Error(), "anthropic.api_key") {
		t.Fatalf("缺少密钥应拒绝: %v", err)
	}
	cfg.Anthropic.APIKey = "private-secret"
	if err := cfg.ValidateDelivery(currentTopic); err != nil {
		t.Fatalf("有 API 密钥和已登记主题即可启动，Webhook 允许页面配置: %v", err)
	}
	if _, err := cfg.DatabasePath(); err == nil {
		t.Fatal("缺少数据库路径应拒绝")
	}
}
