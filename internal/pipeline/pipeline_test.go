package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/analysis"
	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/state"
)

var pipelineTestNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func pipelineIdentity(n int, version string) library.Identity {
	return library.Identity{Source: "arxiv", PaperID: fmt.Sprintf("2610.%05d", n), Version: version}
}
func pipelineVersion(id library.Identity) library.Version {
	return library.Version{Identity: id, Title: "Synthetic integration paper " + id.PaperID, Authors: []string{"Fixture author"}, Abstract: "Fixture abstract", Categories: []string{"cs.IR"}, PrimaryCategory: "cs.IR", PublishedAt: "2026-09-30T12:00:00Z", UpdatedAt: "2026-10-01T12:00:00Z", Origin: "announcement", AnnouncementDate: "2026-10-02", CapturedAt: library.Timestamp(pipelineTestNow), MetadataVerified: true}
}
func pipelineBatch(ids ...library.Identity) library.CategoryBatch {
	b := library.CategoryBatch{Category: "cs.IR", Date: "2026-10-02", CapturedAt: library.Timestamp(pipelineTestNow), Completeness: "complete", Counts: map[string]int{"new": len(ids)}, Artifacts: []library.Artifact{{Path: "synthetic/feed.xml", SHA256: "synthetic-feed", URL: "https://rss.arxiv.org/atom/cs.IR", Kind: "synthetic"}}}
	for _, id := range ids {
		b.Versions = append(b.Versions, pipelineVersion(id))
		kind := "new"
		if id.Number() > 1 {
			kind = "replace"
		}
		b.Events = append(b.Events, library.Announcement{Identity: id, Category: b.Category, Date: b.Date, Type: kind})
	}
	return b
}

type fakePipelineSource struct {
	mu             sync.Mutex
	batches        []library.CategoryBatch
	collectErr     error
	metadataErrors map[string]error
	metadataCalls  map[string]int
	categories     []string
}

func (f *fakePipelineSource) FetchAnnouncements(_ context.Context, categories []string) ([]library.CategoryBatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.categories = append([]string{}, categories...)
	return f.batches, f.collectErr
}
func (f *fakePipelineSource) FetchVersionMetadata(_ context.Context, id library.Identity) (library.Version, []library.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.metadataCalls == nil {
		f.metadataCalls = map[string]int{}
	}
	f.metadataCalls[id.Key()]++
	if err := f.metadataErrors[id.Key()]; err != nil {
		return library.Version{}, nil, err
	}
	v := pipelineVersion(id)
	artifact := library.Artifact{Path: "synthetic/api-" + strings.ReplaceAll(id.Key(), "/", "-") + ".xml", SHA256: "synthetic-api", Kind: "synthetic", URL: id.AbsURL()}
	return v, []library.Artifact{artifact}, nil
}

type fakePipelineDocuments struct {
	mu        sync.Mutex
	ensures   map[string]int
	errors    map[string]error
	documents map[string]library.Document
}

func (f *fakePipelineDocuments) Ensure(_ context.Context, id library.Identity) (library.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ensures == nil {
		f.ensures = map[string]int{}
	}
	f.ensures[id.Key()]++
	if err := f.errors[id.Key()]; err != nil {
		return library.Document{}, err
	}
	if f.documents == nil {
		f.documents = map[string]library.Document{}
	}
	doc := library.Document{ID: "synthetic:" + id.Key(), Identity: id, Source: library.Artifact{Path: "synthetic/document.pdf", SHA256: "pdf-" + id.Key(), Kind: "synthetic"}, Text: library.Artifact{Path: "synthetic/document.json", SHA256: "text-" + id.Key(), Kind: "synthetic"}, Quality: "ready", Extractor: "synthetic-test", Pages: []library.Page{{Number: 1, Text: "Fixture full-text body", SHA256: "pagehash"}}, Blocks: []library.Block{{ID: "p1-b1", Page: 1, Text: "Fixture full-text body", SHA256: "blockhash"}}}
	f.documents[id.Key()] = doc
	return doc, nil
}
func (f *fakePipelineDocuments) Load(_ context.Context, ref any) (library.Document, error) {
	switch v := ref.(type) {
	case library.Document:
		return v, nil
	case *library.Document:
		return *v, nil
	case library.Identity:
		f.mu.Lock()
		defer f.mu.Unlock()
		doc, ok := f.documents[v.Key()]
		if !ok {
			return library.Document{}, library.ErrNotFound
		}
		return doc, nil
	}
	return library.Document{}, library.ErrInvalid
}

