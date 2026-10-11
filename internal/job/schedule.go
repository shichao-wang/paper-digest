package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/shichao-wang/paper-digest/internal/state"
)

// catchUpLimit is how many Shanghai digest dates one morning will generate,
// ending today. A first run has no earlier job and only generates today.
// An outage longer than this leaves the older arXiv days uncovered.
const catchUpLimit = 7

// Serve 在北京时间 08:00～09:00 生成今天以及数据库里尚不存在、且落在最近
// catchUpLimit 天内的日报。每个日期只对应一个 arXiv 公告日。09:00 的一分钟内
// 从早到晚发送这些已就绪的日报；09:01 起不再补发。
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
					dates, err := r.datesToGenerate(workCtx, date)
					if err != nil {
						logger.Error("确定待生成日期失败", "date", date, "error", err)
						return
					}
					for _, digestDate := range dates {
						if err := r.Generate(workCtx, digestDate); err != nil && workCtx.Err() == nil {
							logger.Error("日报生成未完成，稍后重试", "date", digestDate, "error", err)
							return
						}
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
			span, err := recentDigestDates(date)
			if err != nil {
				logger.Error("确定发送日期失败", "date", date, "error", err)
				checkedSend, checkedMissed = "", ""
			} else if err := r.settleSpan(ctx, logger, span, inSendWindow, &work); err != nil {
				logger.Error("检查日报任务失败", "date", date, "error", err)
				checkedSend, checkedMissed = "", ""
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticks:
		}
	}
}

func (r *Runner) datesToGenerate(ctx context.Context, today string) ([]string, error) {
	latest, ok, err := r.Store.LatestJobDate(ctx, Topic)
	if err != nil {
		return nil, err
	}
	return coverDates(today, latest, ok)
}

func (r *Runner) settleSpan(ctx context.Context, logger *slog.Logger, dates []string, inSendWindow bool, work *sync.WaitGroup) error {
	var ready []string
	today := dates[len(dates)-1]
	for _, date := range dates {
		current, err := r.Store.GetJob(ctx, Topic, date)
		if errors.Is(err, state.ErrJobNotFound) {
			if date != today {
				continue
			}
			current, err = r.Store.ClaimDay(ctx, Topic, date)
		}
		if err != nil {
			return err
		}
		if inSendWindow && current.Status == "ready" {
			ready = append(ready, date)
			continue
		}
		if !inSendWindow && (current.Status == "new" || current.Status == "processing" || current.Status == "ready") {
			if err := r.Store.MarkMissed(ctx, Topic, date); err != nil {
				return err
			}
			logger.Error("日报未在北京时间 09:00 就绪或容器错过发送窗口", "date", date)
		}
	}
	if len(ready) == 0 {
		return nil
	}
	work.Add(1)
	go func() {
		defer work.Done()
		for _, date := range ready {
			if err := r.Deliver(ctx, date); err != nil {
				logger.Error("日报发送未确认，请人工核对", "date", date, "error", err)
				return
			}
			logger.Info("日报发送成功", "date", date)
		}
	}()
	return nil
}

func coverDates(today, latest string, hasLatest bool) ([]string, error) {
	if _, err := shiftDate(today, 0); err != nil {
		return nil, err
	}
	start := today
	if hasLatest {
		next, err := shiftDate(latest, 1)
		if err != nil {
			return nil, err
		}
		if next <= today {
			start = next
		}
	}
	floor, err := shiftDate(today, -(catchUpLimit - 1))
	if err != nil {
		return nil, err
	}
	if start < floor {
		start = floor
	}
	return dateSpan(start, today)
}

func recentDigestDates(today string) ([]string, error) {
	floor, err := shiftDate(today, -(catchUpLimit - 1))
	if err != nil {
		return nil, err
	}
	return dateSpan(floor, today)
}

func dateSpan(start, today string) ([]string, error) {
	dates := make([]string, 0, catchUpLimit)
	for date := start; date <= today; {
		dates = append(dates, date)
		if date == today {
			break
		}
		next, err := shiftDate(date, 1)
		if err != nil {
			return nil, err
		}
		date = next
	}
	return dates, nil
}

func shiftDate(date string, days int) (string, error) {
	day, err := time.ParseInLocation("2006-01-02", date, beijing())
	if err != nil || day.Format("2006-01-02") != date {
		return "", fmt.Errorf("invalid date %q", date)
	}
	return day.AddDate(0, 0, days).Format("2006-01-02"), nil
}
