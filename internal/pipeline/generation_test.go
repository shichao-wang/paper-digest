package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/analysis"
	"github.com/shichao-wang/paper-digest/internal/library"
)

type generationAnalyzer struct {
	*fakePipelineAnalyzer
	analysisIDs   []int64
	hashes        [][2]string
	beforeCompare func()
	pause         bool
}

func (a *generationAnalyzer) Compare(ctx context.Context, v library.Version, current, previous library.Document, id int64, chunks []library.Chunk, cp analysis.Checkpoint, save analysis.Save, chunkSave analysis.ChunkSave) (library.Comparison, library.Run, error) {
	a.analysisIDs = append(a.analysisIDs, id)
	a.hashes = append(a.hashes, [2]string{current.Text.SHA256, previous.Text.SHA256})
	if a.beforeCompare != nil {
		a.beforeCompare()
		a.beforeCompare = nil
	}
	if a.pause {
		return library.Comparison{}, library.Run{}, library.ErrPaused
	}
	return a.fakePipelineAnalyzer.Compare(ctx, v, current, previous, id, chunks, cp, save, chunkSave)
}
func generationClaim(t *testing.T, r *Runner, stage string) library.Task {
	t.Helper()
	task, err := r.Store.ClaimTask(context.Background(), stage, pipelineTestNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return task
}
func generationDocument(t *testing.T, r *Runner, id library.Identity, g int, hash string) library.Document {
	t.Helper()
	ctx := context.Background()
	if err := r.Store.EnqueueTask(ctx, id, "document", g); err != nil {
		t.Fatal(err)
	}
	task := generationClaim(t, r, "document")
	doc := library.Document{ID: "doc-" + hash, Identity: id, Quality: "ready", Source: library.Artifact{SHA256: "source-" + hash}, Text: library.Artifact{SHA256: hash}}
	if err := r.Store.CompleteTask(ctx, task, library.Completion{Document: &doc}, pipelineTestNow); err != nil {
		t.Fatal(err)
	}
	return doc
}
func generationAnalysis(t *testing.T, r *Runner, id library.Identity, g int, doc library.Document) int64 {
	t.Helper()
	ctx := context.Background()
	if err := r.Store.EnqueueTask(ctx, id, "analyze", g); err != nil {
		t.Fatal(err)
	}
	task := generationClaim(t, r, "analyze")
	a := library.Analysis{PaperVersionID: id.Key(), Model: doc.Text.SHA256, DocumentHashes: []string{doc.Source.SHA256, doc.Text.SHA256}}
	if err := r.Store.CompleteTask(ctx, task, library.Completion{Analysis: &a}, pipelineTestNow); err != nil {
		t.Fatal(err)
	}
	_, aid, err := r.Store.AnalysisForTask(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	return aid
}

func TestCompareBindsExactGenerationBeforeModelAndPreservesVisibleResults(t *testing.T) {
	ctx := context.Background()
	id := pipelineIdentity(1, "v2")
	r, source, docs, base := pipelineTestRunner(t, nil)
	if err := r.Store.UpsertVersion(ctx, pipelineVersion(id)); err != nil {
		t.Fatal(err)
	}
	oldDoc := generationDocument(t, r, id, 0, "old-text")
	oldID := generationAnalysis(t, r, id, 0, oldDoc)
	if err := r.Store.EnqueueTask(ctx, id, "compare", 0); err != nil {
		t.Fatal(err)
	}
	oldTask := generationClaim(t, r, "compare")
	newDoc := generationDocument(t, r, id, 1, "new-text")
	// 新代次尚未成功时展示仍保留旧分析，exact 查询禁止误拿旧分析。
	if err := r.Store.EnqueueTask(ctx, id, "analyze", 1); err != nil {
		t.Fatal(err)
	}
	newTask := generationClaim(t, r, "analyze")
	if _, _, err := r.Store.AnalysisForTask(ctx, newTask); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("unsuccessful generation fallback=%v", err)
	}
	if err := r.Store.FailTask(ctx, newTask, "paused", "budget", time.Time{}, pipelineTestNow); err != nil {
		t.Fatal(err)
	}
	visible, visibleID, err := r.Store.Analysis(ctx, id)
	if err != nil || visibleID != oldID || visible.Model != "old-text" {
		t.Fatalf("old visible result=%+v id=%d err=%v", visible, visibleID, err)
	}
	analyzer := &generationAnalyzer{fakePipelineAnalyzer: base, pause: true}
	r.Analyzer = analyzer
	var newID int64
	analyzer.beforeCompare = func() {
		// 模型回调模拟新代次并发完成：旧任务已经绑定了旧 analysisID 和文档。
		if err := r.Store.RetryTask(ctx, newTask.ID); err != nil {
			t.Fatal(err)
		}
		task := generationClaim(t, r, "analyze")
		a := library.Analysis{PaperVersionID: id.Key(), Model: "new-text", DocumentHashes: []string{newDoc.Source.SHA256, newDoc.Text.SHA256}}
		if err := r.Store.CompleteTask(ctx, task, library.Completion{Analysis: &a}, pipelineTestNow); err != nil {
			t.Fatal(err)
		}
		_, newID, err = r.Store.AnalysisForTask(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.stage(ctx, oldTask); !errors.Is(err, library.ErrPaused) {
		t.Fatalf("pause=%v", err)
	}
	if analyzer.analysisIDs[0] != oldID || analyzer.hashes[0][0] != "old-text" {
		t.Fatalf("old model binding ids=%v hashes=%v", analyzer.analysisIDs, analyzer.hashes)
	}
	if err := r.Store.FailTask(ctx, oldTask, "paused", "budget", time.Time{}, pipelineTestNow); err != nil {
		t.Fatal(err)
	}
	previous, _ := id.Previous()
	ensures := docs.ensures[previous.Key()]
	calls := source.metadataCalls[previous.Key()]
	if err := r.Store.RetryTask(ctx, oldTask.ID); err != nil {
		t.Fatal(err)
	}
	oldTask = generationClaim(t, r, "compare")
	analyzer.pause = false
	c, err := r.stage(ctx, oldTask)
	if err != nil {
		t.Fatal(err)
	}
	if c.Comparison.AnalysisID != oldID || analyzer.hashes[1][0] != "old-text" || docs.ensures[previous.Key()] != ensures || source.metadataCalls[previous.Key()] != calls {
		t.Fatalf("retry rebound inputs comparison=%+v hashes=%v", c.Comparison, analyzer.hashes)
	}
	if err := r.Store.CompleteTask(ctx, oldTask, c, pipelineTestNow); err != nil {
		t.Fatal(err)
	}
	detail := pipelineDetail(t, r, id)
	if detail.AnalysisID != newID || detail.Comparison.AnalysisID != newID || detail.Comparison.Status != "pending" {
		t.Fatalf("old comparison replaced new visible result=%+v", detail)
	}
	if err := r.Store.EnqueueTask(ctx, id, "compare", 1); err != nil {
		t.Fatal(err)
	}
	newCompare := generationClaim(t, r, "compare")
	c, err = r.stage(ctx, newCompare)
	if err != nil {
		t.Fatal(err)
	}
	if c.Comparison.AnalysisID != newID || analyzer.hashes[2][0] != "new-text" {
		t.Fatalf("new model binding=%+v hashes=%v", c.Comparison, analyzer.hashes)
	}
	if err := r.Store.CompleteTask(ctx, newCompare, c, pipelineTestNow); err != nil {
		t.Fatal(err)
	}
	detail = pipelineDetail(t, r, id)
	if detail.Comparison.AnalysisID != newID || detail.Comparison.Status != "ready" {
		t.Fatalf("new comparison=%+v", detail.Comparison)
	}
}

type reanalysisScreen struct {
	*fakePipelineAnalyzer
	level        string
	beforeScreen func()
}

func (a *reanalysisScreen) Screen(ctx context.Context, v library.Version, cp analysis.Checkpoint, save analysis.Save) (library.Relevance, library.Run, error) {
	if a.beforeScreen != nil {
		a.beforeScreen()
		a.beforeScreen = nil
	}
	rel := pipelineRelevance(a.level == "direct")
	rel.Level = a.level
	return rel, library.Run{Model: "reanalysis screening"}, nil
}

func TestReanalysisScreeningClearsNonDirectResultsOnlyAfterSuccess(t *testing.T) {
	for _, level := range []string{"direct", "unrelated", "uncertain"} {
		t.Run(level, func(t *testing.T) {
			ctx := context.Background()
			id := pipelineIdentity(5, "v2")
			r, _, docs, base := pipelineTestRunner(t, []library.CategoryBatch{pipelineBatch(id)})
			if err := r.Collect(ctx); err != nil {
				t.Fatal(err)
			}
			if err := r.Process(ctx); err != nil {
				t.Fatal(err)
			}
			old := pipelineDetail(t, r, id)
			if old.AnalysisID == 0 || old.Comparison.Status != "ready" {
				t.Fatalf("fixture not ready=%+v", old)
			}
			if err := r.Store.EnqueueTask(ctx, id, "relevance", 1); err != nil {
				t.Fatal(err)
			}
			pending := pipelineDetail(t, r, id)
			if pending.AnalysisID != old.AnalysisID || pending.Comparison.Status != "ready" {
				t.Fatalf("queued reanalysis lost results=%+v", pending)
			}
			r.Analyzer = &reanalysisScreen{fakePipelineAnalyzer: base, level: level}
			base.analyzeErrors[id.Key()] = library.ErrPaused
			ensures := docs.ensures[id.Key()]
			if err := r.Process(ctx); err != nil {
				t.Fatal(err)
			}
			detail := pipelineDetail(t, r, id)
			if detail.Relevance.Level != level {
				t.Fatalf("relevance=%+v", detail.Relevance)
			}
			if level == "direct" {
				if detail.AnalysisID != old.AnalysisID || detail.Comparison.Status != "ready" || pipelineTask(t, detail, "analyze").Status != "paused" {
					t.Fatalf("unsuccessful direct generation lost old results=%+v", detail)
				}
			} else {
				if detail.AnalysisID != 0 || detail.Analysis != nil || detail.Comparison.Status == "ready" || docs.ensures[id.Key()] != ensures || base.analyzed[id.Key()] != 1 {
					t.Fatalf("non-direct reanalysis retained output or did downstream work=%+v", detail)
				}
				for _, task := range detail.Tasks {
					if task.Generation == 1 && task.Stage != "relevance" {
						t.Fatalf("non-direct successor=%+v", task)
					}
				}
			}
			history, aid, err := r.Store.AnalysisForTask(ctx, library.Task{Identity: id, Generation: 0})
			if err != nil || history == nil || aid != old.AnalysisID {
				t.Fatalf("old history lost id=%d err=%v", aid, err)
			}
		})
	}
}

func TestLateScreeningCannotRestartSupersededGeneration(t *testing.T) {
	ctx := context.Background()
	id := pipelineIdentity(6, "v2")
	r, _, docs, base := pipelineTestRunner(t, nil)
	if err := r.Store.UpsertVersion(ctx, pipelineVersion(id)); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.EnqueueTask(ctx, id, "relevance", 0); err != nil {
		t.Fatal(err)
	}
	old := generationClaim(t, r, "relevance")
	r.Analyzer = &reanalysisScreen{fakePipelineAnalyzer: base, level: "direct", beforeScreen: func() {
		// 老筛选执行中，新代次完成拒绝结果，然后老筛选才返回 direct。
		if err := r.Store.EnqueueTask(ctx, id, "relevance", 1); err != nil {
			t.Fatal(err)
		}
		newer := generationClaim(t, r, "relevance")
		rel := pipelineRelevance(false)
		if err := r.Store.CompleteTask(ctx, newer, library.Completion{Relevance: &rel}, pipelineTestNow); err != nil {
			t.Fatal(err)
		}
	}}
	if err := r.execute(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := r.Process(ctx); err != nil {
		t.Fatal(err)
	}
	detail := pipelineDetail(t, r, id)
	if detail.Relevance.Level != "unrelated" || detail.AnalysisID != 0 || len(detail.Tasks) != 2 || len(detail.RelevanceHistory) != 2 || len(docs.ensures) != 0 || len(base.analyzed) != 0 {
		t.Fatalf("old screening restarted processing=%+v", detail)
	}
}

func TestAnalyzeReusesEarlierDocumentAndRejectsFutureGeneration(t *testing.T) {
	ctx := context.Background()
	id := pipelineIdentity(2, "v1")
	r, _, _, base := pipelineTestRunner(t, nil)
	if err := r.Store.UpsertVersion(ctx, pipelineVersion(id)); err != nil {
		t.Fatal(err)
	}
	earlier := generationDocument(t, r, id, 0, "earlier")
	generationDocument(t, r, id, 2, "future")
	if err := r.Store.EnqueueTask(ctx, id, "analyze", 1); err != nil {
		t.Fatal(err)
	}
	task := generationClaim(t, r, "analyze")
	c, err := r.stage(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if c.Analysis.DocumentHashes[1] != earlier.Text.SHA256 {
		t.Fatalf("future document leaked=%+v", c.Analysis)
	}
	if err := r.Store.CompleteTask(ctx, task, c, pipelineTestNow); err != nil {
		t.Fatal(err)
	}
	if base.analyzed[id.Key()] != 1 {
		t.Fatal("analysis did not execute")
	}
	other := pipelineIdentity(3, "v1")
	if err := r.Store.UpsertVersion(ctx, pipelineVersion(other)); err != nil {
		t.Fatal(err)
	}
	generationDocument(t, r, other, 2, "only-future")
	if err := r.Store.EnqueueTask(ctx, other, "analyze", 1); err != nil {
		t.Fatal(err)
	}
	task = generationClaim(t, r, "analyze")
	if _, err := r.stage(ctx, task); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("future-only document accepted=%v", err)
	}
	unbound := pipelineIdentity(4, "v2")
	if err := r.Store.UpsertVersion(ctx, pipelineVersion(unbound)); err != nil {
		t.Fatal(err)
	}
	generationDocument(t, r, unbound, 0, "unbound-text")
	if err := r.Store.EnqueueTask(ctx, unbound, "analyze", 0); err != nil {
		t.Fatal(err)
	}
	analysisTask := generationClaim(t, r, "analyze")
	badAnalysis := library.Analysis{PaperVersionID: unbound.Key()}
	if err := r.Store.CompleteTask(ctx, analysisTask, library.Completion{Analysis: &badAnalysis, Next: []string{"compare"}}, pipelineTestNow); err != nil {
		t.Fatal(err)
	}
	task = generationClaim(t, r, "compare")
	if _, err := r.stage(ctx, task); !errors.Is(err, library.ErrInvalid) {
		t.Fatalf("unbound comparison hashes accepted=%v", err)
	}
}
