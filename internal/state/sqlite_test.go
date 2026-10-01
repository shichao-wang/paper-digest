package state

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/digest"
	"github.com/shichao-wang/paper-digest/internal/papers"
)

func TestExistingDatabaseGetsWebhookTableWithoutChangingJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	ctx := context.Background()
	store := openTestStore(t, path)
	if _, err := store.ClaimDay(ctx, "topic", "2026-09-27"); err != nil {
		t.Fatal(err)
	}
	// 模拟原有数据库：只有日报相关表，没有新设置表。
	if _, err := store.db.Exec(`DROP TABLE topic_webhooks`); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store = openTestStore(t, path)
	defer store.Close()
	if err := store.SetWebhook(ctx, "topic", ""); err != nil {
		t.Fatal(err)
	}
	if current, err := store.GetJob(ctx, "topic", "2026-09-27"); err != nil || current.Status != statusNew {
		t.Fatalf("schema升级影响旧job: %+v %v", current, err)
	}
}

func TestWebhookMigrationPersistenceAndClear(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store := openTestStore(t, path)
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("存储与迁移不得发送请求")
	}))
	defer server.Close()
	topics := []config.Topic{
		{ID: "topic-a", WebhookURL: "  " + server.URL + "/secret-a  "},
		{ID: "topic-b", WebhookURL: server.URL + "/secret-b"},
		{ID: "empty", WebhookURL: " "},
		{ID: "invalid", WebhookURL: "http://example.invalid/secret"},
		{ID: "credentials", WebhookURL: "https://user:secret@example.invalid"},
		{ID: "fragment", WebhookURL: server.URL + "#secret"},
	}
	for i := 0; i < 2; i++ {
		if err := store.MigrateWebhooks(ctx, topics); err != nil {
			t.Fatal(err)
		}
	}
	for topic, want := range map[string]string{"topic-a": server.URL + "/secret-a", "topic-b": server.URL + "/secret-b", "empty": "", "invalid": "", "missing": "", "credentials": "", "fragment": ""} {
		if got, err := store.Webhook(ctx, topic); err != nil || got != want {
			t.Fatalf("topic=%s err=%v unexpected webhook", topic, err)
		}
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM topic_webhooks`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("migration count=%d err=%v", count, err)
	}
	if err := store.SetWebhook(ctx, "topic-a", "  "); err != nil {
		t.Fatal(err)
	}
	if err := store.SetWebhook(ctx, "topic-b", "  "+server.URL+"/updated  "); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store = openTestStore(t, path)
	defer store.Close()
	if err := store.MigrateWebhooks(ctx, topics); err != nil {
		t.Fatal(err)
	}
	for topic, want := range map[string]string{"topic-a": "", "topic-b": server.URL + "/updated"} {
		if got, err := store.Webhook(ctx, topic); err != nil || got != want {
			t.Fatalf("持久化或迁移覆盖了设置: topic=%s err=%v", topic, err)
		}
	}
	for _, value := range []string{"http://example.invalid/secret", "https://user:secret@example.invalid", "https://example.invalid/#secret"} {
		if err := store.SetWebhook(ctx, "topic-a", value); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid URL/error leakage: %v", err)
		}
	}
	store.Close()
	if err := store.SetWebhook(ctx, "topic-a", server.URL+"/secret"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("save error leakage: %v", err)
	}
	if _, err := store.Webhook(ctx, "topic-a"); err == nil {
		t.Fatal("关闭数据库应返回读错误")
	}
	if err := store.MigrateWebhooks(ctx, topics); err == nil {
		t.Fatal("关闭数据库应返回迁移错误")
	}
}

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
	if job.Status != statusSending || job.Message != "rendered message" {
		t.Fatalf("opening database changed send intent: %+v", job)
	}
	if err := store.RecoverInterruptedSends(ctx); err != nil {
		t.Fatal(err)
	}
	job, err = store.GetJob(ctx, "topic", job.Date)
	if err != nil || job.Status != statusUnknown || job.Message != "rendered message" {
		t.Fatalf("worker recovery returned %+v, err=%v; want unknown with preserved message", job, err)
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

func TestSQLiteFilesArePrivateFromCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	// 直接检验 SQLite 打开前的预创建阶段，避免仅在写入后 chmod 掩盖创建窗口。
	if err := protectSQLiteFile(path, true); err != nil {
		t.Fatal(err)
	}
	assertPrivateFile(t, path)
	// 使用原始驱动且不调用 protectFiles，核实新侧文件确实继承 0600，
	// 而不是被 Open 结束时的权限复查事后修正。
	dsn, err := sqliteDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE permission_probe (value TEXT); INSERT INTO permission_probe VALUES ('secret')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		assertPrivateFile(t, path+suffix)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, path)
	defer store.Close()
	if err := store.SetWebhook(context.Background(), "topic", "https://example.invalid/secret"); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		assertPrivateFile(t, path+suffix)
	}
	// 新连接重新创建侧文件时依然沿用主 DB 的 0600。
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	defer store.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		assertPrivateFile(t, path+suffix)
	}
}

func TestOpenTightensExistingDatabaseAndSideFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.sqlite")
	ctx := context.Background()
	original := openTestStore(t, path)
	defer original.Close()
	if _, err := original.ClaimDay(ctx, "topic", "2026-09-27"); err != nil {
		t.Fatal(err)
	}
	if err := original.SetWebhook(ctx, "topic", "https://example.invalid/old-secret"); err != nil {
		t.Fatal(err)
	}
	// 保持旧连接打开，确保 WAL/SHM 真实存在且没有被关闭流程删除。
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	second := openTestStore(t, path)
	defer second.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		assertPrivateFile(t, path+suffix)
	}
	if webhook, err := second.Webhook(ctx, "topic"); err != nil || webhook != "https://example.invalid/old-secret" {
		t.Fatalf("权限收紧后旧设置不应改变: err=%v", err)
	}
	if job, err := second.GetJob(ctx, "topic", "2026-09-27"); err != nil || job.Status != statusNew {
		t.Fatalf("权限收紧后发送状态不应改变: job=%+v err=%v", job, err)
	}
}

func TestWebhookWritesTightenPermissionsBeforeSaving(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		t.Run(map[bool]string{false: "save", true: "migrate"}[migrate], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.sqlite")
			store := openTestStore(t, path)
			defer store.Close()
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if err := os.Chmod(path+suffix, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			webhook := "https://example.invalid/secret"
			var err error
			if migrate {
				err = store.MigrateWebhooks(ctx, []config.Topic{{ID: "topic", WebhookURL: webhook}})
			} else {
				err = store.SetWebhook(ctx, "topic", webhook)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"", "-wal", "-shm"} {
				assertPrivateFile(t, path+suffix)
			}
		})
	}
}

func TestSQLitePathsAndConcurrentCreation(t *testing.T) {
	for _, uri := range []bool{false, true} {
		t.Run(map[bool]string{false: "path", true: "uri"}[uri], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state ?#.sqlite")
			input := path
			if uri {
				input = (&url.URL{Scheme: "file", Path: path}).String()
			}
			const workers = 4
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make(chan error, workers)
			for i := 0; i < workers; i++ {
				wg.Go(func() {
					<-start
					errs <- protectSQLiteFile(path, true)
				})
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			store := openTestStore(t, input)
			defer store.Close()
			for i := 0; i < workers; i++ {
				wg.Go(func() {
					if err := store.SetWebhook(context.Background(), "topic", "https://example.invalid/secret"); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			for _, suffix := range []string{"", "-wal", "-shm"} {
				assertPrivateFile(t, path+suffix)
			}
		})
	}
	for _, input := range []string{":memory:", "file::memory:", "file:test?mode=memory&cache=shared"} {
		store := openTestStore(t, input)
		if err := store.SetWebhook(context.Background(), "topic", "https://example.invalid/secret"); err != nil {
			t.Fatal(err)
		}
		store.Close()
	}
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	if _, err := Open((&url.URL{Scheme: "file", Path: missing, RawQuery: "mode=rw"}).String()); err == nil {
		t.Fatal("mode=rw 不应创建缺失数据库")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mode=rw 创建了文件: %v", err)
	}
}

func TestPermissionErrorsStopOpenAndWebhookWrites(t *testing.T) {
	// 非普通侧文件应明确失败，不能当作缺失文件跳过。
	path := filepath.Join(t.TempDir(), "state.sqlite")
	if err := os.Mkdir(path+"-wal", 0o700); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(path); err == nil {
		store.Close()
		t.Fatal("不安全侧文件不应打开成功")
	}
	path = filepath.Join(t.TempDir(), "state.sqlite")
	store := openTestStore(t, path)
	defer store.Close()
	// 使用 journal 路径模拟权限保护失败，避免篡改 SQLite 正在使用的 WAL/SHM。
	if err := os.Mkdir(path+"-journal", 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.SetWebhook(ctx, "saved", "https://example.invalid/secret"); err == nil {
		t.Fatal("权限保护失败仍写入了设置")
	}
	if err := store.MigrateWebhooks(ctx, []config.Topic{{ID: "migrated", WebhookURL: "https://example.invalid/secret"}}); err == nil {
		t.Fatal("权限保护失败仍迁移了设置")
	}
	for _, topic := range []string{"saved", "migrated"} {
		if webhook, err := store.Webhook(ctx, topic); err != nil || webhook != "" {
			t.Fatalf("失败写入修改了设置: topic=%s err=%v", topic, err)
		}
	}
}

func TestBackupIsPrivateAndCleansUpFailures(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := openTestStore(t, filepath.Join(dir, "state.sqlite"))
	defer store.Close()
	if err := store.SetWebhook(ctx, "topic", "https://example.invalid/secret"); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(dir, "backup.sqlite")
	if err := store.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}
	assertPrivateFile(t, backupPath)
	backup := openTestStore(t, backupPath)
	if webhook, err := backup.Webhook(ctx, "topic"); err != nil || webhook != "https://example.invalid/secret" {
		t.Fatalf("0600备份未保存设置: err=%v", err)
	}
	backup.Close()
	assertPrivateFile(t, backupPath)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	failedPath := filepath.Join(dir, "cancelled.sqlite")
	if err := store.Backup(cancelled, failedPath); err == nil {
		t.Fatal("取消操作仍备份成功")
	}
	if _, err := os.Stat(failedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("失败备份未清理: %v", err)
	}
	store.Close()
	failedPath = filepath.Join(dir, "closed.sqlite")
	if err := store.Backup(ctx, failedPath); err == nil {
		t.Fatal("关闭数据库仍备份成功")
	}
	if _, err := os.Stat(failedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("失败备份未清理: %v", err)
	}
}

func TestConcurrentBackupDoesNotOverwriteWinner(t *testing.T) {
	store := openTestStore(t, ":memory:")
	defer store.Close()
	ctx := context.Background()
	if err := store.SetWebhook(ctx, "topic", "https://example.invalid/secret"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "backup.sqlite")
	const workers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Go(func() {
			<-start
			errs <- store.Backup(ctx, dest)
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		} else if !strings.Contains(err.Error(), "already exists") {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("并发备份成功次数=%d，期望1", successes)
	}
	assertPrivateFile(t, dest)
	backup := openTestStore(t, dest)
	defer backup.Close()
	if webhook, err := backup.Webhook(ctx, "topic"); err != nil || webhook != "https://example.invalid/secret" {
		t.Fatalf("并发备份破坏了成功输出: err=%v", err)
	}
}

func TestSQLiteExistingSymlinkAndReadOnlyURI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.sqlite")
	original := openTestStore(t, path)
	defer original.Close()
	ctx := context.Background()
	if err := original.SetWebhook(ctx, "topic", "https://example.invalid/secret"); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias.sqlite")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	linked := openTestStore(t, alias)
	defer linked.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		assertPrivateFile(t, path+suffix)
	}
	readonly := openTestStore(t, (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	defer readonly.Close()
	if webhook, err := readonly.Webhook(ctx, "topic"); err != nil || webhook != "https://example.invalid/secret" {
		t.Fatalf("权限保护破坏了只读查询: err=%v", err)
	}
	if err := readonly.SetWebhook(ctx, "topic", "https://example.invalid/replacement"); err == nil {
		t.Fatal("mode=ro 仍允许写入")
	}
}

func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s 权限=%04o，期望0600", filepath.Base(path), got)
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
