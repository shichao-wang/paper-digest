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
var webhookEnvPattern = regexp.MustCompile(`^FEISHU_WEBHOOK_[A-Z][A-Z0-9_]*$`)

type topic struct {
	ID         string `json:"id"`
	WebhookEnv string `json:"webhook_env"`
}

type file struct {
	Topics []topic `json:"topics"`
}

type Topics struct {
	webhookEnvByID map[string]string
}

func Load(path, requiredTopic string) (Topics, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Topics{}, fmt.Errorf("读取主题配置失败: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var source file
	if err := decoder.Decode(&source); err != nil {
		return Topics{}, errors.New("解析主题配置失败：JSON 格式或字段不合法")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Topics{}, errors.New("主题配置只能包含一个 JSON 对象")
	}
	if len(source.Topics) == 0 {
		return Topics{}, errors.New("主题配置至少需要一个主题")
	}
	result := Topics{webhookEnvByID: make(map[string]string, len(source.Topics))}
	usedEnv := make(map[string]bool, len(source.Topics))
	for _, entry := range source.Topics {
		if !topicIDPattern.MatchString(entry.ID) {
			return Topics{}, errors.New("主题 ID 不能为空，且只能使用小写字母、数字与连字符")
		}
		if !webhookEnvPattern.MatchString(entry.WebhookEnv) {
			return Topics{}, errors.New("webhook_env 必须是 FEISHU_WEBHOOK_ 开头的环境变量名")
		}
		if _, exists := result.webhookEnvByID[entry.ID]; exists {
			return Topics{}, errors.New("主题 ID 重复")
		}
		if usedEnv[entry.WebhookEnv] {
			return Topics{}, errors.New("机器人环境变量名重复")
		}
		result.webhookEnvByID[entry.ID] = entry.WebhookEnv
		usedEnv[entry.WebhookEnv] = true
	}
	if _, exists := result.webhookEnvByID[requiredTopic]; !exists {
		return Topics{}, errors.New("主题配置缺少当前已实现的日报主题")
	}
	return result, nil
}

func (t Topics) Webhook(topicID string, getenv func(string) string) (string, error) {
	name, exists := t.webhookEnvByID[topicID]
	if !exists {
		return "", errors.New("未知主题；请检查主题配置")
	}
	webhook := strings.TrimSpace(getenv(name))
	if webhook == "" {
		return "", fmt.Errorf("缺少机器人环境变量 %s；请先确认目标群和机器人身份", name)
	}
	parsed, err := url.Parse(webhook)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", fmt.Errorf("%s 必须是有效的 HTTPS Webhook URL", name)
	}
	return webhook, nil
}
