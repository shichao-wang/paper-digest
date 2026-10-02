package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shichao-wang/paper-digest/internal/analysis"
	"github.com/shichao-wang/paper-digest/internal/document"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/modelchat"
)

func (r *Runner) execute(parent context.Context, t library.Task) error {
	timeout := time.Duration(r.Config.TaskTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	heartbeatDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		err := taskHeartbeat(ctx, ticker.C, func() error {
			return r.Store.RenewLease(ctx, t, r.now(), 2*time.Minute)
		})
		if err != nil {
			cancel()
		}
		heartbeatDone <- err
	}()
	completion, taskErr := r.stage(ctx, t)
	cancel()
	leaseErr := <-heartbeatDone
	if leaseErr != nil {
		return leaseErr
	}
	// 任务上下文超时后仍须短暂持久化失败状态，正常完成同样校验租约。
	saveCtx, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer stop()
	if taskErr == nil {
		return r.Store.CompleteTask(saveCtx, t, completion, r.now())
	}
	if errors.Is(taskErr, library.ErrLease) {
		return taskErr
	}
	status := "retry_wait"
	reason := "阶段请求或依赖暂不可用，将按退避重试"
	switch {
	case errors.Is(taskErr, library.ErrPaused), errors.Is(taskErr, modelchat.ErrBudget):
		status = "paused"
		reason = "累计请求或 token 预算已用尽；普通重试保留预算，重解析需新代次"
	case errors.Is(taskErr, library.ErrQuality), errors.Is(taskErr, document.ErrIntegrity):
		status = "blocked"
		reason = "全文提取质量或持久文件完整性不合格，禁止摘要降级"
	case errors.Is(taskErr, analysis.ErrValidation), errors.Is(taskErr, library.ErrInvalid):
		status = "blocked"
		reason = "结构、证据、版本或检查点未通过本地校验"
	case errors.Is(taskErr, modelchat.ErrProtocol), errors.Is(taskErr, modelchat.ErrToolRounds):
		status = "blocked"
		reason = "模型协议或工具阶段未正常完成"
	case parent.Err() != nil:
		reason = "服务已停止；检查点已保存，下次继续"
	}
	delay := time.Minute * time.Duration(1<<min(t.Attempt, 6))
	next := r.now().Add(delay)
	if status != "retry_wait" {
		next = time.Time{}
	}
	if err := r.Store.FailTask(saveCtx, t, status, reason, next, r.now()); err != nil {
		return err
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	return nil
}

// taskHeartbeat 区分正常上下文结束与真实失租，完成落盘仍由存储层校验租约。
// ticks 与 renew 可注入测试，确定复现续租过程中任务完成并取消上下文的竞态。
func taskHeartbeat(ctx context.Context, ticks <-chan time.Time, renew func() error) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticks:
			if err := renew(); err != nil {
				// 已取消上下文可能令正在执行的续租返回取消错误，不能误报失租。
				// ErrLease 优先保留，即使它与取消错误同时出现在错误链中。
				if !errors.Is(err, library.ErrLease) && ctx.Err() != nil &&
					(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
					return nil
				}
				return err
			}
		}
	}
}