type fakePipelineAnalyzer struct {
	mu                           sync.Mutex
	screened, analyzed, compared map[string]int
	unrelated                    map[string]bool
	analyzeErrors                map[string]error
	pauseIdentity                string
	resumeCheckpoints            []analysis.Checkpoint
	comparisons                  [][2]library.Identity
}

func pipelineRelevance(direct bool) library.Relevance {
	level := "direct"
	topics := []string{"search"}
	if !direct {
		level = "unrelated"
		topics = []string{}
	}
	return library.Relevance{Topics: topics, DirectlyRelated: direct, Level: level, Rationale: "Synthetic integration fixture", ExtractedKeywords: []string{}, EvidenceIDs: []string{}}
}
func (f *fakePipelineAnalyzer) Screen(_ context.Context, v library.Version, _ analysis.Checkpoint, _ analysis.Save) (library.Relevance, library.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.screened == nil {
		f.screened = map[string]int{}
	}
	f.screened[v.Key()]++
	return pipelineRelevance(!f.unrelated[v.Key()]), library.Run{Requests: 1, Model: "synthetic"}, nil
}
func (f *fakePipelineAnalyzer) Analyze(_ context.Context, v library.Version, doc library.Document, chunks []library.Chunk, cp analysis.Checkpoint, save analysis.Save, chunkSave analysis.ChunkSave) (library.Analysis, library.Run, error) {
	f.mu.Lock()
	if f.analyzed == nil {
		f.analyzed = map[string]int{}
	}
	f.analyzed[v.Key()]++
	pause := v.Key() == f.pauseIdentity
	failure := f.analyzeErrors[v.Key()]
	if pause {
		f.resumeCheckpoints = append(f.resumeCheckpoints, cp)
	}
	f.mu.Unlock()
	if pause {
		if cp.MaxRequests == 0 {
			cp = analysis.Checkpoint{Run: library.Run{Requests: 7, PromptTokens: 123, ReservedTokens: 99}, MaxRequests: 7, MaxTokens: 1000, Scope: v.Key(), StartedAt: library.Timestamp(pipelineTestNow), Sessions: map[string]analysis.SessionCheckpoint{"chunk": {Phase: "ready", Attempts: 2, ReadBlocks: []string{"p1-b1"}}}}
			if err := save(cp); err != nil {
				return library.Analysis{}, library.Run{}, err
			}
			if err := chunkSave(library.Chunk{DocumentID: doc.ID, DocumentHash: doc.Text.SHA256, BlockID: "p1-b1", Read: true, Notes: []library.Claim{}, Evidence: []library.Evidence{}, MissingFields: []string{}}); err != nil {
				return library.Analysis{}, library.Run{}, err
			}
		}
		return library.Analysis{}, cp.Run, library.ErrPaused
	}
	if failure != nil {
		return library.Analysis{}, library.Run{}, failure
	}
	return library.Analysis{SchemaVersion: library.SchemaVersion, PaperVersionID: v.Key(), Model: "synthetic", PromptVersion: library.PromptVersion, DocumentHashes: []string{doc.Source.SHA256, doc.Text.SHA256}, Content: library.AnalysisContent{TitleZH: "集成测试论文", Relevance: pipelineRelevance(true), SummaryZH: library.Claim{Text: "Synthetic analysis", EvidenceIDs: []string{}}}}, library.Run{Requests: 2, Model: "synthetic"}, nil
}
func (f *fakePipelineAnalyzer) Compare(_ context.Context, v library.Version, current, previous library.Document, analysisID int64, _ []library.Chunk, _ analysis.Checkpoint, _ analysis.Save, _ analysis.ChunkSave) (library.Comparison, library.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.compared == nil {
		f.compared = map[string]int{}
	}
	f.compared[v.Key()]++
	f.comparisons = append(f.comparisons, [2]library.Identity{current.Identity, previous.Identity})
	return library.Comparison{AnalysisID: analysisID, PreviousVersion: previous.Version, Status: "ready", DocumentHashes: []string{current.Text.SHA256, previous.Text.SHA256}, Content: library.ComparisonContent{Changes: []library.Change{}, Evidence: []library.Evidence{}}}, library.Run{Requests: 1, Model: "synthetic"}, nil
}

