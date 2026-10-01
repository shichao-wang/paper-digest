package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
)

var topicIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

type Topic struct {
	ID         string `json:"id"`
	WebhookURL string `json:"webhook_url"`
}

type Config struct {
	Database struct {
		Path string `json:"path"`
	} `json:"database"`
	Delivery struct {
		Enabled bool `json:"enabled"`
	} `json:"delivery"`
	Anthropic struct {
		APIKey  string `json:"api_key"`
		Model   string `json:"model"`
		BaseURL string `json:"base_url"`
	} `json:"anthropic"`
	Arxiv struct {
		LookbackDays int `json:"lookback_days"`
	} `json:"arxiv"`
	Topics []Topic `json:"topics"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置文件失败: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, errors.New("解析配置失败：JSON 格式或字段不合法")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Config{}, errors.New("配置只能包含一个 JSON 对象")
	}
	if len(cfg.Topics) == 0 {
		return Config{}, errors.New("配置至少需要一个主题")
	}
	seen := make(map[string]bool, len(cfg.Topics))
	for _, topic := range cfg.Topics {
		if !topicIDPattern.MatchString(topic.ID) {
			return Config{}, errors.New("主题 ID 不能为空，且只能使用小写字母、数字与连字符")
		}
		if seen[topic.ID] {
			return Config{}, errors.New("主题 ID 重复")
		}
		seen[topic.ID] = true
	}
	if baseURL := strings.TrimSpace(cfg.Anthropic.BaseURL); baseURL != "" {
		parsed, err := url.Parse(baseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
			return Config{}, errors.New("anthropic.base_url 必须是有效的 HTTP 或 HTTPS 服务地址，不能包含凭据、查询参数或片段")
		}
		cfg.Anthropic.BaseURL = baseURL
	}
	if cfg.Arxiv.LookbackDays < 1 || cfg.Arxiv.LookbackDays > 30 {
		return Config{}, errors.New("arxiv.lookback_days 必须为 1 到 30")
	}
	return cfg, nil
}

func (c Config) Webhook(topicID string) (string, error) {
	for _, topic := range c.Topics {
		if topic.ID != topicID {
			continue
		}
		webhook := strings.TrimSpace(topic.WebhookURL)
		if webhook == "" {
			return "", errors.New("缺少主题 Webhook；请先确认目标群和机器人身份")
		}
		if err := ValidateWebhookURL(webhook); err != nil {
			return "", err
		}
		return webhook, nil
	}
	return "", errors.New("未知主题；请检查主题配置")
}

func (c Config) DatabasePath() (string, error) {
	if strings.TrimSpace(c.Database.Path) == "" {
		return "", errors.New("缺少 database.path")
	}
	return c.Database.Path, nil
}

// ValidateWebhookURL 不回显地址，供兼容配置、页面设置与发送复用。
func ValidateWebhookURL(webhook string) error {
	parsed, err := url.Parse(webhook)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || strings.Contains(webhook, "#") {
		return errors.New("主题 Webhook 必须是有效的 HTTPS URL，不能包含凭据或片段")
	}
	return nil
}

// ValidateDelivery 校验真实运行所需的密钥与主题身份，Webhook 允许稍后从页面设置。
func (c Config) ValidateDelivery(topicID string) error {
	if strings.TrimSpace(c.Anthropic.APIKey) == "" {
		return errors.New("启用真实运行需要 anthropic.api_key")
	}
	for _, topic := range c.Topics {
		if topic.ID == topicID {
			return nil
		}
	}
	return errors.New("未知主题；请检查主题配置")
}
