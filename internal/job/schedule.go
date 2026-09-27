package job

import (
	"context"
	"log/slog"
	"time"
)

// Serve 只在本机容器内调度；重启后仅恢复当天且仍在发送时间前的生成任务。
func (r *Runner) Serve(ctx context.Context, logger *slog.Logger) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var day, checkedSend string
	var nextAttempt time.Time
	running := make(chan struct{}, 1)
	for {
		now := r.Now().In(beijing())
		date := BeijingDate(now)
		if day != date {
			day, checkedSend = date, ""
			nextAttempt = time.Time{}
		}
		if now.Hour() >= 8 && now.Hour() < 9 && !now.Before(nextAttempt) {
			select {
			case running <- struct{}{}:
				nextAttempt = now.Add(2 * time.Minute)
				go func() {
					defer func() { <-running }()
					deadline := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, beijing())
					workCtx, cancel := context.WithDeadline(ctx, deadline)
					defer cancel()
					if err := r.Generate(workCtx, date); err != nil && workCtx.Err() == nil {
						logger.Error("日报生成未完成，稍后重试", "date", date, "error", err)
					}
				}()
			default:
			}
		}
		if now.Hour() >= 9 && checkedSend != date {
			checkedSend = date
			if _, err := r.Store.ClaimDay(ctx, Topic, date); err != nil {
				logger.Error("检查日报任务失败", "date", date, "error", err)
				checkedSend = ""
			} else if current, err := r.Store.GetJob(ctx, Topic, date); err != nil {
				logger.Error("读取日报状态失败", "date", date, "error", err)
				checkedSend = ""
			} else if now.Hour() == 9 && now.Minute() == 0 && current.Status == "ready" {
				// 发送在后台运行，以免阻塞调度；发送意图会先写入 SQLite。
				go func() {
					if err := r.Deliver(ctx, date); err != nil {
						logger.Error("日报发送未确认，请人工核对", "date", date, "error", err)
						return
					}
					logger.Info("日报发送成功", "date", date)
				}()
			} else if current.Status != "sent" && current.Status != "unknown" && current.Status != "sending" {
				if err := r.Store.MarkMissed(ctx, Topic, date); err != nil {
					logger.Error("记录漏发失败", "date", date, "error", err)
					checkedSend = ""
				} else {
					logger.Error("日报未在北京时间 09:00 就绪或容器错过发送窗口", "date", date)
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
