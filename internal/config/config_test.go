package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shichao-wang/paper-digest/internal/papers"
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

func TestOmittedSelectionUsesBuiltinRules(t *testing.T) {
	cfg, err := loadFixture(t, `{"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search"},{"id":"another"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	rules, spec, ok := cfg.TopicSelection(currentTopic)
	if !ok || rules.Query != papers.DefaultRules().Query || spec.MaxPapers != papers.DefaultSelection().MaxPapers {
		t.Fatalf("omitted selection = %s ok=%v", rules.Query, ok)
	}
	handBuilt := Config{Topics: []Topic{{ID: currentTopic}}}
	handRules, _, handOK := handBuilt.TopicSelection(currentTopic)
	if !handOK || handRules.Query != papers.DefaultRules().Query {
		t.Fatal("hand-built joint topic did not use the built-in rules")
	}
	if _, _, ok := cfg.TopicSelection("another"); ok {
		t.Fatal("a topic without selection was treated as having rules")
	}
	paper := papers.Paper{ID: "arxiv:2610.10483", Title: "Two-Level Softmax Sampling", Abstract: "softmax sampling", Categories: []string{"cs.IR"}}
	if papers.Explain(paper, rules).Reason == "" {
		t.Fatal("builtin rules did not explain a paper")
	}
}

func TestSelectionRulesAreValidatedWithoutEchoingThePattern(t *testing.T) {
	secret := "super-secret-pattern"
	text := `{"arxiv":{"lookback_days":7},"topics":[{"id":"another","selection":{"max_papers":5,"min_tier":1,"query":{"categories":["cs.IR"]},"signals":[{"name":"title:hit","pattern":"(` + secret + `","fields":["title"],"tier":2}],"decisions":[{"tier":2,"reason":"title match","min_signal_tier":2}],"fallback_reason":"none"}}]}`
	_, err := loadFixture(t, text)
	if err == nil || !strings.Contains(err.Error(), "筛选规则无效") || !strings.Contains(err.Error(), "正则") || strings.Contains(err.Error(), secret) {
		t.Fatalf("err = %v", err)
	}
	unknown := `{"arxiv":{"lookback_days":7},"topics":[{"id":"recommendation-advertising-search","selection":{"max_papers":5,"min_tier":1,"query":{"categories":["cs.IR"]},"signals":[],"decisions":[],"fallback_reason":"none","` + secret + `":1}}]}`
	_, err = loadFixture(t, unknown)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unknown field err = %v", err)
	}
}

func TestExampleConfigMatchesBuiltinSelection(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	rules, _, ok := cfg.TopicSelection(currentTopic)
	if !ok || rules.Query != papers.DefaultRules().Query || rules.MaxPapers() != 5 || rules.MinTier() != 1 {
		t.Fatalf("example rules query=%s max=%d min=%d ok=%v", rules.Query, rules.MaxPapers(), rules.MinTier(), ok)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "config.example.json"))
	if err != nil || !strings.Contains(string(raw), `"selection"`) {
		t.Fatal("example config is missing the selection block")
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "papers", "testdata", "2026-10-09-ras.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ID         string   `json:"id"`
		Title      string   `json:"title"`
		Abstract   string   `json:"abstract"`
		Categories []string `json:"categories"`
	}
	if err := json.Unmarshal(fixture, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		paper := papers.Paper{ID: row.ID, Title: row.Title, Abstract: row.Abstract, Categories: row.Categories}
		got := papers.Explain(paper, rules)
		want := papers.Explain(paper, papers.DefaultRules())
		if got.Tier != want.Tier || got.Reason != want.Reason {
			t.Fatalf("%s example=%+v builtin=%+v", row.ID, got, want)
		}
	}
}
