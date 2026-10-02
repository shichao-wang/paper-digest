package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/digest"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/papers"
)

var libraryNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func versionFixture(n int, version string) library.Version {
	return library.Version{Identity: library.Identity{Source: "arxiv", PaperID: fmt.Sprintf("2610.%05d", n), Version: version}, Title: fmt.Sprintf("Paper %d", n), Authors: []string{"Author"}, Abstract: "abstract", MetadataVerified: true}
}
func batchFixture(count int) library.CategoryBatch {
	b := library.CategoryBatch{Category: "cs.IR", Date: "2026-10-02", CapturedAt: library.Timestamp(libraryNow), Completeness: "complete", Counts: map[string]int{"new": count}, Artifacts: []library.Artifact{{Path: "snapshots/feed.html", SHA256: "feedhash", Kind: "html"}}}
	for i := 1; i <= count; i++ {
		v := versionFixture(i, "v1")
		b.Versions = append(b.Versions, v)
		b.Events = append(b.Events, library.Announcement{Identity: v.Identity, Type: "new", Category: b.Category, Date: b.Date})
	}
	return b
}
func claimFixture(t *testing.T, s *Store, stage string, now time.Time) library.Task {
	t.Helper()
	task, err := s.ClaimTask(context.Background(), stage, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return task
}
func mustLibraryCount(t *testing.T, s *Store, table string, want int) {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("%s count=%d want %d", table, n, want)
	}
}

