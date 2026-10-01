package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/delivery"
	"github.com/shichao-wang/paper-digest/internal/digest"
	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/internal/state"
	"github.com/shichao-wang/paper-digest/internal/web"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

const testMessage = "【论文日报机器人连通性测试】这是一条人工触发的测试消息，不是正式论文日报；未调用模型，也未整理真实论文。"

func sendTest(ctx context.Context, store *state.Store, topic string, client *http.Client) error {
	return (delivery.StoredFeishu{Store: store, Topic: topic, Client: client}).Send(ctx, testMessage)
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
		knownTopic := false
		for _, topic := range cfg.Topics {
			if topic.ID == args[2] {
				knownTopic = true
				break
			}
		}
		if !knownTopic {
			return errors.New("未知主题；请检查主题配置")
		}
		store, err := openStore(context.Background(), cfg)
		if err != nil {
			return err
		}
		defer store.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := sendTest(ctx, store, args[2], nil); err != nil {
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
		flags := flag.NewFlagSet("health", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		endpoint := flags.String("url", "http://127.0.0.1:8080/api/health", "health endpoint")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("用法: paper-digest health [--url <地址>]")
		}
		return checkHealth(context.Background(), *endpoint, &http.Client{Timeout: 5 * time.Second})
	}
	if args[0] != "status" && args[0] != "backup" && args[0] != "serve" {
		return fmt.Errorf("未知命令 %q", args[0])
	}
	if args[0] == "backup" && len(args) != 2 {
		return errors.New("用法: paper-digest backup <未存在的目标文件>")
	}
	options := serveOptions{Listen: "127.0.0.1:8080", WebDir: "web/dist"}
	if args[0] == "serve" {
		flags := flag.NewFlagSet("serve", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		flags.StringVar(&options.Listen, "listen", options.Listen, "HTTP listen address")
		flags.StringVar(&options.WebDir, "web-dir", options.WebDir, "static web directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || options.Listen == "" || options.WebDir == "" {
			return errors.New("用法: paper-digest serve [--listen <地址>] [--web-dir <目录>]")
		}
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if args[0] == "serve" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return serve(ctx, cfg, options, serveDependencies{})
	}
	path, err := cfg.DatabasePath()
	if err != nil {
		return err
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
	}
	return nil
}

type serveOptions struct {
	Listen string
	WebDir string
}

type worker interface {
	Serve(context.Context, *slog.Logger) error
}

type serveDependencies struct {
	StaticFS  fs.FS
	Listen    func(string, string) (net.Listener, error)
	NewWorker func(config.Config, *state.Store) (worker, error)
}

func defaultWorker(cfg config.Config, store *state.Store) (worker, error) {
	model := cfg.Anthropic.Model
	if model == "" {
		model = "claude-opus-5"
	}
	return &job.Runner{
		Store: store,
		Fetch: func(ctx context.Context) ([]papers.Paper, error) {
			fetchCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
			defer cancel()
			return papers.Fetch(fetchCtx, &http.Client{Timeout: 40 * time.Second}, "", 100)
		},
		Analyzer: digest.ClaudeAnalyzer{Model: model, APIKey: cfg.Anthropic.APIKey, BaseURL: cfg.Anthropic.BaseURL},
		Sender:   delivery.StoredFeishu{Store: store, Topic: job.Topic}, LookbackDays: cfg.Arxiv.LookbackDays, Now: time.Now,
	}, nil
}

func openStore(ctx context.Context, cfg config.Config) (*state.Store, error) {
	path, err := cfg.DatabasePath()
	if err != nil {
		return nil, err
	}
	store, err := state.Open(path)
	if err != nil {
		return nil, err
	}
	if err := store.MigrateWebhooks(ctx, cfg.Topics); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func serve(parent context.Context, cfg config.Config, options serveOptions, deps serveDependencies) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if cfg.Delivery.Enabled {
		if err := cfg.ValidateDelivery(job.Topic); err != nil {
			return err
		}
	}
	store, err := openStore(parent, cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	staticFS := deps.StaticFS
	if staticFS == nil {
		staticFS = os.DirFS(options.WebDir)
	}
	handler, err := web.New(store, staticFS, web.Options{DeliveryEnabled: cfg.Delivery.Enabled})
	if err != nil {
		return err
	}
	var runner worker
	if cfg.Delivery.Enabled {
		factory := deps.NewWorker
		if factory == nil {
			factory = defaultWorker
		}
		runner, err = factory(cfg, store)
		if err != nil {
			return err
		}
		if runner == nil {
			return errors.New("worker initialization returned nil")
		}
	}
	listen := deps.Listen
	if listen == nil {
		listen = net.Listen
	}
	listener, err := listen("tcp", options.Listen)
	if err != nil {
		return fmt.Errorf("listen HTTP: %w", err)
	}
	defer listener.Close()
	// 启动校验与端口绑定成功后，才恢复状态和启动 worker。
	if runner != nil {
		if err := store.RecoverInterruptedSends(parent); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var handlers sync.WaitGroup
	var handlerMu sync.Mutex
	var drained bool
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerMu.Lock()
			if drained {
				handlerMu.Unlock()
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
			handlers.Add(1)
			handlerMu.Unlock()
			defer handlers.Done()
			handler.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second,
		IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx },
	}
	httpDone := make(chan error, 1)
	go func() { httpDone <- server.Serve(listener) }()
	var workerDone chan error
	if runner != nil {
		workerDone = make(chan error, 1)
		go func() { workerDone <- runner.Serve(ctx, slog.Default()) }()
	}
	var result error
	var httpStopped, workerStopped bool
	select {
	case <-parent.Done():
	case result = <-httpDone:
		httpStopped = true
		if errors.Is(result, http.ErrServerClosed) {
			result = nil
		}
	case result = <-workerDone:
		workerStopped = true
		if parent.Err() != nil && errors.Is(result, parent.Err()) {
			result = nil
		} else if result == nil {
			result = errors.New("worker stopped unexpectedly")
		}
	}
	cancel()
	shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := server.Shutdown(shutdownCtx); err != nil {
		// 关闭连接取消未结束的请求；仍须等 handler 返回再关闭 Store。
		_ = server.Close()
		result = errors.Join(result, fmt.Errorf("shutdown HTTP: %w", err))
	}
	if !httpStopped {
		if err := <-httpDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			result = errors.Join(result, err)
		}
	}
	handlerMu.Lock()
	drained = true
	handlerMu.Unlock()
	handlers.Wait()
	if workerDone != nil && !workerStopped {
		if err := <-workerDone; err != nil && !errors.Is(err, context.Canceled) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func checkHealth(ctx context.Context, endpoint string, client *http.Client) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("health URL must be a valid HTTP or HTTPS address")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("HTTP health check failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP health check returned status %d", response.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4096))
	if err := decoder.Decode(&body); err != nil || body.Status != "ok" {
		return errors.New("invalid HTTP health response")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("invalid HTTP health response")
	}
	return nil
}
