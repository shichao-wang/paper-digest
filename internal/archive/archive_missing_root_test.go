package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/document"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/internal/state"
)

func TestBackupMissingArtifactRootWithoutDependencies(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "empty"
		if legacy {
			name = "migrated-legacy"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "live.db")
			if legacy {
				db, err := sql.Open("sqlite", dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec(`CREATE TABLE jobs(topic TEXT NOT NULL,date TEXT NOT NULL,status TEXT NOT NULL,message TEXT NOT NULL,PRIMARY KEY(topic,date));
CREATE TABLE papers(topic TEXT NOT NULL,paper_id TEXT NOT NULL,version TEXT NOT NULL,data BLOB NOT NULL,PRIMARY KEY(topic,paper_id,version));
INSERT INTO jobs VALUES('legacy','2026-09-28','sent','retained history')`); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(papers.Paper{ID: "arxiv:2610.00001", Version: "1", Title: "legacy paper"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO papers VALUES(?,?,?,?)`, "legacy", "arxiv:2610.00001", "old-hash", raw); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			s, err := state.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			root := filepath.Join(dir, "never-created", "artifacts")
			backup := filepath.Join(dir, "backup")
			if err := Backup(ctx, s, root, backup); err != nil {
				t.Fatal(err)
			}
			// The live artifact directory remains absent; the archive owns its empty root.
			assertAbsent(t, root)
			m := readManifest(t, backup)
			if len(m.Files) != 1 || m.Files[0].Path != "database.db" {
				t.Fatalf("unexpected empty dependency closure: %+v", m.Files)
			}
			restored := filepath.Join(dir, "restored")
			if err := Restore(ctx, backup, restored); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(filepath.Join(restored, "artifacts"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("restored artifact root: entries=%v err=%v", entries, err)
			}
			db, err := openSnapshot(filepath.Join(restored, "database.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM library_versions`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if legacy {
				want = 1
				var status, message string
				if err := db.QueryRow(`SELECT status,message FROM jobs WHERE topic='legacy'`).Scan(&status, &message); err != nil || status != "sent" || message != "retained history" {
					t.Fatalf("legacy history: status=%s message=%s err=%v", status, message, err)
				}
				var raw []byte
				if err := db.QueryRow(`SELECT data FROM library_versions`).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var v library.Version
				if err := json.Unmarshal(raw, &v); err != nil || v.Origin != "legacy" || v.Title != "legacy paper" {
					t.Fatalf("legacy version: %+v err=%v", v, err)
				}
			}
			if count != want {
				t.Fatalf("restored version count=%d want=%d", count, want)
			}
		})
	}
}

func TestBackupMissingArtifactRootWithDependenciesFails(t *testing.T) {
	for _, dependency := range []string{"source-response", "document"} {
		t.Run(dependency, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			s, err := state.Open(filepath.Join(dir, "live.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			root := filepath.Join(dir, "artifacts")
			id := library.Identity{Source: "arxiv", PaperID: "2610.00001", Version: "v1"}
			if dependency == "source-response" {
				err = s.SaveSourceObservation(ctx, library.CategoryBatch{Category: "cs.IR", Artifacts: []library.Artifact{{Path: "source.bin", SHA256: strings.Repeat("a", 64)}}})
			} else {
				if err := s.SaveCategoryBatch(ctx, library.CategoryBatch{Category: "cs.IR", Date: "2026-10-02", Versions: []library.Version{{Identity: id}}}); err != nil {
					t.Fatal(err)
				}
				d, err := (&document.Repository{Root: root}).SaveFixture(id, []string{strings.Repeat("Complete offline document evidence. ", 20)})
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				task, err := s.ClaimTask(ctx, "metadata", now, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.CompleteTask(ctx, task, library.Completion{Document: &d}, now); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "backup")
			if err := Backup(ctx, s, root, target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing referenced root accepted or wrong error: %v", err)
			}
			assertAbsent(t, target)
			matches, err := filepath.Glob(filepath.Join(dir, ".archive-*"))
			if err != nil || len(matches) != 0 {
				t.Fatalf("failed backup retained staging: %v err=%v", matches, err)
			}
		})
	}
}

func TestObservationContentDeduplicationPreservesBackupDependencies(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := state.Open(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	root := filepath.Join(dir, "artifacts")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	for i, content := range []string{"first response", "first response", "changed response"} {
		path := []string{"first.bin", "repeat.bin", "changed.bin"}[i]
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		capturedAt := library.Timestamp(time.Date(2026, 10, 2, 8+i, 0, 0, 0, time.UTC))
		if err := s.SaveSourceObservation(ctx, library.CategoryBatch{Category: "cs.IR", CapturedAt: capturedAt, Artifacts: []library.Artifact{{Path: path, SHA256: hashBytes([]byte(content)), CapturedAt: capturedAt, Kind: "source-response"}}}); err != nil {
			t.Fatal(err)
		}
	}
	backup := filepath.Join(dir, "backup")
	if err := Backup(ctx, s, root, backup); err != nil {
		t.Fatal(err)
	}
	m := readManifest(t, backup)
	if len(m.Files) != 3 {
		t.Fatalf("expected database and two retained responses: %+v", m.Files)
	}
	restored := filepath.Join(dir, "restored")
	if err := Restore(ctx, backup, restored); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{"first.bin": "first response", "changed.bin": "changed response"} {
		raw, err := os.ReadFile(filepath.Join(restored, "artifacts", path))
		if err != nil || string(raw) != content {
			t.Fatalf("retained response %s: %s err=%v", path, raw, err)
		}
	}
	assertAbsent(t, filepath.Join(restored, "artifacts", "repeat.bin"))
}