func pipelineTestRunner(t *testing.T, batches []library.CategoryBatch) (*Runner, *fakePipelineSource, *fakePipelineDocuments, *fakePipelineAnalyzer) {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	source := &fakePipelineSource{batches: batches, metadataErrors: map[string]error{}}
	docs := &fakePipelineDocuments{errors: map[string]error{}}
	analyzer := &fakePipelineAnalyzer{unrelated: map[string]bool{}, analyzeErrors: map[string]error{}}
	return &Runner{Store: store, Source: source, Documents: docs, Analyzer: analyzer, Config: config.Library{Categories: []string{"cs.IR", "cs.LG", "cs.AI", "cs.CL", "stat.ML"}, Concurrency: 4, TaskTimeoutSeconds: 30}, Now: func() time.Time { return pipelineTestNow }}, source, docs, analyzer
}
func pipelineDetail(t *testing.T, r *Runner, id library.Identity) library.Detail {
	t.Helper()
	detail, err := r.Store.LibraryDetail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return detail
}
func pipelineTask(t *testing.T, detail library.Detail, stage string) library.Task {
	t.Helper()
	for _, task := range detail.Tasks {
		if task.Stage == stage {
			return task
		}
	}
	t.Fatalf("missing %s task in %#v", stage, detail.Tasks)
	return library.Task{}
}

func TestPipelineProcessesEveryCandidateAndVersionWithoutSending(t *testing.T) {
	ids := []library.Identity{}
	for n := 1; n <= 111; n++ {
		ids = append(ids, pipelineIdentity(n, "v1"))
	}
	ids = append(ids, pipelineIdentity(1, "v2"))
	runner, source, docs, analyzer := pipelineTestRunner(t, []library.CategoryBatch{pipelineBatch(ids...)})
	if err := runner.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runner.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(source.categories) != 5 {
		t.Fatal("collection omitted requested categories")
	}
	if err := runner.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	page, err := runner.Store.BrowseLibrary(context.Background(), library.Query{Relevance: "all", PageSize: 100})
	if err != nil || page.Total != 112 {
		t.Fatalf("total=%d err=%v", page.Total, err)
	}
	for _, id := range ids {
		detail := pipelineDetail(t, runner, id)
		if detail.Analysis == nil || !detail.Version.MetadataVerified || len(detail.Version.MetadataArtifacts) != 1 {
			t.Fatalf("missing persisted output for %s: %#v", id.Key(), detail)
		}
		for _, stage := range []string{"metadata", "relevance", "document", "analyze"} {
			if task := pipelineTask(t, detail, stage); task.Status != "succeeded" || task.Attempt != 1 {
				t.Fatalf("task = %#v", task)
			}
		}
		if id.Number() == 1 {
			if detail.Comparison == nil || detail.Comparison.Status != "not_applicable" {
				t.Fatalf("v1 comparison=%#v", detail.Comparison)
			}
		} else {
			if task := pipelineTask(t, detail, "compare"); task.Status != "succeeded" {
				t.Fatalf("compare=%#v", task)
			}
			if detail.Comparison.PreviousVersion != "v1" {
				t.Fatal("comparison did not bind exact previous version")
			}
		}
	}
	if len(analyzer.screened) != 112 || len(analyzer.analyzed) != 112 || len(analyzer.compared) != 1 || len(docs.ensures) != 112 {
		t.Fatalf("stage counts: screened=%d analyzed=%d compared=%d docs=%d", len(analyzer.screened), len(analyzer.analyzed), len(analyzer.compared), len(docs.ensures))
	}
	legacy, err := runner.Store.GetJob(context.Background(), "ras", "2026-10-02")
	if err == nil && legacy.Status != "" {
		t.Fatalf("pipeline created delivery job %#v", legacy)
	}
}