func TestLibraryMigrationBackupPreservesLegacy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE jobs(topic TEXT NOT NULL,date TEXT NOT NULL,status TEXT NOT NULL,message TEXT NOT NULL,PRIMARY KEY(topic,date));
 CREATE TABLE papers(topic TEXT NOT NULL,paper_id TEXT NOT NULL,version TEXT NOT NULL,data BLOB NOT NULL,PRIMARY KEY(topic,paper_id,version));
 CREATE TABLE job_papers(topic TEXT NOT NULL,date TEXT NOT NULL,paper_id TEXT NOT NULL,version TEXT NOT NULL,position INTEGER NOT NULL,summary BLOB,PRIMARY KEY(topic,date,paper_id));
 CREATE TABLE recommendations(topic TEXT NOT NULL,paper_id TEXT NOT NULL,date TEXT NOT NULL,version TEXT NOT NULL,PRIMARY KEY(topic,paper_id));`)
	if err != nil {
		t.Fatal(err)
	}
	p := papers.Paper{ID: "arxiv:2610.00001", Version: "2", Title: "Historical paper"}
	raw, _ := json.Marshal(p)
	summary, _ := json.Marshal(digest.Summary{Text: "historical summary", Model: "old-model"})
	for i, status := range []string{"sent", "unknown"} {
		date := fmt.Sprintf("2026-09-%02d", 28+i)
		if _, err := db.Exec(`INSERT INTO jobs VALUES(?,?,?,?)`, "legacy", date, status, "history message"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO job_papers VALUES(?,?,?,?,?,?)`, "legacy", date, p.ID, "hash-not-real", 0, summary); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO papers VALUES(?,?,?,?)`, "legacy", p.ID, "hash-not-real", raw); err != nil {
		t.Fatal(err)
	}
	p.ID = "2610.00002"
	p.Version = ""
	raw, _ = json.Marshal(p)
	if _, err := db.Exec(`INSERT INTO papers VALUES(?,?,?,?)`, "legacy", p.ID, "abcdhash", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO recommendations VALUES(?,?,?,?)`, "legacy", "arxiv:2610.00001", "2026-09-28", "hash-not-real"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s := openTestStore(t, path)
	matches, err := filepath.Glob(path + ".pre-library-*.db")
	if err != nil || len(matches) != 1 {
		t.Fatalf("backups=%v err=%v", matches, err)
	}
	backup, err := sql.Open("sqlite", matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var migrated int
	if err := backup.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='schema_migrations'`).Scan(&migrated); err != nil {
		t.Fatal(err)
	}
	if migrated != 0 {
		t.Fatal("backup created after new migration table")
	}
	backup.Close()
	for i, status := range []string{"sent", "unknown"} {
		date := fmt.Sprintf("2026-09-%02d", 28+i)
		j, err := s.GetJob(ctx, "legacy", date)
		if err != nil || j.Status != status || j.Message != "history message" {
			t.Fatalf("job %+v err=%v", j, err)
		}
		completed, err := s.Completed(ctx, "legacy", date)
		if err != nil || len(completed) != 1 || completed[0].Summary.Text != "historical summary" {
			t.Fatalf("legacy summary %+v err=%v", completed, err)
		}
	}
	seen, err := s.Seen(ctx, "legacy", "arxiv:2610.00001")
	if err != nil || !seen {
		t.Fatal("recommendation lost")
	}
	v, err := s.GetVersion(ctx, versionFixture(1, "v2").Identity)
	if err != nil || v.Title != "Historical paper" || v.Origin != "legacy" || v.AnnouncementDate != "" {
		t.Fatalf("imported %+v err=%v", v, err)
	}
	mustLibraryCount(t, s, "library_versions", 1)
	mustLibraryCount(t, s, "library_tasks", 0)
	mustLibraryCount(t, s, "library_events", 0)
	mustLibraryCount(t, s, "schema_migrations", 2)
	s.Close()
	s = openTestStore(t, path)
	s.Close()
	matches, _ = filepath.Glob(path + ".pre-library-*.db")
	if len(matches) != 1 {
		t.Fatalf("reopen duplicated backup %v", matches)
	}
	fresh := filepath.Join(dir, "empty.db")
	s = openTestStore(t, fresh)
	s.Close()
	matches, _ = filepath.Glob(fresh + ".pre-library-*.db")
	if len(matches) != 0 {
		t.Fatalf("empty database backed up: %v", matches)
	}
	s = openTestStore(t, ":memory:")
	s.Close()
}

func TestLibraryBatchStoresAllCandidatesAcrossCategoriesAndVersions(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, ":memory:")
	defer s.Close()
	b := batchFixture(155)
	if err := s.SaveCategoryBatch(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCategoryBatch(ctx, b); err != nil {
		t.Fatal(err)
	}
	b.Category = "cs.LG"
	for i := range b.Events {
		b.Events[i].Category = b.Category
	}
	if err := s.SaveCategoryBatch(ctx, b); err != nil {
		t.Fatal(err)
	}
	mustLibraryCount(t, s, "library_versions", 155)
	mustLibraryCount(t, s, "library_tasks", 155)
	mustLibraryCount(t, s, "library_events", 310)
	mustLibraryCount(t, s, "library_batch_versions", 310)
	b = batchFixture(1)
	b.Versions[0].Version = "v2"
	b.Events[0].Version = "v2"
	b.Events[0].Type = "replace"
	if err := s.SaveCategoryBatch(ctx, b); err != nil {
		t.Fatal(err)
	}
	mustLibraryCount(t, s, "library_versions", 156)
	mustLibraryCount(t, s, "library_tasks", 156)
	page, err := s.BrowseLibrary(ctx, library.Query{Relevance: "all", PageSize: 100})
	if err != nil || page.Total != 156 || len(page.Items) != 100 {
		t.Fatalf("page %+v err=%v", page, err)
	}
	page, err = s.BrowseLibrary(ctx, library.Query{Relevance: "all", PageSize: 100, Page: 2})
	if err != nil || page.Total != 156 || len(page.Items) != 56 {
		t.Fatalf("second page %+v err=%v", page, err)
	}
	for _, item := range page.Items {
		if item.Tasks == nil {
			t.Fatal("tasks should encode as array")
		}
	}
	page, err = s.BrowseLibrary(ctx, library.Query{Relevance: "all", Batch: "cs.LG/2026-10-02", PageSize: 100})
	if err != nil || page.Total != 155 {
		t.Fatalf("batch total=%d err=%v", page.Total, err)
	}
	status, err := s.LibraryStatus(ctx)
	if err != nil || status.Versions != 156 || len(status.Batches) != 2 || status.Batches[0].Versions != nil || status.Batches[0].Events != nil {
		t.Fatalf("status %+v err=%v", status, err)
	}
	if _, err := s.ClaimTask(ctx, "analyze", libraryNow, time.Minute); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("unexpected task claim: %v", err)
	}
}

func TestLibraryConcurrentClaimsAndDelayedFailureDoNotBlock(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, ":memory:")
	defer s.Close()
	if err := s.SaveCategoryBatch(ctx, batchFixture(12)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claimed := make(chan library.Task, 12)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, err := s.ClaimTask(ctx, "metadata", libraryNow, time.Minute)
			if err != nil {
				errs <- err
				return
			}
			claimed <- task
		}()
	}
	wg.Wait()
	close(claimed)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	var first library.Task
	for task := range claimed {
		if ids[task.ID] {
			t.Fatalf("duplicate concurrent claim %d", task.ID)
		}
		ids[task.ID] = true
		first = task
	}
	if len(ids) != 12 {
		t.Fatalf("claims=%d", len(ids))
	}
	if err := s.FailTask(ctx, first, "retry_wait", "temporary failure", libraryNow.Add(time.Hour), libraryNow); err != nil {
		t.Fatal(err)
	}
	v := versionFixture(13, "v1")
	if err := s.UpsertVersion(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTask(ctx, v.Identity, "metadata", 0); err != nil {
		t.Fatal(err)
	}
	next := claimFixture(t, s, "metadata", libraryNow)
	if next.Identity != v.Identity {
		t.Fatalf("delayed failure blocked new candidate %+v", next)
	}
	if _, err := s.ClaimTask(ctx, "metadata", libraryNow, time.Minute); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("premature delayed retry=%v", err)
	}
}

func TestLibraryLeaseRejectsExpiredAndSupersededWrites(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, ":memory:")
	defer s.Close()
	if err := s.SaveCategoryBatch(ctx, batchFixture(1)); err != nil {
		t.Fatal(err)
	}
	old := claimFixture(t, s, "metadata", libraryNow)
	expired := libraryNow.Add(time.Minute)
	staleWrites := []func(library.Task, time.Time) error{
		func(t library.Task, now time.Time) error { return s.RenewLease(ctx, t, now, time.Minute) },
		func(t library.Task, now time.Time) error {
			return s.SaveCheckpoint(ctx, t, json.RawMessage(`{"budget":10}`), now)
		},
		func(t library.Task, now time.Time) error {
			return s.SaveChunk(ctx, t, library.Chunk{DocumentID: "d", DocumentHash: "h", BlockID: "b"}, now)
		},
		func(t library.Task, now time.Time) error {
			return s.CompleteTask(ctx, t, library.Completion{Next: []string{"relevance"}}, now)
		},
		func(t library.Task, now time.Time) error { return s.FailTask(ctx, t, "retry_wait", "stale", now, now) },
	}
	for _, write := range staleWrites {
		if err := write(old, expired); !errors.Is(err, library.ErrLease) {
			t.Fatalf("expired write err=%v", err)
		}
	}
	current := claimFixture(t, s, "metadata", expired)
	if current.LeaseToken == old.LeaseToken || current.Attempt != 2 {
		t.Fatalf("reclaim %+v", current)
	}
	for _, write := range staleWrites {
		if err := write(old, expired.Add(time.Second)); !errors.Is(err, library.ErrLease) {
			t.Fatalf("superseded write err=%v", err)
		}
	}
	if err := s.RenewLease(ctx, current, expired.Add(30*time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckpoint(ctx, current, json.RawMessage(`{"live":true}`), expired.Add(70*time.Second)); err != nil {
		t.Fatal(err)
	}
	mustLibraryCount(t, s, "library_outputs", 0)
	mustLibraryCount(t, s, "library_runs", 0)
}

func TestLibraryRetryRecoversCheckpointChunksAndAtomicCompletion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "resume.db")
	s := openTestStore(t, path)
	if err := s.SaveCategoryBatch(ctx, batchFixture(1)); err != nil {
		t.Fatal(err)
	}
	task := claimFixture(t, s, "metadata", libraryNow)
	cp := json.RawMessage(`{"requests":7,"reserved_tokens":800,"messages":["delivered receipt"]}`)
	chunk := library.Chunk{DocumentID: "document", DocumentHash: "hash", BlockID: "b1", Read: true}
	if err := s.SaveCheckpoint(ctx, task, cp, libraryNow); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChunk(ctx, task, chunk, libraryNow); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChunk(ctx, task, chunk, libraryNow); err != nil {
		t.Fatal(err)
	}
	changed := chunk
	changed.Read = false
	if err := s.SaveChunk(ctx, task, changed, libraryNow); !errors.Is(err, library.ErrInvalid) {
		t.Fatalf("chunk overwrite=%v", err)
	}
	if err := s.FailTask(ctx, task, "paused", "budget exhausted", time.Time{}, libraryNow); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openTestStore(t, path)
	defer s.Close()
	if err := s.RetryTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	task = claimFixture(t, s, "metadata", libraryNow.Add(time.Second))
	if string(task.Checkpoint) != string(cp) || task.Generation != 0 || task.Attempt != 2 {
		t.Fatalf("resume %+v", task)
	}
	chunks, err := s.Chunks(ctx, task)
	if err != nil || len(chunks) != 1 || !reflect.DeepEqual(chunks[0], chunk) {
		t.Fatalf("chunks=%+v err=%v", chunks, err)
	}
	v := versionFixture(1, "v1")
	v.Title = "transactional update"
	c := library.Completion{Version: &v, Next: []string{"relevance", "invalid-stage"}, Run: library.Run{Requests: 9}}
	if err := s.CompleteTask(ctx, task, c, libraryNow.Add(time.Second)); !errors.Is(err, library.ErrInvalid) {
		t.Fatalf("bad completion=%v", err)
	}
	mustLibraryCount(t, s, "library_outputs", 0)
	mustLibraryCount(t, s, "library_runs", 0)
	mustLibraryCount(t, s, "library_tasks", 1)
	got, err := s.GetVersion(ctx, task.Identity)
	if err != nil || got.Title == v.Title {
		t.Fatalf("rollback version %+v err=%v", got, err)
	}
	gotTask, err := s.GetTask(ctx, task.ID)
	if err != nil || gotTask.Status != "running" {
		t.Fatalf("rollback task %+v err=%v", gotTask, err)
	}
	c.Next = []string{"relevance"}
	if err := s.CompleteTask(ctx, task, c, libraryNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	mustLibraryCount(t, s, "library_outputs", 1)
	mustLibraryCount(t, s, "library_runs", 1)
	mustLibraryCount(t, s, "library_tasks", 2)
	if err := s.CompleteTask(ctx, task, c, libraryNow.Add(time.Second)); !errors.Is(err, library.ErrLease) {
		t.Fatalf("duplicate completion=%v", err)
	}
	relTask := claimFixture(t, s, "relevance", libraryNow.Add(time.Second))
	if relTask.Generation != 0 {
		t.Fatal("next generation mismatch")
	}
}

func TestLibraryGenerationPointersAndComparisonAnalysisBinding(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, ":memory:")
	defer s.Close()
	v := versionFixture(1, "v2")
	v.Origin = "previous-fetch"
	if err := s.UpsertVersion(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTask(ctx, v.Identity, "analyze", 0); err != nil {
		t.Fatal(err)
	}
	older := claimFixture(t, s, "analyze", libraryNow)
	if err := s.EnqueueTask(ctx, v.Identity, "analyze", 1); err != nil {
		t.Fatal(err)
	}
	newer := claimFixture(t, s, "analyze", libraryNow)
	analysis := library.Analysis{PaperVersionID: v.Identity.Key(), SchemaVersion: library.SchemaVersion, Model: "new-model", Content: library.AnalysisContent{TitleZH: "new"}}
	if err := s.CompleteTask(ctx, newer, library.Completion{Analysis: &analysis}, libraryNow); err != nil {
		t.Fatal(err)
	}
	_, newID, err := s.Analysis(ctx, v.Identity)
	if err != nil {
		t.Fatal(err)
	}
	analysis.Model = "old-model"
	if err := s.CompleteTask(ctx, older, library.Completion{Analysis: &analysis}, libraryNow); err != nil {
		t.Fatal(err)
	}
	oldAnalysis, oldID, err := s.AnalysisForTask(ctx, older)
	if err != nil || oldAnalysis.Model != "old-model" || oldID == newID {
		t.Fatalf("exact old analysis=%+v id=%d err=%v", oldAnalysis, oldID, err)
	}
	if _, _, err := s.AnalysisForTask(ctx, library.Task{Identity: v.Identity, Generation: 2}); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("missing exact generation=%v", err)
	}
	got, id, err := s.Analysis(ctx, v.Identity)
	if err != nil || id != newID || got.Model != "new-model" {
		t.Fatalf("analysis pointer %+v %d err=%v", got, id, err)
	}
	if err := s.EnqueueTask(ctx, v.Identity, "compare", 1); err != nil {
		t.Fatal(err)
	}
	cmpTask := claimFixture(t, s, "compare", libraryNow)
	cmp := library.Comparison{AnalysisID: 9999, PreviousVersion: "v1", Status: "ready"}
	if err := s.CompleteTask(ctx, cmpTask, library.Completion{Comparison: &cmp}, libraryNow); !errors.Is(err, library.ErrInvalid) {
		t.Fatalf("foreign analysis accepted: %v", err)
	}
	cmp.AnalysisID = oldID
	if err := s.CompleteTask(ctx, cmpTask, library.Completion{Comparison: &cmp}, libraryNow); !errors.Is(err, library.ErrInvalid) {
		t.Fatalf("cross-generation comparison accepted=%v", err)
	}
	cmp.AnalysisID = newID
	cmp.PreviousVersion = "v0"
	if err := s.CompleteTask(ctx, cmpTask, library.Completion{Comparison: &cmp}, libraryNow); !errors.Is(err, library.ErrInvalid) {
		t.Fatalf("wrong prior version: %v", err)
	}
	cmp.PreviousVersion = "v1"
	if err := s.CompleteTask(ctx, cmpTask, library.Completion{Comparison: &cmp}, libraryNow); err != nil {
		t.Fatal(err)
	}
	detail, err := s.LibraryDetail(ctx, v.Identity)
	if err != nil || detail.Comparison.Status != "ready" || detail.Comparison.AnalysisID != newID {
		t.Fatalf("detail %+v err=%v", detail, err)
	}
	if err := s.EnqueueTask(ctx, v.Identity, "analyze", 2); err != nil {
		t.Fatal(err)
	}
	task := claimFixture(t, s, "analyze", libraryNow)
	analysis.Model = "reparse"
	if err := s.CompleteTask(ctx, task, library.Completion{Analysis: &analysis}, libraryNow); err != nil {
		t.Fatal(err)
	}
	detail, err = s.LibraryDetail(ctx, v.Identity)
	if err != nil || detail.Comparison.Status != "pending" || detail.Comparison.PreviousVersion != "v1" {
		t.Fatalf("new analysis stale comparison %+v err=%v", detail.Comparison, err)
	}
	first := versionFixture(1, "v1")
	if err := s.UpsertVersion(ctx, first); err != nil {
		t.Fatal(err)
	}
	detail, err = s.LibraryDetail(ctx, first.Identity)
	if err != nil || detail.Comparison.Status != "not_applicable" || len(detail.Tasks) != 0 {
		t.Fatalf("v1 comparison %+v err=%v", detail, err)
	}
	mustLibraryCount(t, s, "library_events", 0)
	mustLibraryCount(t, s, "library_batches", 0)
}

func TestLibraryNonDirectReanalysisInvalidatesPointersAndPreservesHistory(t *testing.T) {
	for _, level := range []string{"unrelated", "uncertain"} {
		t.Run(level, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t, ":memory:")
			defer s.Close()
			v := versionFixture(1, "v2")
			if err := s.UpsertVersion(ctx, v); err != nil {
				t.Fatal(err)
			}
			if err := s.EnqueueTask(ctx, v.Identity, "analyze", 0); err != nil {
				t.Fatal(err)
			}
			old := claimFixture(t, s, "analyze", libraryNow)
			direct := library.Relevance{Level: "direct", DirectlyRelated: true, Topics: []string{"search"}, Rationale: "old screening"}
			a := library.Analysis{PaperVersionID: v.Key(), Model: "old", Content: library.AnalysisContent{TitleZH: "obsolete analysis", Relevance: direct}}
			cmp := library.Comparison{PreviousVersion: "v1", Status: "ready", Reason: "old comparison"}
			if err := s.CompleteTask(ctx, old, library.Completion{Relevance: &direct, Analysis: &a, Comparison: &cmp}, libraryNow); err != nil {
				t.Fatal(err)
			}
			_, oldID, err := s.Analysis(ctx, v.Identity)
			if err != nil {
				t.Fatal(err)
			}
			var oldAnalysis, oldComparison []byte
			if err := s.db.QueryRow(`SELECT a.data,c.data FROM library_versions v JOIN library_outputs a ON a.id=v.analysis_id JOIN library_outputs c ON c.id=v.comparison_id`).Scan(&oldAnalysis, &oldComparison); err != nil {
				t.Fatal(err)
			}
			if err := s.EnqueueTask(ctx, v.Identity, "relevance", 1); err != nil {
				t.Fatal(err)
			}
			newTask := claimFixture(t, s, "relevance", libraryNow)
			if err := s.FailTask(ctx, newTask, "paused", "budget", time.Time{}, libraryNow); err != nil {
				t.Fatal(err)
			}
			detail, err := s.LibraryDetail(ctx, v.Identity)
			if err != nil || detail.AnalysisID != oldID || detail.Comparison.Status != "ready" || detail.Relevance.Level != "direct" {
				t.Fatalf("unfinished reanalysis lost visible results: %+v err=%v", detail, err)
			}
			if err := s.RetryTask(ctx, newTask.ID); err != nil {
				t.Fatal(err)
			}
			newTask = claimFixture(t, s, "relevance", libraryNow)
			nonDirect := library.Relevance{Level: level, Rationale: "new screening"}
			// 指针撤销必须随整个完成事务提交，不能在后继校验失败时提前生效。
			if err := s.CompleteTask(ctx, newTask, library.Completion{Relevance: &nonDirect, Next: []string{"invalid"}}, libraryNow); !errors.Is(err, library.ErrInvalid) {
				t.Fatalf("invalid completion=%v", err)
			}
			if _, id, err := s.Analysis(ctx, v.Identity); err != nil || id != oldID {
				t.Fatalf("rollback lost analysis id=%d err=%v", id, err)
			}
			if err := s.CompleteTask(ctx, newTask, library.Completion{Relevance: &nonDirect}, libraryNow); err != nil {
				t.Fatal(err)
			}
			detail, err = s.LibraryDetail(ctx, v.Identity)
			if err != nil || detail.Relevance.Level != level || detail.Analysis != nil || detail.AnalysisID != 0 || detail.Comparison.AnalysisID != 0 || detail.Comparison.Status == "ready" || len(detail.RelevanceHistory) != 2 {
				t.Fatalf("obsolete detail output=%+v err=%v", detail, err)
			}
			if _, _, err := s.Analysis(ctx, v.Identity); !errors.Is(err, library.ErrNotFound) {
				t.Fatalf("obsolete analysis lookup=%v", err)
			}
			for _, q := range []library.Query{{Relevance: "all"}, {Relevance: level}, {Relevance: "direct"}, {Relevance: "all", Q: "obsolete analysis"}} {
				page, err := s.BrowseLibrary(ctx, q)
				want := 1
				if q.Relevance == "direct" || q.Q != "" {
					want = 0
				}
				if err != nil || page.Total != want || (want == 1 && page.Items[0].AnalysisID != 0) {
					t.Fatalf("obsolete browse output query=%+v page=%+v err=%v", q, page, err)
				}
			}
			var aid, cid sql.NullInt64
			var ag, cg int
			if err := s.db.QueryRow(`SELECT analysis_id,comparison_id,analysis_generation,comparison_generation FROM library_versions`).Scan(&aid, &cid, &ag, &cg); err != nil || aid.Valid || cid.Valid || ag != 1 || cg != 1 {
				t.Fatalf("invalidated pointers=%v/%v generations=%d/%d err=%v", aid, cid, ag, cg, err)
			}
			var savedAnalysis, savedComparison []byte
			if err := s.db.QueryRow(`SELECT data FROM library_outputs WHERE id=?`, oldID).Scan(&savedAnalysis); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT data FROM library_outputs WHERE kind='comparison'`).Scan(&savedComparison); err != nil {
				t.Fatal(err)
			}
			if string(savedAnalysis) != string(oldAnalysis) || string(savedComparison) != string(oldComparison) {
				t.Fatal("immutable analysis/comparison history changed")
			}
			historical, id, err := s.AnalysisForTask(ctx, old)
			if err != nil || id != oldID || historical.Model != "old" {
				t.Fatalf("history lost analysis=%+v id=%d err=%v", historical, id, err)
			}
		})
	}
}

