// Package pipeline 编排公告、持久任务和全文 Agent；不参与消息投递。
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/shichao-wang/paper-digest/internal/analysis"
	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/document"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/modelchat"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/internal/state"
)

type Source interface {
	FetchAnnouncements(context.Context, []string) ([]library.CategoryBatch, error)
	FetchVersionMetadata(context.Context, library.Identity) (library.Version, []library.Artifact, error)
}
type Documents interface {
	Ensure(context.Context, library.Identity) (library.Document, error)
	Load(context.Context, any) (library.Document, error)
}
type Analyzer interface {
	Screen(context.Context, library.Version, analysis.Checkpoint, analysis.Save) (library.Relevance, library.Run, error)
	Analyze(context.Context, library.Version, library.Document, []library.Chunk, analysis.Checkpoint, analysis.Save, analysis.ChunkSave) (library.Analysis, library.Run, error)
	Compare(context.Context, library.Version, library.Document, library.Document, int64, []library.Chunk, analysis.Checkpoint, analysis.Save, analysis.ChunkSave) (library.Comparison, library.Run, error)
}
type Runner struct {
	Store          *state.Store
	Source         Source
	Documents      Documents
	Analyzer       Analyzer
	Config         config.Library
	Now            func() time.Time
	Query          library.Query
	IdentityFilter *library.Identity
}

func New(cfg config.Config, store *state.Store) (*Runner, error) {
	if store == nil {
		return nil, library.ErrInvalid
	}
	if err := cfg.ValidateLibrary(); err != nil {
		return nil, err
	}
	model := cfg.Anthropic.Model
	if model == "" {
		model = "deepseek-flash"
	}
	base := cfg.Anthropic.BaseURL
	if base == "" {
		base = "https://api.deepseek.com/v1"
	}
	factory := func(b *modelchat.Budget) (*modelchat.Client, error) {
		return modelchat.NewClient(modelchat.Options{APIKey: cfg.Anthropic.APIKey, BaseURL: base, Model: model, Timeout: 90 * time.Second, Budget: b})
	}
	if cfg.Library.ProcessEnabled {
		if _, err := factory(modelchat.NewBudget(1)); err != nil {
			return nil, err
		}
	}
	return &Runner{Store: store, Source: papers.Source{ArtifactRoot: cfg.Library.DocumentDir}, Documents: &document.Repository{Root: cfg.Library.DocumentDir}, Analyzer: analysis.Engine{NewClient: factory, Model: model, MaxRequests: cfg.Library.MaxRequests, MaxTokens: cfg.Library.MaxTokens}, Config: cfg.Library, Now: time.Now}, nil
}
func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
func (r *Runner) Collect(ctx context.Context) error {
	old, err := r.Store.LibraryStatus(ctx)
	if err != nil {
		return err
	}
	batches, fetchErr := r.Source.FetchAnnouncements(ctx, r.Config.Categories)
	for _, b := range batches {
		if b.Date == "" {
			if err := r.Store.SaveSourceObservation(ctx, b); err != nil {
				return err
			}
			continue
		}
		latest := ""
		for _, p := range old.Batches {
			if p.Category == b.Category && p.Date > latest {
				latest = p.Date
			}
		}
		if latest != "" && b.Date > latest && hasAnnouncementBetween(latest, b.Date) {
			if err := r.Store.SaveGap(ctx, library.Gap{Category: b.Category, After: latest, Before: b.Date, Reason: "服务未捕获中间公告；当前来源不能证明历史修订已经恢复"}); err != nil {
				return err
			}
		}
		if err := r.Store.SaveCategoryBatch(ctx, b); err != nil {
			return err
		}
	}
	return fetchErr
}
func hasAnnouncementBetween(after, before string) bool {
	start, e1 := time.Parse("2006-01-02", after)
	end, e2 := time.Parse("2006-01-02", before)
	if e1 != nil || e2 != nil {
		return false
	}
	for d := start.AddDate(0, 0, 1); d.Before(end); d = d.AddDate(0, 0, 1) {
		if d.Weekday() >= time.Monday && d.Weekday() <= time.Friday {
			return true
		}
	}
	return false
}

var stages = []string{"metadata", "relevance", "document", "analyze", "compare"}

// Process 消费当前可执行任务；退避中的任务留待下次运行，不在任务内长睡眠。
func (r *Runner) Process(ctx context.Context) error {
	count := r.Config.Concurrency
	if count < 1 {
		count = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for n := 0; n < count; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.consume(ctx); err != nil {
				errs <- err
				cancel()
			}
		}()
	}
	wg.Wait()
	close(errs)
	var result error
	for err := range errs {
		result = errors.Join(result, err)
	}
	return result
}
func (r *Runner) consume(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		found := false
		for _, stage := range stages {
			task, err := r.Store.ClaimTaskQuery(ctx, stage, r.now(), 2*time.Minute, r.Query, r.IdentityFilter)
			if errors.Is(err, library.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			found = true
			if err := r.execute(ctx, task); err != nil {
				return err
			}
		}
		if !found {
			return nil
		}
	}
}
func (r *Runner) Serve(ctx context.Context, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var collectors sync.WaitGroup
	if r.Config.CollectEnabled {
		collectors.Add(1)
		go func() {
			defer collectors.Done()
			ticker := time.NewTicker(time.Duration(r.Config.PollSeconds) * time.Second)
			defer ticker.Stop()
			for {
				if err := r.Collect(ctx); err != nil && ctx.Err() == nil {
					log.Warn("公告采集未完整完成", "error", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	defer collectors.Wait()
	defer cancel()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if r.Config.ProcessEnabled {
			if err := r.Process(ctx); err != nil {
				return fmt.Errorf("library worker: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
