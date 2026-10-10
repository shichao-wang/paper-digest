package job

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shichao-wang/paper-digest/internal/delivery"
	"github.com/shichao-wang/paper-digest/internal/digest"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/internal/state"
)

const Topic = "recommendation-advertising-search"

type Runner struct {
	Store        *state.Store
	Fetch        func(context.Context) ([]papers.Paper, error)
	Analyzer     digest.Analyzer
	Sender       delivery.Sender
	LookbackDays int
	Now          func() time.Time
	Rules        papers.Rules
}

func BeijingDate(now time.Time) string {
	return now.In(beijing()).Format("2006-01-02")
}

func beijing() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}
	return loc
}

func (r *Runner) Generate(ctx context.Context, date string) error {
	current, err := r.Store.ClaimDay(ctx, Topic, date)
	if err != nil {
		return err
	}
	if current.Status != "new" && current.Status != "processing" {
		return nil
	}
	if current.Status == "new" {
		all, err := r.Fetch(ctx)
		if err != nil {
			return fmt.Errorf("获取 arXiv 论文失败: %w", err)
		}
		var seenErr error
		seen := func(id string) bool {
			yes, err := r.Store.Seen(ctx, Topic, id)
			if err != nil {
				seenErr = err
				return true
			}
			return yes
		}
		selected := papers.Select(all, r.Now(), r.LookbackDays, r.Rules, seen)
		if seenErr != nil {
			return seenErr
		}
		if err := r.Store.SaveCandidates(ctx, Topic, date, selected); err != nil {
			return err
		}
	}
	remaining, err := r.Store.PendingPapers(ctx, Topic, date)
	if err != nil {
		return err
	}
	for _, paper := range remaining {
		if err := ctx.Err(); err != nil {
			return err
		}
		analyzeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		summary, err := r.Analyzer.Analyze(analyzeCtx, paper)
		cancel()
		if err != nil {
			return fmt.Errorf("论文 %s 摘要失败: %w", paper.ID, err)
		}
		if err := r.Store.SaveSummary(ctx, Topic, date, paper.ID, summary); err != nil {
			return err
		}
	}
	completed, err := r.Store.Completed(ctx, Topic, date)
	if err != nil {
		return err
	}
	day, err := time.ParseInLocation("2006-01-02", date, beijing())
	if err != nil {
		return err
	}
	return r.Store.Ready(ctx, Topic, date, digest.Render(day, completed))
}

func (r *Runner) Deliver(ctx context.Context, date string) error {
	if r.Sender == nil {
		return errors.New("发送未启用")
	}
	sender := r.Sender
	if preparer, ok := sender.(delivery.Preparer); ok {
		prepared, err := preparer.Prepare(ctx)
		if err != nil {
			return err
		}
		if prepared == nil {
			return errors.New("发送未启用")
		}
		sender = prepared
	}
	items, err := r.Store.Completed(ctx, Topic, date)
	if err != nil {
		return err
	}
	day, err := time.ParseInLocation("2006-01-02", date, beijing())
	if err != nil {
		return err
	}
	messages := digest.RenderMessages(day, items)
	if validator, ok := sender.(delivery.MarkdownValidator); ok {
		for i, message := range messages {
			if err := validator.ValidateMarkdown(message); err != nil {
				return fmt.Errorf("第 %d/%d 条论文消息预检失败: %w", i+1, len(messages), err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	claimed, err := r.Store.ClaimSend(ctx, Topic, date)
	if err != nil {
		return err
	}
	if !claimed {
		return errors.New("日报未就绪、已发送或发送结果待核对")
	}
	for i, message := range messages {
		// Space a five-paper batch below the robot's five requests/second limit.
		if i > 0 {
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
		send := sender.Send
		if markdown, ok := sender.(delivery.MarkdownSender); ok {
			send = markdown.SendMarkdown
		}
		err := ctx.Err()
		if err == nil {
			err = send(ctx, message)
		}
		if err == nil && len(items) > 0 {
			writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err = r.Store.MarkPaperSent(writeCtx, Topic, date, items[i].Paper.ID)
			cancel()
		}
		if err == nil {
			continue
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if markErr := r.Store.MarkUnknown(writeCtx, Topic, date); markErr != nil {
			return fmt.Errorf("飞书响应未确认且本地状态保存失败，禁止重发: %w", errors.Join(err, markErr))
		}
		return fmt.Errorf("第 %d/%d 条论文消息发送未完成: %w", i+1, len(messages), err)
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.Store.MarkSent(writeCtx, Topic, date); err != nil {
		return fmt.Errorf("飞书已确认成功但本地状态保存失败，禁止重发: %w", err)
	}
	return nil
}