func TestLibrarySupersededGenerationCompletesOnlyIntoHistory(t *testing.T) {
	for _, stage := range []string{"metadata", "relevance", "non-direct-relevance", "document", "saved-document", "analyze", "compare"} {
		for _, newerSucceeded := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/newer-succeeded=%t", stage, newerSucceeded), func(t *testing.T) {
				ctx := context.Background()
				s := openTestStore(t, ":memory:")
				defer s.Close()
				v := versionFixture(1, "v2")
				if err := s.UpsertVersion(ctx, v); err != nil {
					t.Fatal(err)
				}
				taskStage := stage
				if stage == "saved-document" {
					taskStage = "document"
				} else if stage == "non-direct-relevance" {
					taskStage = "relevance"
				}
				var boundID int64
				if stage == "compare" {
					if err := s.EnqueueTask(ctx, v.Identity, "analyze", 0); err != nil {
						t.Fatal(err)
					}
					a := library.Analysis{PaperVersionID: v.Key(), Model: "existing"}
					if err := s.CompleteTask(ctx, claimFixture(t, s, "analyze", libraryNow), library.Completion{Analysis: &a}, libraryNow); err != nil {
						t.Fatal(err)
					}
					_, boundID, _ = s.Analysis(ctx, v.Identity)
				}
				if err := s.EnqueueTask(ctx, v.Identity, taskStage, 0); err != nil {
					t.Fatal(err)
				}
				old := claimFixture(t, s, taskStage, libraryNow)
				if err := s.EnqueueTask(ctx, v.Identity, "relevance", 1); err != nil {
					t.Fatal(err)
				}
				if newerSucceeded {
					newTask := claimFixture(t, s, "relevance", libraryNow)
					rel := library.Relevance{Level: "unrelated", Rationale: "new generation"}
					if err := s.CompleteTask(ctx, newTask, library.Completion{Relevance: &rel}, libraryNow); err != nil {
						t.Fatal(err)
					}
				}
				before, err := s.LibraryDetail(ctx, v.Identity)
				if err != nil {
					t.Fatal(err)
				}
				var beforePointers string
				if err := s.db.QueryRow(`SELECT json_array(relevance_id,document_id,analysis_id,comparison_id,metadata_generation,relevance_generation,document_generation,analysis_generation,comparison_generation) FROM library_versions`).Scan(&beforePointers); err != nil {
					t.Fatal(err)
				}
				c := library.Completion{Run: library.Run{Model: "old history"}}
				kind := taskStage
				direct := library.Relevance{Level: "direct", DirectlyRelated: true, Topics: []string{"search"}, Rationale: "old generation"}
				doc := library.Document{ID: "late-document", Identity: v.Identity, Quality: "ready"}
				switch stage {
				case "metadata":
					v.Title = "obsolete metadata"
					c.Version, c.Next = &v, []string{"relevance"}
				case "relevance", "non-direct-relevance":
					if stage == "non-direct-relevance" {
						direct.Level, direct.DirectlyRelated = "uncertain", false
					}
					c.Relevance, c.Next = &direct, []string{"document"}
				case "document":
					c.Document, c.Next = &doc, []string{"analyze"}
				case "saved-document":
					if err := s.SaveTaskDocument(ctx, old, doc, libraryNow); err != nil {
						t.Fatal(err)
					}
					c.Document, c.Next = &doc, []string{"analyze"}
				case "analyze":
					kind = "analysis"
					c.Analysis = &library.Analysis{PaperVersionID: v.Key(), Model: "late analysis"}
					c.Relevance, c.Next = &direct, []string{"compare"}
				case "compare":
					kind = "comparison"
					c.Comparison = &library.Comparison{AnalysisID: boundID, PreviousVersion: "v1", Status: "ready", Reason: "late comparison"}
				}
				if err := s.CompleteTask(ctx, old, c, libraryNow); err != nil {
					t.Fatal(err)
				}
				after, err := s.LibraryDetail(ctx, v.Identity)
				if err != nil || !reflect.DeepEqual(after.Version, before.Version) || !reflect.DeepEqual(after.Relevance, before.Relevance) || after.AnalysisID != before.AnalysisID || !reflect.DeepEqual(after.Comparison, before.Comparison) || len(after.Tasks) != len(before.Tasks) {
					t.Fatalf("stale completion published or enqueued: before=%+v after=%+v err=%v", before, after, err)
				}
				var afterPointers string
				if err := s.db.QueryRow(`SELECT json_array(relevance_id,document_id,analysis_id,comparison_id,metadata_generation,relevance_generation,document_generation,analysis_generation,comparison_generation) FROM library_versions`).Scan(&afterPointers); err != nil || afterPointers != beforePointers {
					t.Fatalf("stale pointers before=%s after=%s err=%v", beforePointers, afterPointers, err)
				}
				var count int
				if err := s.db.QueryRow(`SELECT COUNT(*) FROM library_outputs WHERE task_id=? AND kind=?`, old.ID, kind).Scan(&count); err != nil || count != 1 {
					t.Fatalf("history output count=%d err=%v", count, err)
				}
				got, err := s.GetTask(ctx, old.ID)
				if err != nil || got.Status != "succeeded" || got.LeaseToken != "" || after.Runs[len(after.Runs)-1].Model != "old history" {
					t.Fatalf("stale completion history task=%+v err=%v", got, err)
				}
			})
		}
	}
}

