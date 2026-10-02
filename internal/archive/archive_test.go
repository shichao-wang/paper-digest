package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/document"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/state"
)

type fixture struct {
	store                    *state.Store
	root, db                 string
	doc                      library.Document
	batch, metadata, history library.Artifact
}

func setup(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "live.db")
	root := filepath.Join(dir, "live-artifacts")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	artifact := func(p, b string) library.Artifact {
		path := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(b), 0600); err != nil {
			t.Fatal(err)
		}
		return library.Artifact{Path: p, SHA256: hashBytes([]byte(b)), Kind: "source-response"}
	}
	metadata := artifact("artifacts/arxiv/metadata.bin", "<feed>exact version metadata</feed>")
	batch := artifact("artifacts/arxiv/batch.bin", "<html>official announcement batch</html>")
	history := artifact("artifacts/arxiv/historical.bin", "historical output source response")
	id := library.Identity{Source: "arxiv", PaperID: "2610.00001", Version: "v1"}
	v := library.Version{Identity: id, Title: "offline paper", Abstract: "abstract", MetadataArtifacts: []library.Artifact{metadata}}
	if err = s.SaveCategoryBatch(context.Background(), library.CategoryBatch{Category: "cs.IR", Date: "2026-10-02", Completeness: "complete", Artifacts: []library.Artifact{batch}, Versions: []library.Version{v}}); err != nil {
		t.Fatal(err)
	}
	repo := document.Repository{Root: root}
	d, err := repo.SaveFixture(id, []string{strings.Repeat("Complete scientific text with recommendation results and offline reproducible evidence. ", 5)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	task, err := s.ClaimTask(context.Background(), "metadata", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteTask(context.Background(), task, library.Completion{Document: &d}, now); err != nil {
		t.Fatal(err)
	}
	// A historic output has an arbitrarily nested Artifact not present in current
	// Version metadata. This proves generic traversal of every output generation.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"history": []any{map[string]any{"deep": history}}})
	if _, err = db.Exec(`INSERT INTO library_outputs(task_id,kind,source,paper_id,version,generation,data) VALUES(?,?,?,?,?,?,?)`, task.ID, "reference", id.Source, id.PaperID, id.Version, 0, raw); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return fixture{s, root, dbPath, d, batch, metadata, history}
}
func readManifest(t *testing.T, path string) Manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(path, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target should be absent: %s err=%v", path, err)
	}
}
func TestSnapshotRestoreRetainsAllReferencesAndLoadEntrypoints(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	dir := t.TempDir()
	backup := filepath.Join(dir, "backup")
	// Repository state ahead of the DB must not leak into the backup.
	extra, err := (&document.Repository{Root: f.root}).SaveFixture(library.Identity{Source: "arxiv", PaperID: "2610.00002", Version: "v1"}, []string{strings.Repeat("uncommitted future document content ", 10)})
	if err != nil {
		t.Fatal(err)
	}
	if err = Backup(ctx, f.store, f.root, backup); err != nil {
		t.Fatal(err)
	}
	m := readManifest(t, backup)
	if len(m.Files) != 8 {
		t.Fatalf("got %d files: %+v", len(m.Files), m.Files)
	}
	for _, file := range m.Files {
		if strings.Contains(file.Path, extra.ID) {
			t.Fatal("future document leaked")
		}
	}
	// Live changes after publication cannot change archived references or contents.
	if err = os.WriteFile(filepath.Join(f.root, f.metadata.Path), []byte("new incompatible data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = f.store.UpsertVersion(ctx, library.Version{Identity: library.Identity{Source: "arxiv", PaperID: "2610.00003", Version: "v1"}, Title: "later"}); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored")
	if err = Restore(ctx, backup, restored); err != nil {
		t.Fatal(err)
	}
	repo := document.Repository{Root: filepath.Join(restored, "artifacts")}
	for _, ref := range []any{f.doc.ID, f.doc, f.doc.Identity} {
		got, err := repo.Load(ctx, ref)
		if err != nil || got.ID != f.doc.ID {
			t.Fatalf("Load(%T): %s %v", ref, got.ID, err)
		}
	}
	db, err := openSnapshot(filepath.Join(restored, "database.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err = db.QueryRow("SELECT COUNT(*) FROM library_versions").Scan(&n); err != nil || n != 1 {
		t.Fatalf("snapshot versions %d err=%v", n, err)
	}
	for _, a := range []library.Artifact{f.metadata, f.batch, f.history} {
		b, err := os.ReadFile(filepath.Join(restored, "artifacts", filepath.FromSlash(a.Path)))
		if err != nil || hashBytes(b) != a.SHA256 {
			t.Fatalf("restored source %s: %v", a.Path, err)
		}
	}
	// No Store.Open was used by archive: migration rows and task lease/status stay
	// byte-identical, and no SQLite sidecars or pre-migration backup are created.
	a, err := os.ReadFile(filepath.Join(backup, "database.db"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(restored, "database.db"))
	if err != nil {
		t.Fatal(err)
	}
	if hashBytes(a) != hashBytes(b) {
		t.Fatal("restore mutated database")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		assertAbsent(t, filepath.Join(restored, "database.db"+suffix))
	}
	if err = Restore(ctx, backup, restored); err == nil {
		t.Fatal("existing restore target accepted")
	}
	if err = Backup(ctx, f.store, f.root, backup); err == nil {
		t.Fatal("existing backup target accepted")
	}
}
func TestBackupRejectsMissingCorruptAndSymlinkDependencies(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "symlink", "manifest", "identity", "unsafe-path", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			target := filepath.Join(t.TempDir(), "backup")
			p := filepath.Join(f.root, f.metadata.Path)
			switch mode {
			case "missing":
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(p, []byte("tampered"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(f.root, f.batch.Path), p); err != nil {
					t.Fatal(err)
				}
			case "manifest":
				if err := os.Remove(filepath.Join(f.root, "documents", f.doc.ID, "manifest.json")); err != nil {
					t.Fatal(err)
				}
			case "identity":
				if err := os.WriteFile(filepath.Join(f.root, "identities", hashBytes([]byte(f.doc.Identity.Key()))+".json"), []byte(strings.Repeat("a", 64)), 0600); err != nil {
					t.Fatal(err)
				}
			case "unsafe-path", "conflict":
				a := f.metadata
				if mode == "unsafe-path" {
					a.Path = "../escape.bin"
				} else {
					a.SHA256 = strings.Repeat("a", 64)
				}
				if err := f.store.SaveCategoryBatch(context.Background(), library.CategoryBatch{Category: "cs.AI", Date: "2026-10-02", Artifacts: []library.Artifact{a}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := Backup(context.Background(), f.store, f.root, target); err == nil {
				t.Fatal("invalid dependency accepted")
			}
			assertAbsent(t, target)
			matches, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".archive-*"))
			if len(matches) != 0 {
				t.Fatalf("staging not cleaned: %v", matches)
			}
		})
	}
}
func TestRestoreRejectsTamperingMissingAndUnsafeManifest(t *testing.T) {
	for _, mode := range []string{"tamper", "missing", "symlink", "path", "database-corrupt", "foreign-key", "closure", "identity"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			dir := t.TempDir()
			backup := filepath.Join(dir, "backup")
			if err := Backup(context.Background(), f.store, f.root, backup); err != nil {
				t.Fatal(err)
			}
			m := readManifest(t, backup)
			p := filepath.Join(backup, "artifacts", f.metadata.Path)
			updateHash := func(path string) {
				for i := range m.Files {
					if m.Files[i].Path == path {
						b, err := os.ReadFile(filepath.Join(backup, filepath.FromSlash(path)))
						if err != nil {
							t.Fatal(err)
						}
						m.Files[i].SHA256 = hashBytes(b)
						m.Files[i].Size = int64(len(b))
						return
					}
				}
				t.Fatal("missing manifest record")
			}
			switch mode {
			case "tamper":
				if err := os.WriteFile(p, []byte("tampered"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(f.root, f.metadata.Path), p); err != nil {
					t.Fatal(err)
				}
			case "path":
				m.Files[0].Path = "../escape"
			case "database-corrupt":
				if err := os.WriteFile(filepath.Join(backup, "database.db"), []byte("not SQLite"), 0600); err != nil {
					t.Fatal(err)
				}
				updateHash("database.db")
			case "foreign-key":
				db, err := sql.Open("sqlite", filepath.Join(backup, "database.db"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec("PRAGMA foreign_keys=OFF; DELETE FROM library_tasks"); err != nil {
					t.Fatal(err)
				}
				db.Close()
				updateHash("database.db")
			case "closure":
				for i := range m.Files {
					if m.Files[i].Path == "artifacts/"+f.history.Path {
						m.Files = append(m.Files[:i], m.Files[i+1:]...)
						break
					}
				}
			case "identity":
				path := "artifacts/identities/" + hashBytes([]byte(f.doc.Identity.Key())) + ".json"
				if err := os.WriteFile(filepath.Join(backup, filepath.FromSlash(path)), []byte(strings.Repeat("b", 64)), 0600); err != nil {
					t.Fatal(err)
				}
				updateHash(path)
			}
			raw, _ := json.Marshal(m)
			if err := os.WriteFile(filepath.Join(backup, "manifest.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "restored")
			if err := Restore(context.Background(), backup, target); err == nil {
				t.Fatal("tampered archive accepted")
			}
			assertAbsent(t, target)
		})
	}
}
func TestBackupEmptyLibraryAndCancelledOperation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := state.Open(filepath.Join(dir, "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	root := filepath.Join(dir, "artifacts")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backup")
	if err = Backup(ctx, s, root, backup); err != nil {
		t.Fatal(err)
	}
	if err = Restore(ctx, backup, filepath.Join(dir, "restore")); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	target := filepath.Join(dir, "cancelled")
	if err = Backup(canceled, s, root, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	assertAbsent(t, target)
	if err = Restore(canceled, backup, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel restore: %v", err)
	}
	assertAbsent(t, target)
}

// Build the PDF repository contract directly from a synthetic document's complete
// page/block fixture. No HTTP, Poppler executable, or production data is used.
func TestPDFAndReferenceDocumentSnapshotClosure(t *testing.T) {
	f := setup(t)
	id := library.Identity{Source: "arxiv", PaperID: "2610.00004", Version: "v1"}
	repo := document.Repository{Root: f.root}
	d, err := repo.SaveFixture(id, []string{strings.Repeat("A complete previous-version document used as comparison evidence. ", 6)})
	if err != nil {
		t.Fatal(err)
	}
	size := 12000
	pdf := []byte("%PDF-1.7\n% offline immutable PDF bytes\n%%EOF\n")
	d.Source.SHA256 = hashBytes(pdf)
	d.Source.URL = id.PDFURL()
	d.Source.Kind = "pdf"
	d.Extractor = "poppler-pdftotext-layout-utf8-v1"
	d.Issues = []string{"warning: PDF visual fidelity unverified; text may omit images or formula symbols and may not preserve table cells or reading order"}
	d.ID = hashBytes([]byte(id.Key() + "\n" + d.Source.SHA256 + "\n" + d.Text.SHA256 + "\n" + strconv.Itoa(size)))
	d.Source.Path = "documents/" + d.ID + "/original.pdf"
	oldTextPath := d.Text.Path
	d.Text.Path = "documents/" + d.ID + "/pages.json"
	prefix := filepath.Join(f.root, "documents", d.ID)
	if err = os.Mkdir(prefix, 0700); err != nil {
		t.Fatal(err)
	}
	pages, err := os.ReadFile(filepath.Join(f.root, oldTextPath))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(struct {
		Format     int              `json:"format"`
		Document   library.Document `json:"document"`
		BlockBytes int              `json:"block_bytes"`
	}{1, d, size})
	for name, b := range map[string][]byte{"original.pdf": pdf, "pages.json": pages, "manifest.json": raw} {
		if err = os.WriteFile(filepath.Join(prefix, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(f.root, "identities", hashBytes([]byte(id.Key()))+".json"), []byte(d.ID), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Load(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.db)
	if err != nil {
		t.Fatal(err)
	}
	// A reference document may have a different Identity from the output's task;
	// archive must follow the retained output rather than the current version pointer.
	raw, _ = json.Marshal(map[string]any{"reference_documents": []library.Document{d}})
	if _, err = db.Exec(`INSERT INTO library_outputs(task_id,kind,source,paper_id,version,generation,data) SELECT id,'reference-documents',source,paper_id,version,generation,? FROM library_tasks LIMIT 1`, raw); err != nil {
		t.Fatal(err)
	}
	db.Close()
	dir := t.TempDir()
	backup := filepath.Join(dir, "backup")
	restored := filepath.Join(dir, "restore")
	if err = Backup(context.Background(), f.store, f.root, backup); err != nil {
		t.Fatal(err)
	}
	if err = Restore(context.Background(), backup, restored); err != nil {
		t.Fatal(err)
	}
	loaded, err := (&document.Repository{Root: filepath.Join(restored, "artifacts")}).Load(context.Background(), id)
	if err != nil || loaded.Source.Kind != "pdf" || loaded.ID != d.ID {
		t.Fatalf("reference PDF lost: %+v %v", loaded, err)
	}
}

func TestArchiveUndatedSourceObservation(t *testing.T) {
	f := setup(t)
	source := library.Artifact{Path: "artifacts/arxiv/undated.bin", SHA256: hashBytes([]byte("undated failed source response")), Kind: "source-response"}
	if err := os.WriteFile(filepath.Join(f.root, source.Path), []byte("undated failed source response"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveSourceObservation(context.Background(), library.CategoryBatch{Category: "cs.IR", Reason: "no trustworthy date", Artifacts: []library.Artifact{source}}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	backup := filepath.Join(dir, "backup")
	restored := filepath.Join(dir, "restore")
	err := Backup(context.Background(), f.store, f.root, backup)
	if err != nil {
		t.Fatal(err)
	}
	if err = Restore(context.Background(), backup, restored); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(restored, "artifacts", source.Path))
	if err != nil || hashBytes(raw) != source.SHA256 {
		t.Fatalf("undated source lost: %v", err)
	}
}

func TestExclusivePublicationNeverReplacesExistingEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	err := publish(context.Background(), target, func(stage string, root *os.Root) error {
		// Simulate another actor creating an otherwise empty target between the initial
		// exists check and rename. Ordinary os.Rename would overwrite it on Unix.
		if err := os.Mkdir(target, 0700); err != nil {
			return err
		}
		return writeBytes(root, "sentinel", []byte("never publish"))
	})
	if err == nil {
		t.Fatal("exclusive rename replaced concurrent target")
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatalf("target changed: %v err=%v", entries, err)
	}
}