func TestPipelineSingleErrorContinuesAndUnrelatedNeverDownloads(t *testing.T) {
	failed, unrelated, healthy := pipelineIdentity(1, "v1"), pipelineIdentity(2, "v1"), pipelineIdentity(3, "v1")
	runner, source, docs, analyzer := pipelineTestRunner(t, []library.CategoryBatch{pipelineBatch(failed, unrelated, healthy)})
	source.metadataErrors[failed.Key()] = errors.New("synthetic source unavailable")
	analyzer.unrelated[unrelated.Key()] = true
	if err := runner.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runner.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if task := pipelineTask(t, pipelineDetail(t, runner, failed), "metadata"); task.Status != "retry_wait" {
		t.Fatalf("failure task=%#v", task)
	}
	detail := pipelineDetail(t, runner, unrelated)
	if detail.Relevance == nil || detail.Relevance.Level != "unrelated" || len(detail.Documents) != 0 || detail.Analysis != nil || docs.ensures[unrelated.Key()] != 0 || analyzer.analyzed[unrelated.Key()] != 0 {
		t.Fatalf("unrelated was downloaded/analyzed: %#v", detail)
	}
	for _, task := range detail.Tasks {
		if task.Stage == "document" || task.Stage == "analyze" || task.Stage == "compare" {
			t.Fatalf("unexpected unrelated task=%#v", task)
		}
	}
	if detail := pipelineDetail(t, runner, healthy); detail.Analysis == nil {
		t.Fatalf("healthy candidate blocked by peer failure %#v", detail)
	}
}

func TestPipelineVersionTwoRemainsVisibleWithoutExactPrevious(t *testing.T) {
	for _, stage := range []string{"metadata", "document"} {
		t.Run(stage, func(t *testing.T) {
			current := pipelineIdentity(7, "v2")
			previous, _ := current.Previous()
			runner, source, docs, analyzer := pipelineTestRunner(t, []library.CategoryBatch{pipelineBatch(current)})
			if stage == "metadata" {
				source.metadataErrors[previous.Key()] = errors.New("exact previous metadata unavailable")
			} else {
				docs.errors[previous.Key()] = errors.New("exact previous PDF unavailable")
			}
			if err := runner.Collect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := runner.Process(context.Background()); err != nil {
				t.Fatal(err)
			}
			detail := pipelineDetail(t, runner, current)
			if detail.Analysis == nil || detail.Analysis.PaperVersionID != current.Key() || detail.Comparison.Status != "pending" || pipelineTask(t, detail, "compare").Status != "retry_wait" || analyzer.compared[current.Key()] != 0 {
				t.Fatalf("current output lost or comparison fabricated: %#v", detail)
			}
			if source.metadataCalls[previous.Key()] != 1 {
				t.Fatalf("did not request exact previous %s", previous.Key())
			}
			if stage == "metadata" && docs.ensures[previous.Key()] != 0 {
				t.Fatal("downloaded previous document without exact metadata")
			}
			for key := range docs.ensures {
				if key != current.Key() && key != previous.Key() {
					t.Fatalf("downloaded wrong version %s", key)
				}
			}
		})
	}
}