func (r *Runner) stage(ctx context.Context, t library.Task) (library.Completion, error) {
	var c library.Completion
	v, err := r.Store.GetVersion(ctx, t.Identity)
	if err != nil {
		return c, err
	}
	var cp analysis.Checkpoint
	if len(t.Checkpoint) > 0 {
		if err := json.Unmarshal(t.Checkpoint, &cp); err != nil {
			return c, fmt.Errorf("checkpoint: %w", library.ErrInvalid)
		}
	}
	save := func(cp analysis.Checkpoint) error {
		raw, err := json.Marshal(cp)
		if err != nil {
			return err
		}
		return r.Store.SaveCheckpoint(ctx, t, raw, r.now())
	}
	chunkSave := func(chunk library.Chunk) error { return r.Store.SaveChunk(ctx, t, chunk, r.now()) }
	switch t.Stage {
	case "metadata":
		meta, artifacts, err := r.Source.FetchVersionMetadata(ctx, t.Identity)
		if err != nil {
			return c, err
		}
		meta.MetadataArtifacts = artifacts
		c.Version = &meta
		c.Next = []string{"relevance"}
	case "relevance":
		rel, run, err := r.Analyzer.Screen(ctx, v, cp, save)
		if err != nil {
			return c, err
		}
		c.Relevance = &rel
		c.Run = run
		if rel.DirectlyRelated {
			c.Next = []string{"document"}
		}
	case "document":
		doc, err := r.Documents.Ensure(ctx, t.Identity)
		if errors.Is(err, library.ErrQuality) && doc.ID != "" {
			if saveErr := r.Store.SaveTaskDocument(ctx, t, doc, r.now()); saveErr != nil {
				return c, saveErr
			}
		}
		if err != nil {
			return c, err
		}
		if doc.Quality != "ready" {
			return c, library.ErrQuality
		}
		c.Document = &doc
		c.Next = []string{"analyze"}
	case "analyze":
		doc, err := r.currentDocument(ctx, t, nil)
		if err != nil {
			return c, err
		}
		chunks, err := r.Store.Chunks(ctx, t)
		if err != nil {
			return c, err
		}
		a, run, err := r.Analyzer.Analyze(ctx, v, doc, chunks, cp, save, chunkSave)
		if err != nil {
			return c, err
		}
		c.Analysis = &a
		c.Relevance = &a.Content.Relevance
		c.Run = run
		if _, ok := v.Previous(); ok {
			c.Next = []string{"compare"}
		} else {
			c.Comparison = &library.Comparison{Status: "not_applicable", Reason: "首版没有上一版本", DocumentHashes: []string{}, Content: library.ComparisonContent{Changes: []library.Change{}, Evidence: []library.Evidence{}}}
		}
	case "compare":
		boundAnalysis, analysisID, err := r.Store.AnalysisForTask(ctx, t)
		if err != nil {
			return c, err
		}
		if len(boundAnalysis.DocumentHashes) != 2 {
			return c, fmt.Errorf("comparison analysis document binding: %w", library.ErrInvalid)
		}
		current, err := r.currentDocument(ctx, t, boundAnalysis.DocumentHashes)
		if err != nil {
			return c, err
		}
		prev, ok := v.Previous()
		if !ok {
			return c, library.ErrInvalid
		}
		previous, err := r.Store.TaskDocument(ctx, t, prev)
		if errors.Is(err, library.ErrNotFound) {
			prior, artifacts, err := r.Source.FetchVersionMetadata(ctx, prev)
			if err != nil {
				return c, err
			}
			prior.Origin = "comparison_reference"
			prior.MetadataArtifacts = artifacts
			if err := r.Store.UpsertVersion(ctx, prior); err != nil {
				return c, err
			}
			previous, err = r.Documents.Ensure(ctx, prev)
			if err != nil {
				return c, err
			}
		} else if err != nil {
			return c, err
		}
		previous, err = r.Documents.Load(ctx, previous)
		if err != nil {
			return c, err
		}
		if err := r.Store.SaveReferenceDocument(ctx, t, previous, r.now()); err != nil {
			return c, err
		}
		chunks, err := r.Store.Chunks(ctx, t)
		if err != nil {
			return c, err
		}
		comparison, run, err := r.Analyzer.Compare(ctx, v, current, previous, analysisID, chunks, cp, save, chunkSave)
		if err != nil {
			return c, err
		}
		c.Comparison = &comparison
		c.ReferenceDocuments = []library.Document{previous}
		c.Run = run
	default:
		return c, library.ErrInvalid
	}
	return c, nil
}
func (r *Runner) currentDocument(ctx context.Context, t library.Task, hashes []string) (library.Document, error) {
	// 重试优先读取模型调用前固定的输入；不能跟随新代次文档指针。
	doc, err := r.Store.TaskDocument(ctx, t, t.Identity)
	if err == nil {
		if !documentHashesMatch(doc, hashes) {
			return library.Document{}, library.ErrInvalid
		}
		return r.Documents.Load(ctx, doc)
	}
	if !errors.Is(err, library.ErrNotFound) {
		return library.Document{}, err
	}
	docs, err := r.Store.DocumentsForGeneration(ctx, t.Identity, t.Generation)
	if err != nil {
		return library.Document{}, err
	}
	for _, candidate := range docs {
		if !documentHashesMatch(candidate, hashes) {
			continue
		}
		doc, err = r.Documents.Load(ctx, candidate)
		if err != nil {
			return library.Document{}, err
		}
		if err := r.Store.SaveTaskDocument(ctx, t, doc, r.now()); err != nil {
			return library.Document{}, err
		}
		return doc, nil
	}
	return library.Document{}, library.ErrNotFound
}

func documentHashesMatch(doc library.Document, hashes []string) bool {
	if hashes == nil {
		return true
	}
	// 分析合同固定记录原文及抽取文本 hash，比较输入必须完全对应。
	return len(hashes) == 2 && doc.Source.SHA256 == hashes[0] && doc.Text.SHA256 == hashes[1]
}
