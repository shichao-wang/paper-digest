package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/delivery"
	"github.com/shichao-wang/paper-digest/internal/digest"
	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/internal/state"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

const testMessage = "【论文日报机器人连通性测试】这是一条人工触发的测试消息，不是正式论文日报；未调用模型，也未整理真实论文。"

func sendTest(ctx context.Context, webhook string, client *http.Client) error {
	return (delivery.Feishu{WebhookURL: webhook, Client: client}).Send(ctx, testMessage)
}

func configuredWebhook(topicID string) (string, error) {
	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		path = "config/topics.json"
	}
	topics, err := config.Load(path, job.Topic)
	if err != nil {
		return "", err
	}
	return topics.Webhook(topicID, os.Getenv)
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("用法: paper-digest serve|health|status [日期]|preview <fixture.json>|backup <文件>|send-test --topic <id> --confirm")
	}
	if args[0] == "send-test" {
		if len(args) != 4 || args[1] != "--topic" || args[2] == "" || args[3] != "--confirm" {
			return errors.New("用法: paper-digest send-test --topic <id> --confirm（先确认目标群、机器人身份和测试内容）")
		}
		webhook, err := configuredWebhook(args[2])
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := sendTest(ctx, webhook, nil); err != nil {
			return err
		}
		fmt.Println("飞书已确认接收测试请求；请在目标群核对消息，不要仅凭响应判断送达。")
		return nil
	}
	if args[0] == "preview" {
		if len(args) != 2 {
			return errors.New("用法: paper-digest preview <fixture.json>")
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		var items []digest.Item
		if err := json.Unmarshal(data, &items); err != nil {
			return err
		}
		fmt.Print(digest.Render(time.Now(), items))
		return nil
	}
	path := os.Getenv("DB_PATH")
	if path == "" {
		path = "data/digest.db"
	}
	store, err := state.Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx := context.Background()
	switch args[0] {
	case "health":
		return nil
	case "status":
		date := job.BeijingDate(time.Now())
		if len(args) > 1 {
			date = args[1]
		}
		current, err := store.GetJob(ctx, job.Topic, date)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(current)
	case "backup":
		if len(args) != 2 {
			return errors.New("用法: paper-digest backup <未存在的目标文件>")
		}
		return store.Backup(ctx, args[1])
	case "serve":
		if os.Getenv("ENABLE_DELIVERY") != "true" {
			slog.Info("真实运行未启用；容器仅供状态检查与离线预览")
			wait, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			<-wait.Done()
			return nil
		}
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			return errors.New("启用真实运行需要 ANTHROPIC_API_KEY")
		}
		webhook, err := configuredWebhook(job.Topic)
		if err != nil {
			return err
		}
		lookback := 7
		if value := os.Getenv("ARXIV_LOOKBACK_DAYS"); value != "" {
			lookback, err = strconv.Atoi(value)
			if err != nil || lookback < 1 || lookback > 30 {
				return errors.New("ARXIV_LOOKBACK_DAYS 必须为 1 到 30")
			}
		}
		model := os.Getenv("ANTHROPIC_MODEL")
		if model == "" {
			model = "claude-opus-5"
		}
		runner := &job.Runner{
			Store: store,
			Fetch: func(ctx context.Context) ([]papers.Paper, error) {
				fetchCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
				defer cancel()
				return papers.Fetch(fetchCtx, &http.Client{Timeout: 40 * time.Second}, "", 100)
			},
			Analyzer:     digest.ClaudeAnalyzer{Model: model},
			Sender:       delivery.Feishu{WebhookURL: webhook},
			LookbackDays: lookback,
			Now:          time.Now,
		}
		wait, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := runner.Serve(wait, slog.Default()); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	default:
		return fmt.Errorf("未知命令 %q", args[0])
	}
}
