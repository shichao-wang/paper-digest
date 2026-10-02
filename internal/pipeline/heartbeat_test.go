package pipeline

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/library"
)

func awaitHeartbeat(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("心跳未及时结束")
		return nil
	}
}

func TestTaskHeartbeatCancellationDuringRenewal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		renewErr error
		want     error
	}{
		{"完成导致取消", context.Canceled, nil},
		{"包装的取消", fmt.Errorf("续租: %w", context.Canceled), nil},
		{"超时结束", context.DeadlineExceeded, nil},
		{"取消后真实失租", library.ErrLease, library.ErrLease},
		{"混合失租和取消", errors.Join(context.Canceled, library.ErrLease), library.ErrLease},
		{"取消后其他存储错误", library.ErrInvalid, library.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ticks := make(chan time.Time, 1)
			ticks <- time.Now()
			renewing := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- taskHeartbeat(ctx, ticks, func() error {
					close(renewing)
					// 确定性模拟续租已开始后，stage 返回并取消任务上下文。
					<-ctx.Done()
					return tc.renewErr
				})
			}()
			select {
			case <-renewing:
			case <-time.After(2 * time.Second):
				t.Fatal("续租未开始")
			}
			cancel()
			err := awaitHeartbeat(t, done)
			if tc.want == nil && err != nil {
				t.Fatalf("正常退出误报错误: %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("丢失真实错误: %v", err)
			}
		})
	}
}

func TestTaskHeartbeatPreservesUnexpectedCancellation(t *testing.T) {
	ctx := context.Background()
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	done := make(chan error, 1)
	go func() { done <- taskHeartbeat(ctx, ticks, func() error { return context.Canceled }) }()
	if err := awaitHeartbeat(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("吞掉非任务取消错误: %v", err)
	}
}

func TestTaskHeartbeatCompletionStillRequiresValidLease(t *testing.T) {
	for _, loseLease := range []bool{false, true} {
		t.Run(fmt.Sprint(loseLease), func(t *testing.T) {
			id := pipelineIdentity(1, "v1")
			runner, _, _, _ := pipelineTestRunner(t, []library.CategoryBatch{pipelineBatch(id)})
			if err := runner.Collect(context.Background()); err != nil {
				t.Fatal(err)
			}
			task, err := runner.Store.ClaimTask(context.Background(), "metadata", pipelineTestNow, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ticks := make(chan time.Time, 1)
			ticks <- pipelineTestNow
			renewalStarted := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- taskHeartbeat(ctx, ticks, func() error { close(renewalStarted); <-ctx.Done(); return ctx.Err() })
			}()
			<-renewalStarted
			cancel()
			if err := awaitHeartbeat(t, done); err != nil {
				t.Fatalf("误报取消: %v", err)
			}
			// 心跳正常结束后仍调用原有完成事务；过期 token 不会因此得到豁免。
			now := pipelineTestNow
			if loseLease {
				now = now.Add(2 * time.Minute)
				if _, err := runner.Store.ClaimTask(context.Background(), "metadata", now, time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			err = runner.Store.CompleteTask(context.Background(), task, library.Completion{}, now)
			if loseLease && !errors.Is(err, library.ErrLease) {
				t.Fatalf("过期租约被允许完成: %v", err)
			}
			if !loseLease && err != nil {
				t.Fatalf("有效租约未完成: %v", err)
			}
		})
	}
}

func TestTaskHeartbeatCancellationAllowsFailurePersistence(t *testing.T) {
	id := pipelineIdentity(1, "v1")
	runner, _, _, _ := pipelineTestRunner(t, []library.CategoryBatch{pipelineBatch(id)})
	if err := runner.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	task, err := runner.Store.ClaimTask(context.Background(), "metadata", pipelineTestNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time, 1)
	ticks <- pipelineTestNow
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- taskHeartbeat(ctx, ticks, func() error { close(started); <-ctx.Done(); return ctx.Err() })
	}()
	<-started
	cancel()
	if err := awaitHeartbeat(t, done); err != nil {
		t.Fatal(err)
	}
	if err := runner.Store.FailTask(context.Background(), task, "retry_wait", "阶段暂不可用", pipelineTestNow.Add(time.Minute), pipelineTestNow); err != nil {
		t.Fatal(err)
	}
	detail := pipelineDetail(t, runner, id)
	if got := pipelineTask(t, detail, "metadata"); got.Status != "retry_wait" {
		t.Fatalf("失败状态未落盘: %#v", got)
	}
}