func TestPipelinePausedRetryKeepsCheckpointBudgetAndChunks(t *testing.T) {
	id := pipelineIdentity(1, "v1")
	runner, _, _, analyzer := pipelineTestRunner(t, []library.CategoryBatch{pipelineBatch(id)})
	analyzer.pauseIdentity = id.Key()
	if err := runner.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runner.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := pipelineTask(t, pipelineDetail(t, runner, id), "analyze")
	if first.Status != "paused" || len(first.Checkpoint) == 0 {
		t.Fatalf("pause not persisted %#v", first)
	}
	if err := runner.Store.RetryTask(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	if err := runner.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := pipelineTask(t, pipelineDetail(t, runner, id), "analyze")
	if second.Status != "paused" || second.Generation != first.Generation || second.Attempt != 2 || string(second.Checkpoint) != string(first.Checkpoint) {
		t.Fatalf("retry refreshed checkpoint %#v / %#v", first, second)
	}
	var cp analysis.Checkpoint
	if err := json.Unmarshal(second.Checkpoint, &cp); err != nil {
		t.Fatal(err)
	}
	if cp.Run.Requests != 7 || cp.MaxRequests != 7 || cp.Run.PromptTokens != 123 || cp.Run.ReservedTokens != 99 || cp.MaxTokens != 1000 || cp.Sessions["chunk"].Attempts != 2 {
		t.Fatalf("checkpoint changed %#v", cp)
	}
	chunks, err := runner.Store.Chunks(context.Background(), second)
	if err != nil || len(chunks) != 1 || !chunks[0].Read {
		t.Fatalf("chunks=%#v err=%v", chunks, err)
	}
	if len(analyzer.resumeCheckpoints) != 2 || analyzer.resumeCheckpoints[1].MaxRequests != 7 || analyzer.resumeCheckpoints[1].Run.Requests != 7 {
		t.Fatalf("analyzer did not resume saved checkpoint %#v", analyzer.resumeCheckpoints)
	}
	if pipelineDetail(t, runner, id).Analysis != nil {
		t.Fatal("paused analysis published output")
	}
}

func TestPipelineCollectPersistsPartialAndGap(t *testing.T) {
	id := pipelineIdentity(1, "v1")
	batch := pipelineBatch(id)
	batch.Completeness = "incomplete"
	batch.Reason = "synthetic truncation"
	runner, source, _, _ := pipelineTestRunner(t, []library.CategoryBatch{batch})
	source.collectErr = errors.New("synthetic source partial error")
	old := pipelineBatch()
	old.Date = "2026-09-29"
	if err := runner.Store.SaveCategoryBatch(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if err := runner.Collect(context.Background()); !errors.Is(err, source.collectErr) {
		t.Fatalf("collection error=%v", err)
	}
	status, err := runner.Store.LibraryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Batches) != 2 || len(status.Gaps) != 1 || status.Gaps[0].After != "2026-09-29" || status.Gaps[0].Before != "2026-10-02" {
		t.Fatalf("partial/gap status=%#v", status)
	}
	found := false
	for _, b := range status.Batches {
		if b.Date == batch.Date {
			found = true
			if b.Completeness != "incomplete" || b.Reason != batch.Reason || len(b.Artifacts) != 1 {
				t.Fatalf("partial evidence lost %#v", b)
			}
		}
	}
	if !found {
		t.Fatal("partial batch not persisted")
	}
	if task := pipelineTask(t, pipelineDetail(t, runner, id), "metadata"); task.Status != "queued" {
		t.Fatalf("partial candidate lost task %#v", task)
	}
}

func TestPipelineCollectUndatedObservationPreservesCandidates(t *testing.T) {
	id := pipelineIdentity(9, "v2")
	batch := pipelineBatch(id)
	batch.Date = ""
	batch.Completeness = "incomplete"
	batch.Reason = "synthetic source with no verified batch date"
	batch.Versions[0].AnnouncementDate = ""
	batch.Events[0].Date = ""
	runner, _, _, _ := pipelineTestRunner(t, []library.CategoryBatch{batch})
	if err := runner.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	detail := pipelineDetail(t, runner, id)
	if detail.Version.AnnouncementDate != "" || detail.Version.Origin != "announcement_unverified" || pipelineTask(t, detail, "metadata").Status != "queued" {
		t.Fatalf("undated candidate was fabricated or discarded: %#v", detail)
	}
	status, err := runner.Store.LibraryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Batches) != 1 || status.Batches[0].Date != "" || status.Batches[0].Reason != batch.Reason || len(status.Batches[0].Artifacts) != 1 || len(status.Gaps) != 0 {
		t.Fatalf("undated observation was lost or became dated batch: %#v", status)
	}
}

func TestPipelineProcessingFiltersRemainBoundedToRequestedWork(t *testing.T) {
	for _, mode := range []string{"identity", "query"} {
		t.Run(mode, func(t *testing.T) {
			selected, other := pipelineIdentity(1, "v1"), pipelineIdentity(2, "v1")
			runner, source, _, analyzer := pipelineTestRunner(t, []library.CategoryBatch{pipelineBatch(selected, other)})
			if err := runner.Collect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if mode == "identity" {
				runner.IdentityFilter = &selected
			} else {
				runner.Query = library.Query{Q: selected.PaperID, Relevance: "all"}
			}
			if err := runner.Process(context.Background()); err != nil {
				t.Fatal(err)
			}
			if pipelineDetail(t, runner, selected).Analysis == nil || pipelineDetail(t, runner, other).Analysis != nil || source.metadataCalls[other.Key()] != 0 || analyzer.screened[other.Key()] != 0 {
				t.Fatal("processing filter leaked to unselected work")
			}
			if pipelineTask(t, pipelineDetail(t, runner, other), "metadata").Status != "queued" {
				t.Fatal("unselected task changed")
			}
		})
	}
}

func TestHasAnnouncementBetween(t *testing.T) {
	for _, tt := range []struct {
		after, before string
		want          bool
	}{{"2026-10-02", "2026-10-05", false}, {"2026-10-01", "2026-10-05", true}, {"2026-10-01", "2026-10-02", false}, {"invalid", "2026-10-02", false}} {
		if got := hasAnnouncementBetween(tt.after, tt.before); got != tt.want {
			t.Errorf("between %s %s=%v want %v", tt.after, tt.before, got, tt.want)
		}
	}
}
