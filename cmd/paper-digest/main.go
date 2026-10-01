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

func run(args []string) error {
	configPath := "config/config.json"
	if len(args) > 0 && args[0] == "--config" {
		if len(args) < 3 || args[1] == "" {
			return errors.New("用法: paper-digest --config <文件> <命令>")
		}
		configPath, args = args[1], args[2:]
	}
	if len(args) == 0 {
		return errors.New("用法: paper-digest [--config <文件>] serve|health|status [日期]|preview <fixture.json>|backup <文件>|send-test --topic <id> --confirm")
	}
	if args[0] == "send-test" {
		if len(args) != 4 || args[1] != "--topic" || args[2] == "" || args[3] != "--confirm" {
			return errors.New("用法: paper-digest send-test --topic <id> --confirm（先确认目标群、机器人身份和测试内容）")
		}
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		webhook, err := cfg.Webhook(args[2])
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
	if args[0] == "health" {
		return nil
	}
	if args[0] != "status" && args[0] != "backup" && args[0] != "serve" {
		return fmt.Errorf("未知命令 %q", args[0])
	}
	if args[0] == "backup" && len(args) != 2 {
		return errors.New("用法: paper-digest backup <未存在的目标文件>")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if args[0] == "serve" && !cfg.Delivery.Enabled {
		slog.Info("真实运行未启用；容器仅供状态检查与离线预览")
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()
		return nil
	}
	path, err := cfg.DatabasePath()
	if err != nil {
		return err
	}
	var webhook string
	if args[0] == "serve" {
		webhook, err = cfg.ValidateDelivery(job.Topic)
		if err != nil {
			return err
		}
	}
	store, err := state.Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx := context.Background()
	switch args[0] {
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
		return store.Backup(ctx, args[1])
	case "serve":
		model := cfg.Anthropic.Model
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
			Analyzer:     digest.ClaudeAnalyzer{Model: model, APIKey: cfg.Anthropic.APIKey, BaseURL: cfg.Anthropic.BaseURL},
			Sender:       delivery.Feishu{WebhookURL: webhook},
			LookbackDays: cfg.Arxiv.LookbackDays,
			Now:          time.Now,
		}
		wait, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := runner.Serve(wait, slog.Default()); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}
	return nil
}
