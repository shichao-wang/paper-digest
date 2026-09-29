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
		APIKey string `json:"api_key"`
		Model  string `json:"model"`
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
		parsed, err := url.Parse(webhook)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			return "", errors.New("主题 Webhook 必须是有效的 HTTPS URL")
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

func (c Config) ValidateDelivery(topicID string) (string, error) {
	if strings.TrimSpace(c.Anthropic.APIKey) == "" {
		return "", errors.New("启用真实运行需要 anthropic.api_key")
	}
	return c.Webhook(topicID)
}
