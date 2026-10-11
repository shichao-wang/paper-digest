package job

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Serve 只处理当天的北京时间日期。08:00～09:00 创建或恢复当天任务；
// 09:00 的一分钟内只发送当天已就绪的日报。09:01 起把当天未完成的任务记为 missed，不补发，也不补停机期间错过的日期。
func (r *Runner) Serve(ctx context.Context, logger *slog.Logger) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	return r.serve(ctx, logger, ticker.C)
}

func (r *Runner) serve(ctx context.Context, logger *slog.Logger, ticks <-chan time.Time) error {
	var day, checkedSend, checkedMissed string
	var nextAttempt time.Time
	running := make(chan struct{}, 1)
	var work sync.WaitGroup
	// Deliver 取消后仍可能写入发送结果；等待任务结束后，调用方才能关闭 Store。
	defer work.Wait()
	for {
		now := r.Now().In(beijing())
		date := BeijingDate(now)
		if day != date {
			day, checkedSend, checkedMissed = date, "", ""
			nextAttempt = time.Time{}
		}
		if now.Hour() >= 8 && now.Hour() < 9 && !now.Before(nextAttempt) {
			select {
			case running <- struct{}{}:
				nextAttempt = now.Add(2 * time.Minute)
				work.Add(1)
				go func() {
					defer work.Done()
					defer func() { <-running }()
					deadline := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, beijing())
					workCtx, cancel := context.WithDeadline(ctx, deadline)
					defer cancel()
					if err := r.Generate(workCtx, date); err != nil && errors.Is(err, errScreeningClosed) {
						logger.Error("精筛失败，当天不发送", "date", date, "error", err)
					} else if err != nil && workCtx.Err() == nil {
						logger.Error("日报生成未完成，稍后重试", "date", date, "error", err)
					}
				}()
			default:
			}
		}
		inSendWindow := now.Hour() == 9 && now.Minute() == 0
		// 窗口内只尝试一次发送；窗口结束后仍须单独收口未发出的 ready 任务。
		if now.Hour() >= 9 && ((inSendWindow && checkedSend != date) || (!inSendWindow && checkedMissed != date)) {
			if inSendWindow {
				checkedSend = date
			} else {
				checkedMissed = date
			}
			if _, err := r.Store.ClaimDay(ctx, Topic, date); err != nil {
				logger.Error("检查日报任务失败", "date", date, "error", err)
				checkedSend, checkedMissed = "", ""
			} else if current, err := r.Store.GetJob(ctx, Topic, date); err != nil {
				logger.Error("读取日报状态失败", "date", date, "error", err)
				checkedSend, checkedMissed = "", ""
			} else if inSendWindow && current.Status == "ready" {
				// 发送在后台运行，以免阻塞调度；发送意图会先写入 SQLite。
				work.Add(1)
				go func() {
					defer work.Done()
					if err := r.Deliver(ctx, date); err != nil {
						logger.Error("日报发送未确认，请人工核对", "date", date, "error", err)
						return
					}
					logger.Info("日报发送成功", "date", date)
				}()
			} else if current.Status != "sent" && current.Status != "unknown" && current.Status != "sending" {
				if err := r.Store.MarkMissed(ctx, Topic, date); err != nil {
					logger.Error("记录漏发失败", "date", date, "error", err)
					checkedSend, checkedMissed = "", ""
				} else {
					logger.Error("日报未在北京时间 09:00 就绪或容器错过发送窗口", "date", date)
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticks:
		}
	}
}
