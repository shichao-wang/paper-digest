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

	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/skills"
)

var topicIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

// builtinSelectionTopic keeps the current recommendation, advertising, and
// search filter when an existing config omits topics[].selection.
const builtinSelectionTopic = "recommendation-advertising-search"

type Topic struct {
	ID         string            `json:"id"`
	WebhookURL string            `json:"webhook_url"`
	Selection  *papers.Selection `json:"selection,omitempty"`
	Skills     *TopicSkills      `json:"skills,omitempty"`
	rules      papers.Rules      `json:"-"`
}

// TopicSkills names the screening and summary skills for one topic.
// Empty names use paper-screening and paper-summary.
type TopicSkills struct {
	Screening string `json:"screening,omitempty"`
	Summary   string `json:"summary,omitempty"`
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
	Skills struct {
		Dir string `json:"dir,omitempty"`
	} `json:"skills,omitempty"`
	Topics []Topic `json:"topics"`
}

func removedArxivField(err error) bool {
	message := err.Error()
	return strings.Contains(message, `unknown field "lookback_days"`) || strings.Contains(message, `unknown field "arxiv"`)
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
		if removedArxivField(err) {
			return Config{}, errors.New("arxiv.lookback_days 已删除，日报改为每个北京时间日期只采集一个 arXiv 公告日。请从配置中去掉 lookback_days；如果 arxiv 段因此没有其他字段，把整个 arxiv 段删掉，保存后执行 docker compose up -d --force-recreate paper-digest")
		}
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
		if topic.Skills != nil {
			topic.Skills.Screening = strings.TrimSpace(topic.Skills.Screening)
			topic.Skills.Summary = strings.TrimSpace(topic.Skills.Summary)
			if (topic.Skills.Screening != "" && !topicIDPattern.MatchString(topic.Skills.Screening)) || (topic.Skills.Summary != "" && !topicIDPattern.MatchString(topic.Skills.Summary)) {
				return Config{}, errors.New("技能名称只能使用小写字母、数字与连字符")
			}
		}
	}
	if baseURL := strings.TrimSpace(cfg.Anthropic.BaseURL); baseURL != "" {
		parsed, err := url.Parse(baseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
			return Config{}, errors.New("anthropic.base_url 必须是有效的 HTTP 或 HTTPS 服务地址，不能包含凭据、查询参数或片段")
		}
		cfg.Anthropic.BaseURL = baseURL
	}
	cfg.Skills.Dir = strings.TrimSpace(cfg.Skills.Dir)
	for i := range cfg.Topics {
		topic := &cfg.Topics[i]
		if topic.Selection == nil {
			if topic.ID == builtinSelectionTopic {
				topic.rules = papers.DefaultRules()
			}
			continue
		}
		compiled, err := papers.Compile(*topic.Selection)
		if err != nil {
			return Config{}, fmt.Errorf("主题 %s 的筛选规则无效: %w", topic.ID, err)
		}
		topic.rules = compiled
	}
	return cfg, nil
}

// TopicSelection returns the compiled rules and the JSON spec for a topic.
// The joint topic uses the built-in rules when selection is omitted, including
// for a Config value that was not loaded from JSON.
func (c Config) TopicSelection(id string) (papers.Rules, papers.Selection, bool) {
	for _, topic := range c.Topics {
		if topic.ID != id {
			continue
		}
		if topic.rules.Active() {
			if topic.Selection != nil {
				return topic.rules, *topic.Selection, true
			}
			if id == builtinSelectionTopic {
				return topic.rules, papers.DefaultSelection(), true
			}
			return topic.rules, papers.Selection{}, true
		}
		if topic.Selection == nil && id == builtinSelectionTopic {
			return papers.DefaultRules(), papers.DefaultSelection(), true
		}
		return papers.Rules{}, papers.Selection{}, false
	}
	return papers.Rules{}, papers.Selection{}, false
}

// SkillNames returns the screening and summary skill names for a topic.
func (c Config) SkillNames(id string) (string, string) {
	screening, summary := skills.Screening, skills.Summary
	for _, topic := range c.Topics {
		if topic.ID != id || topic.Skills == nil {
			continue
		}
		if topic.Skills.Screening != "" {
			screening = topic.Skills.Screening
		}
		if topic.Skills.Summary != "" {
			summary = topic.Skills.Summary
		}
	}
	return screening, summary
}

// ResolveSkills checks that every topic's skills can be loaded.
func (c Config) ResolveSkills(set skills.Set) error {
	for _, topic := range c.Topics {
		screening, summary := c.SkillNames(topic.ID)
		if _, err := set.Get(screening); err != nil {
			return fmt.Errorf("主题 %s 的精筛技能无效: %w", topic.ID, err)
		}
		if _, err := set.Get(summary); err != nil {
			return fmt.Errorf("主题 %s 的摘要技能无效: %w", topic.ID, err)
		}
	}
	return nil
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
