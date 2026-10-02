package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/shichao-wang/paper-digest/internal/arxivclient"
)

var topicIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

type Topic struct {
	ID         string `json:"id"`
	WebhookURL string `json:"webhook_url"`
}

type Library struct {
	CollectEnabled     bool     `json:"collect_enabled"`
	ProcessEnabled     bool     `json:"process_enabled"`
	Categories         []string `json:"categories"`
	DocumentDir        string   `json:"document_dir"`
	Concurrency        int      `json:"concurrency"`
	PollSeconds        int      `json:"poll_seconds"`
	MaxRequests        int      `json:"max_requests"`
	MaxTokens          int64    `json:"max_tokens"`
	TaskTimeoutSeconds int      `json:"task_timeout_seconds"`
}

func (l *Library) defaults(databasePath string) error {
	if len(l.Categories) == 0 {
		l.Categories = arxivclient.AnnouncementCategories()
	}
	if l.DocumentDir == "" {
		var err error
		l.DocumentDir, err = defaultDocumentDir(databasePath)
		if err != nil {
			return err
		}
	}
	if l.Concurrency == 0 {
		l.Concurrency = 1
	}
	if l.PollSeconds == 0 {
		l.PollSeconds = 900
	}
	if l.MaxRequests == 0 {
		l.MaxRequests = 120
	}
	if l.MaxTokens == 0 {
		l.MaxTokens = 500000
	}
	if l.TaskTimeoutSeconds == 0 {
		l.TaskTimeoutSeconds = 1800
	}
	return nil
}

// defaultDocumentDir 使用 SQLite file: URI 的文件路径；内存库没有相邻目录，
// 与普通相对路径一样默认将文档保存在当前目录下的 library。
func defaultDocumentDir(databasePath string) (string, error) {
	if databasePath == ":memory:" {
		return "library", nil
	}
	if strings.HasPrefix(databasePath, "file:") {
		invalidURI := errors.New("无法从 database.path 推导 library.document_dir；请配置有效的 SQLite 文件 URI 或显式文档目录")
		u, err := url.Parse(databasePath)
		if err != nil {
			return "", invalidURI
		}
		path := u.Path
		if u.Opaque != "" {
			path, err = url.PathUnescape(u.Opaque)
			if err != nil {
				return "", invalidURI
			}
		}
		if path == ":memory:" || u.Query().Get("mode") == "memory" {
			return "library", nil
		}
		if path == "" || (u.Host != "" && u.Host != "localhost") {
			return "", invalidURI
		}
		databasePath = filepath.FromSlash(path)
	}
	return filepath.Join(filepath.Dir(databasePath), "library"), nil
}

func (l Library) Validate() error {
	if l.Concurrency < 1 || l.Concurrency > 8 || l.PollSeconds < 60 || l.MaxRequests < 1 || l.MaxRequests > 1000 || l.MaxTokens < 4096 || l.TaskTimeoutSeconds < 60 || l.TaskTimeoutSeconds > 7200 || strings.TrimSpace(l.DocumentDir) == "" {
		return errors.New("library 的并发、轮询、预算、超时或文档目录不合法")
	}
	seen := map[string]bool{}
	for _, c := range l.Categories {
		if !arxivclient.SupportsAnnouncementCategory(c) || seen[c] {
			return errors.New("library.categories 包含不支持或重复分类；支持 cs.IR、cs.LG、cs.AI、cs.CL、stat.ML")
		}
		seen[c] = true
	}
	return nil
}

type Config struct {
	Library  Library `json:"library"`
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
	if err := cfg.Library.defaults(cfg.Database.Path); err != nil {
		return Config{}, err
	}
	if err := cfg.Library.Validate(); err != nil {
		return Config{}, err
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