func TestLibraryReferenceDocumentPersistsWithoutSchedulingAndFirstAnnouncementQueues(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, ":memory:")
	defer s.Close()
	current := versionFixture(1, "v2")
	previous := versionFixture(1, "v1")
	previous.Origin = "comparison_reference"
	if err := s.UpsertVersion(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertVersion(ctx, previous); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTask(ctx, current.Identity, "analyze", 0); err != nil {
		t.Fatal(err)
	}
	task := claimFixture(t, s, "analyze", libraryNow)
	currentDoc := library.Document{ID: "current-document", Identity: current.Identity, Source: library.Artifact{Path: "current.pdf", SHA256: "current-hash"}}
	previousDoc := library.Document{ID: "previous-document", Identity: previous.Identity, Source: library.Artifact{Path: "previous.pdf", SHA256: "previous-hash"}, Text: library.Artifact{Path: "previous.txt", SHA256: "text-hash"}}
	if err := s.SaveReferenceDocument(ctx, task, previousDoc, libraryNow); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveReferenceDocument(ctx, task, previousDoc, libraryNow); err != nil {
		t.Fatal(err)
	}
	altered := previousDoc
	altered.ID = "replacement"
	if err := s.SaveReferenceDocument(ctx, task, altered, libraryNow); !errors.Is(err, library.ErrInvalid) {
		t.Fatalf("reference overwrite=%v", err)
	}
	if err := s.SaveReferenceDocument(ctx, task, currentDoc, libraryNow); !errors.Is(err, library.ErrInvalid) {
		t.Fatalf("wrong reference version=%v", err)
	}
	if err := s.SaveReferenceDocument(ctx, task, previousDoc, libraryNow.Add(time.Minute)); !errors.Is(err, library.ErrLease) {
		t.Fatalf("expired reference write=%v", err)
	}
	if err := s.FailTask(ctx, task, "paused", "budget", time.Time{}, libraryNow); err != nil {
		t.Fatal(err)
	}
	mustLibraryCount(t, s, "library_outputs", 1)
	pausedDocuments, err := s.Documents(ctx, previous.Identity)
	if err != nil || len(pausedDocuments) != 1 {
		t.Fatalf("paused reference=%+v err=%v", pausedDocuments, err)
	}
	if err := s.SaveReferenceDocument(ctx, task, previousDoc, libraryNow); !errors.Is(err, library.ErrLease) {
		t.Fatalf("paused reference write=%v", err)
	}
	if err := s.RetryTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	task = claimFixture(t, s, "analyze", libraryNow)
	if err := s.CompleteTask(ctx, task, library.Completion{Document: &currentDoc, ReferenceDocuments: []library.Document{previousDoc}}, libraryNow); err != nil {
		t.Fatal(err)
	}
	mustLibraryCount(t, s, "library_outputs", 2)
	mustLibraryCount(t, s, "library_tasks", 1)
	mustLibraryCount(t, s, "library_events", 0)
	documents, err := s.Documents(ctx, previous.Identity)
	if err != nil || len(documents) != 1 || documents[0].ID != previousDoc.ID {
		t.Fatalf("reference documents=%+v err=%v", documents, err)
	}
	detail, err := s.LibraryDetail(ctx, previous.Identity)
	if err != nil || len(detail.Documents) != 1 || len(detail.Tasks) != 0 {
		t.Fatalf("reference detail=%+v err=%v", detail, err)
	}
	var pointer sql.NullInt64
	if err := s.db.QueryRow(`SELECT document_id FROM library_versions WHERE source=? AND paper_id=? AND version=?`, identityArgs(previous.Identity)...).Scan(&pointer); err != nil {
		t.Fatal(err)
	}
	if pointer.Valid {
		t.Fatal("reference document advanced pointer")
	}
	b := batchFixture(1)
	if err := s.SaveCategoryBatch(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCategoryBatch(ctx, b); err != nil {
		t.Fatal(err)
	}
	mustLibraryCount(t, s, "library_tasks", 2)
	got := claimFixture(t, s, "metadata", libraryNow)
	if got.Identity != previous.Identity {
		t.Fatalf("announcement task=%+v", got)
	}
}

func TestLibraryObservationBatchArtifactsAndFilteredClaim(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, ":memory:")
	defer s.Close()
	observation := batchFixture(1)
	observation.Date = ""
	observation.Events[0].Date = ""
	if err := s.SaveSourceObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSourceObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	mustLibraryCount(t, s, "library_source_observations", 1)
	mustLibraryCount(t, s, "library_batches", 0)
	mustLibraryCount(t, s, "library_events", 0)
	v, err := s.GetVersion(ctx, observation.Versions[0].Identity)
	if err != nil || v.AnnouncementDate != "" || v.Origin != "announcement_unverified" {
		t.Fatalf("observation version=%+v err=%v", v, err)
	}
	status, err := s.LibraryStatus(ctx)
	if err != nil || len(status.Batches) != 1 || status.Batches[0].Date != "" || status.Batches[0].Completeness != "incomplete" || status.Batches[0].Versions != nil {
		t.Fatalf("observation status=%+v err=%v", status, err)
	}
	b := batchFixture(2)
	if err := s.SaveCategoryBatch(ctx, b); err != nil {
		t.Fatal(err)
	}
	b.Artifacts = []library.Artifact{{Path: "feed-updated.html", SHA256: "newhash"}}
	if err := s.SaveCategoryBatch(ctx, b); err != nil {
		t.Fatal(err)
	}
	status, err = s.LibraryStatus(ctx)
	if err != nil || len(status.Batches) != 2 || len(status.Batches[0].Artifacts) != 2 {
		t.Fatalf("merged artifacts=%+v err=%v", status, err)
	}
	identity := b.Versions[1].Identity
	task, err := s.ClaimTaskQuery(ctx, "metadata", libraryNow, time.Minute, library.Query{Batch: "cs.IR/2026-10-02"}, &identity)
	if err != nil || task.Identity != identity {
		t.Fatalf("filtered claim=%+v err=%v", task, err)
	}
	unclaimed, err := s.GetTask(ctx, 1)
	if err != nil || unclaimed.Status != "queued" || unclaimed.Attempt != 0 {
		t.Fatalf("unrelated task changed=%+v err=%v", unclaimed, err)
	}
	if _, err := s.ClaimTaskQuery(ctx, "metadata", libraryNow, time.Minute, library.Query{Batch: "cs.LG/2026-10-02"}, nil); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("wrong batch claim=%v", err)
	}
	remaining, err := s.ClaimTaskQuery(ctx, "metadata", libraryNow, time.Minute, library.Query{Batch: "2026-10-02"}, nil)
	if err != nil || remaining.Identity != b.Versions[0].Identity {
		t.Fatalf("date claim=%+v err=%v", remaining, err)
	}
}

