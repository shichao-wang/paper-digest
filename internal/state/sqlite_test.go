package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/shichao-wang/paper-digest/internal/digest"
	"github.com/shichao-wang/paper-digest/internal/papers"
)

func TestClaimDayUsesTopicAndDateUniqueness(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state.sqlite"))
	defer store.Close()

	const workers = 12
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := store.ClaimDay(ctx, "topic-a", "2026-09-27")
			if err == nil && (job.Status != statusNew || job.Topic != "topic-a" || job.Date != "2026-09-27") {
				errs <- ErrInvalidState
				return
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ClaimDay returned an error: %v", err)
		}
	}

	var count int
	if err := store.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM jobs WHERE topic = ? AND date = ?`, "topic-a", "2026-09-27").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one job for the topic/date pair, got %d", count)
	}

	if _, err := store.ClaimDay(ctx, "topic-b", "2026-09-27"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimDay(ctx, "topic-a", "2026-09-28"); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.ListPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("expected three unique pending jobs, got %d", len(jobs))
	}
}

func TestInterruptedSendRecoversAsUnknownAndPapersResumeIndividually(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	store := openTestStore(t, dbPath)

	job, err := store.ClaimDay(ctx, "topic", "2026-09-27")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != statusNew {
		t.Fatalf("new job status = %q, want %q", job.Status, statusNew)
	}
	papersToSave := []papers.Paper{
		{ID: "2609.00001", Version: "1", Title: "Paper one"},
		{ID: "2609.00002", Version: "2", Title: "Paper two"},
	}
	if err := store.SaveCandidates(ctx, "topic", job.Date, papersToSave); err != nil {
		t.Fatal(err)
	}
	if job, err = store.GetJob(ctx, "topic", job.Date); err != nil || job.Status != statusProcessing {
		t.Fatalf("after saving candidates, job = %+v, err = %v", job, err)
	}
	if err := store.SaveCandidates(ctx, "topic", job.Date, []papers.Paper{{ID: "unexpected", Version: "1"}}); err != nil {
		t.Fatal(err)
	}

	pending, err := store.PendingPapers(ctx, "topic", job.Date)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ID != papersToSave[0].ID || pending[1].ID != papersToSave[1].ID {
		t.Fatalf("unexpected initial pending papers: %+v", pending)
	}

	first := digest.Summary{Text: "first saved summary", Model: "test-model"}
	if err := store.SaveSummary(ctx, "topic", job.Date, papersToSave[0].ID, first); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSummary(ctx, "topic", job.Date, papersToSave[0].ID, digest.Summary{Text: "replacement"}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.PendingPapers(ctx, "topic", job.Date)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != papersToSave[1].ID {
		t.Fatalf("expected only the unsummarized paper to remain, got %+v", pending)
	}
	if err := store.Ready(ctx, "topic", job.Date, "rendered message"); err == nil {
		t.Fatal("Ready succeeded while a paper summary was still pending")
	}

	second := digest.Summary{Text: "second saved summary", PromptVersion: "test-v1"}
	if err := store.SaveSummary(ctx, "topic", job.Date, papersToSave[1].ID, second); err != nil {
		t.Fatal(err)
	}
	completed, err := store.Completed(ctx, "topic", job.Date)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 2 || completed[0].Summary.Text != first.Text || completed[1].Summary.Text != second.Text || completed[0].Paper.Version != "1" || completed[1].Paper.Version != "2" {
		t.Fatalf("completed papers did not preserve per-paper summaries and versions: %+v", completed)
	}
	if err := store.Ready(ctx, "topic", job.Date, "rendered message"); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimSend(ctx, "topic", job.Date)
	if err != nil || !claimed {
		t.Fatalf("ClaimSend = %v, %v; want true, nil", claimed, err)
	}
	if err := store.MarkMissed(ctx, "topic", job.Date); err != nil {
		t.Fatal(err)
	}
	job, err = store.GetJob(ctx, "topic", job.Date)
	if err != nil || job.Status != statusSending {
		t.Fatalf("MarkMissed overwrote send intent: job=%+v err=%v", job, err)
	}
	claimed, err = store.ClaimSend(ctx, "topic", job.Date)
	if err != nil || claimed {
		t.Fatalf("second ClaimSend = %v, %v; want false, nil", claimed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestStore(t, dbPath)
	defer store.Close()
	job, err = store.GetJob(ctx, "topic", job.Date)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != statusUnknown || job.Message != "rendered message" {
		t.Fatalf("reopened interrupted send as %+v; want unknown with preserved message", job)
	}
	pending, err = store.PendingPapers(ctx, "topic", job.Date)
	if err != nil || len(pending) != 0 {
		t.Fatalf("reopened paper state has pending papers %+v, err=%v", pending, err)
	}
	completed, err = store.Completed(ctx, "topic", job.Date)
	if err != nil || len(completed) != 2 || completed[0].Summary.Text != first.Text || completed[1].Summary.Text != second.Text {
		t.Fatalf("reopened per-paper summaries = %+v, err=%v", completed, err)
	}
	if claimed, err := store.ClaimSend(ctx, "topic", job.Date); err != nil || claimed {
		t.Fatalf("ClaimSend after recovery = %v, %v; want false, nil", claimed, err)
	}
	if err := store.MarkMissed(ctx, "topic", job.Date); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSent(ctx, "topic", job.Date); err == nil {
		t.Fatal("MarkSent converted an unknown send outcome to sent")
	}
	job, err = store.GetJob(ctx, "topic", job.Date)
	if err != nil || job.Status != statusUnknown {
		t.Fatalf("unknown status was overwritten: job=%+v err=%v", job, err)
	}
	seen, err := store.Seen(ctx, "topic", papersToSave[0].ID)
	if err != nil || seen {
		t.Fatalf("unsent papers must not be recorded as recommendations: Seen=%v err=%v", seen, err)
	}
	pendingJobs, err := store.ListPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingJobs) != 0 {
		t.Fatalf("unknown send must not be auto-retried, pending jobs: %+v", pendingJobs)
	}
}

func TestMarkSentRecordsRecommendationsAndMissedCannotOverwrite(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state.sqlite"))
	defer store.Close()

	job, err := store.ClaimDay(ctx, "topic", "2026-09-27")
	if err != nil {
		t.Fatal(err)
	}
	paper := papers.Paper{ID: "2609.00003", Version: "1", Title: "Sent paper"}
	if err := store.SaveCandidates(ctx, "topic", job.Date, []papers.Paper{paper}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSummary(ctx, "topic", job.Date, paper.ID, digest.Summary{Text: "summary"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(ctx, "topic", job.Date, "message"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimSend(ctx, "topic", job.Date); err != nil || !claimed {
		t.Fatalf("ClaimSend = %v, %v; want true, nil", claimed, err)
	}
	if err := store.MarkSent(ctx, "topic", job.Date); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkMissed(ctx, "topic", job.Date); err != nil {
		t.Fatal(err)
	}
	job, err = store.GetJob(ctx, "topic", job.Date)
	if err != nil || job.Status != statusSent {
		t.Fatalf("MarkMissed overwrote sent job: job=%+v err=%v", job, err)
	}
	seen, err := store.Seen(ctx, "topic", paper.ID)
	if err != nil || !seen {
		t.Fatalf("sent paper should be recorded as recommended: Seen=%v err=%v", seen, err)
	}
	if claimed, err := store.ClaimSend(ctx, "topic", job.Date); err != nil || claimed {
		t.Fatalf("repeat ClaimSend = %v, %v; want false, nil", claimed, err)
	}
}

func TestMarkMissedCanAbandonReadyButNotSending(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state.sqlite"))
	defer store.Close()

	missed, err := store.ClaimDay(ctx, "topic", "2026-09-27")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCandidates(ctx, "topic", missed.Date, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(ctx, "topic", missed.Date, "ready but late"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkMissed(ctx, "topic", missed.Date); err != nil {
		t.Fatal(err)
	}
	missed, err = store.GetJob(ctx, "topic", missed.Date)
	if err != nil || missed.Status != statusMissed {
		t.Fatalf("ready job should transition to missed: job=%+v err=%v", missed, err)
	}
	if err := store.Ready(ctx, "topic", missed.Date, "must not replace missed"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("Ready on missed job error = %v, want ErrInvalidState", err)
	}
	if err := store.SaveSummary(ctx, "topic", missed.Date, "paper-id", digest.Summary{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("SaveSummary on missed job error = %v, want ErrInvalidState", err)
	}

	sending, err := store.ClaimDay(ctx, "topic", "2026-09-28")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCandidates(ctx, "topic", sending.Date, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(ctx, "topic", sending.Date, "send intent"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimSend(ctx, "topic", sending.Date); err != nil || !claimed {
		t.Fatalf("ClaimSend = %v, %v; want true, nil", claimed, err)
	}
	if err := store.MarkMissed(ctx, "topic", sending.Date); err != nil {
		t.Fatal(err)
	}
	sending, err = store.GetJob(ctx, "topic", sending.Date)
	if err != nil || sending.Status != statusSending {
		t.Fatalf("MarkMissed overwrote sending intent: job=%+v err=%v", sending, err)
	}
}

func TestBackupCreatesRestorableSnapshotAndRejectsExistingDestination(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := openTestStore(t, filepath.Join(dir, "state.sqlite"))
	defer store.Close()

	job, err := store.ClaimDay(ctx, "topic", "2026-09-27")
	if err != nil {
		t.Fatal(err)
	}
	paper := papers.Paper{ID: "2609.00004", Version: "1", Title: "Backup paper"}
	if err := store.SaveCandidates(ctx, "topic", job.Date, []papers.Paper{paper}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSummary(ctx, "topic", job.Date, paper.ID, digest.Summary{Text: "persisted summary"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(ctx, "topic", job.Date, "persisted message"); err != nil {
		t.Fatal(err)
	}

	backupPath := filepath.Join(dir, "backup.sqlite")
	if err := store.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}
	backup := openTestStore(t, backupPath)
	defer backup.Close()
	backupJob, err := backup.GetJob(ctx, "topic", job.Date)
	if err != nil || backupJob.Status != statusReady || backupJob.Message != "persisted message" {
		t.Fatalf("backup job = %+v, err = %v", backupJob, err)
	}
	completed, err := backup.Completed(ctx, "topic", job.Date)
	if err != nil || len(completed) != 1 || completed[0].Summary.Text != "persisted summary" {
		t.Fatalf("backup completed papers = %+v, err = %v", completed, err)
	}
	if _, err := store.ClaimDay(ctx, "topic", "2026-09-28"); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.GetJob(ctx, "topic", "2026-09-28"); err != ErrJobNotFound {
		t.Fatalf("backup should not include later source writes, got err = %v", err)
	}
	if err := store.Backup(ctx, backupPath); err == nil {
		t.Fatal("Backup overwrote an existing destination")
	}

	existingPath := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(existingPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Backup(ctx, existingPath); err == nil {
		t.Fatal("Backup replaced an existing non-database file")
	}
	contents, err := os.ReadFile(existingPath)
	if err != nil || string(contents) != "keep" {
		t.Fatalf("existing destination changed: contents=%q err=%v", contents, err)
	}
}

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	return store
}