func TestLibraryBlockedDocumentAndComparisonReasons(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, ":memory:")
	defer s.Close()
	v := versionFixture(1, "v2")
	if err := s.UpsertVersion(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTask(ctx, v.Identity, "document", 0); err != nil {
		t.Fatal(err)
	}
	task := claimFixture(t, s, "document", libraryNow)
	doc := library.Document{ID: "quality-report", Identity: v.Identity, Quality: "blocked", Issues: []string{"incomplete extraction"}, Source: library.Artifact{Path: "paper.pdf", SHA256: "source"}}
	if err := s.SaveTaskDocument(ctx, task, doc, libraryNow); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTaskDocument(ctx, task, doc, libraryNow); err != nil {
		t.Fatal(err)
	}
	if err := s.FailTask(ctx, task, "blocked", "quality", time.Time{}, libraryNow); err != nil {
		t.Fatal(err)
	}
	detail, err := s.LibraryDetail(ctx, v.Identity)
	if err != nil || len(detail.Documents) != 1 || detail.Documents[0].Quality != "blocked" {
		t.Fatalf("quality detail=%+v err=%v", detail, err)
	}
	if err := s.RetryTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	task = claimFixture(t, s, "document", libraryNow)
	if err := s.CompleteTask(ctx, task, library.Completion{Document: &doc}, libraryNow); err != nil {
		t.Fatal(err)
	}
	mustLibraryCount(t, s, "library_outputs", 1)
	if err := s.EnqueueTask(ctx, v.Identity, "analyze", 0); err != nil {
		t.Fatal(err)
	}
	task = claimFixture(t, s, "analyze", libraryNow)
	analysis := library.Analysis{PaperVersionID: v.Identity.Key()}
	if err := s.CompleteTask(ctx, task, library.Completion{Analysis: &analysis, Next: []string{"compare"}}, libraryNow); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"blocked", "paused", "retry_wait"} {
		task = claimFixture(t, s, "compare", libraryNow)
		if err := s.FailTask(ctx, task, state, "previous version unavailable", time.Time{}, libraryNow); err != nil {
			t.Fatal(err)
		}
		detail, err = s.LibraryDetail(ctx, v.Identity)
		want := state
		if state == "retry_wait" {
			want = "pending"
		}
		if err != nil || detail.Comparison.Status != want || detail.Comparison.Reason != "previous version unavailable" {
			t.Fatalf("comparison=%+v err=%v", detail.Comparison, err)
		}
		if err := s.RetryTask(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLibraryBrowseFiltersLiteralSearchAndMetadataMerge(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, ":memory:")
	defer s.Close()
	b := batchFixture(3)
	b.Versions[0].Title = "literal 50%_word"
	if err := s.SaveCategoryBatch(ctx, b); err != nil {
		t.Fatal(err)
	}
	for i, level := range []string{"direct", "unrelated", "uncertain"} {
		id := b.Versions[i].Identity
		if err := s.EnqueueTask(ctx, id, "relevance", 0); err != nil {
			t.Fatal(err)
		}
		task := claimFixture(t, s, "relevance", libraryNow)
		r := library.Relevance{Level: level, DirectlyRelated: level == "direct", Rationale: "source evidence", Topics: []string{}}
		if level == "direct" {
			r.Topics = []string{"search"}
		}
		if err := s.CompleteTask(ctx, task, library.Completion{Relevance: &r}, libraryNow); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		q    library.Query
		want int
	}{{library.Query{}, 1}, {library.Query{Relevance: "all"}, 3}, {library.Query{Relevance: "unrelated"}, 1}, {library.Query{Relevance: "all", Topic: "search"}, 1}, {library.Query{Relevance: "all", Q: "%_"}, 1}, {library.Query{Relevance: "all", Q: "50%_word"}, 1}, {library.Query{Relevance: "all", Q: "no match"}, 0}, {library.Query{Relevance: "all", Status: "metadata:queued"}, 3}, {library.Query{Relevance: "all", Status: "relevance:succeeded"}, 3}}
	for _, tc := range cases {
		page, err := s.BrowseLibrary(ctx, tc.q)
		if err != nil || page.Total != tc.want || len(page.Items) != tc.want {
			t.Fatalf("query %+v total=%d len=%d want=%d err=%v", tc.q, page.Total, len(page.Items), tc.want, err)
		}
	}
	for _, q := range []library.Query{{Relevance: "bad"}, {Batch: "bad"}, {Status: "bad"}, {Page: -1}, {PageSize: 101}, {Topic: "bad"}} {
		if _, err := s.BrowseLibrary(ctx, q); !errors.Is(err, library.ErrInvalid) {
			t.Fatalf("bad query %+v err=%v", q, err)
		}
	}
	id := b.Versions[0].Identity
	refresh := library.Version{Identity: id, Title: "new title", MetadataArtifacts: []library.Artifact{{Path: "metadata/one.xml", SHA256: "hash"}}}
	if err := s.UpsertVersion(ctx, refresh); err != nil {
		t.Fatal(err)
	}
	refresh.MetadataArtifacts = []library.Artifact{{Path: "metadata/two.xml", SHA256: "hash2"}}
	if err := s.UpsertVersion(ctx, refresh); err != nil {
		t.Fatal(err)
	}
	v, err := s.GetVersion(ctx, id)
	if err != nil || v.Origin != "announcement" || v.AnnouncementDate != b.Date || v.CapturedAt != b.CapturedAt || len(v.MetadataArtifacts) != 2 || v.Abstract == "" {
		t.Fatalf("metadata merge %+v err=%v", v, err)
	}
	gap := library.Gap{Category: "cs.IR", After: "2026-09-29", Before: "2026-10-02", Reason: "missed window"}
	if err := s.SaveGap(ctx, gap); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveGap(ctx, gap); err != nil {
		t.Fatal(err)
	}
	status, err := s.LibraryStatus(ctx)
	if err != nil || len(status.Gaps) != 1 {
		t.Fatalf("status gaps %+v err=%v", status.Gaps, err)
	}
	detail, err := s.LibraryDetail(ctx, id)
	if err != nil || len(detail.RelevanceHistory) != 1 || detail.Relevance == nil || !strings.Contains(detail.Relevance.Rationale, "evidence") {
		t.Fatalf("relevance detail %+v err=%v", detail, err)
	}
}
