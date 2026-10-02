package state

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func legacySnapshotFixture(t *testing.T, path, marker string) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;
	 CREATE TABLE jobs(topic TEXT NOT NULL,date TEXT NOT NULL,status TEXT NOT NULL,message TEXT NOT NULL,PRIMARY KEY(topic,date));
	 CREATE TABLE papers(topic TEXT NOT NULL,paper_id TEXT NOT NULL,version TEXT NOT NULL,data BLOB NOT NULL,PRIMARY KEY(topic,paper_id,version));
	 CREATE TABLE legacy_extra(value TEXT COLLATE NOCASE, payload BLOB);
	 CREATE TABLE library_versions(marker TEXT);`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO jobs VALUES('legacy','2026-09-28','sent',?);
	 INSERT INTO legacy_extra VALUES('A',x'010203'),('A',x'010203');`, marker); err != nil {
		db.Close()
		t.Fatal(err)
	}
	s := &Store{db: db, path: path}
	t.Cleanup(func() { db.Close() })
	return s
}

func snapshotBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func requireSnapshotUnchanged(t *testing.T, path string, original []byte) {
	t.Helper()
	if got := snapshotBytes(t, path); string(got) != string(original) {
		t.Fatal("existing snapshot was modified")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot permissions: info=%v err=%v", info, err)
	}
}

func TestLibraryMigrationFailureReusesStableSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s := legacySnapshotFixture(t, path, "preserved history")
	// The preexisting conflicting library table makes migration fail transactionally.
	// Missing legacy tables are filled in before this failure, and must not cause
	// each subsequent startup to discard the original pre-initialization snapshot.
	dest := path + ".pre-library-v1.db"
	var original []byte
	for attempt := 0; attempt < 4; attempt++ {
		store, err := Open(path)
		if store != nil {
			store.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "create library schema") {
			t.Fatalf("attempt %d: expected migration failure, got %v", attempt, err)
		}
		if attempt == 0 {
			original = snapshotBytes(t, dest)
		}
		requireSnapshotUnchanged(t, dest, original)
		matches, err := filepath.Glob(path + ".pre-library-*.db")
		if err != nil || len(matches) != 1 {
			t.Fatalf("attempt %d snapshots=%v err=%v", attempt, matches, err)
		}
	}
	var migrated int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='schema_migrations'`).Scan(&migrated); err != nil || migrated != 0 {
		t.Fatalf("migration failure left schema_migrations: %d %v", migrated, err)
	}
	backup, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dest, RawQuery: "mode=ro&immutable=1"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var message string
	if err := backup.QueryRow(`SELECT message FROM jobs`).Scan(&message); err != nil || message != "preserved history" {
		t.Fatalf("WAL history was not backed up: %q %v", message, err)
	}
	if err := backup.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('schema_migrations','topic_webhooks','recommendations')`).Scan(&migrated); err != nil || migrated != 0 {
		t.Fatalf("snapshot was created after initialization: %d %v", migrated, err)
	}
}

func TestLibraryMigrationRejectsInvalidStableSnapshot(t *testing.T) {
	for _, kind := range []string{"empty", "corrupt", "unrelated", "changed schema", "wrong known index", "wrong known table", "changed data", "changed duplicate count", "case changed", "migrated", "side file", "symlink", "public permissions"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "legacy.db")
			s := legacySnapshotFixture(t, path, "current history")
			dest := path + ".pre-library-v1.db"
			switch kind {
			case "empty", "corrupt":
				data := []byte(nil)
				if kind == "corrupt" {
					data = []byte("not a sqlite snapshot")
				}
				if err := os.WriteFile(dest, data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "unrelated":
				other := legacySnapshotFixture(t, filepath.Join(dir, "other.db"), "other history")
				if err := other.Backup(context.Background(), dest); err != nil {
					t.Fatal(err)
				}
			default:
				if err := s.Backup(context.Background(), dest); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "changed schema":
					_, err := s.db.Exec(`CREATE TABLE another_legacy_table(value TEXT)`)
					if err != nil {
						t.Fatal(err)
					}
				case "wrong known index", "wrong known table":
					query := `CREATE INDEX jobs_pending ON jobs(message)`
					if kind == "wrong known table" {
						query = `CREATE TABLE recommendations(topic TEXT, paper_id TEXT, date TEXT, version TEXT, extra TEXT, PRIMARY KEY(topic,paper_id), UNIQUE(extra))`
					}
					if _, err := s.db.Exec(query); err != nil {
						t.Fatal(err)
					}
				case "changed data", "changed duplicate count", "case changed", "migrated":
					query := map[string]string{
						"changed data":            `UPDATE jobs SET message='new history'`,
						"changed duplicate count": `DELETE FROM legacy_extra WHERE rowid=1`,
						"case changed":            `UPDATE legacy_extra SET value='a'`,
						"migrated":                `CREATE TABLE schema_migrations(version INTEGER)`,
					}[kind]
					if kind == "migrated" {
						backup, err := sql.Open("sqlite", dest)
						if err != nil {
							t.Fatal(err)
						}
						_, err = backup.Exec(query)
						backup.Close()
						if err != nil {
							t.Fatal(err)
						}
					} else if _, err := s.db.Exec(query); err != nil {
						t.Fatal(err)
					}
				case "side file":
					if err := os.WriteFile(dest+"-wal", []byte("pending WAL"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					target := filepath.Join(dir, "preserved.db")
					if err := os.Rename(dest, target); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, dest); err != nil {
						t.Fatal(err)
					}
				case "public permissions":
					if err := os.Chmod(dest, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			original := snapshotBytes(t, dest)
			for attempt := 0; attempt < 2; attempt++ {
				if err := s.backupLegacyLibrary(context.Background()); err == nil {
					t.Fatal("invalid or unrelated snapshot accepted")
				}
				if got := snapshotBytes(t, dest); string(got) != string(original) {
					t.Fatal("invalid snapshot overwritten")
				}
				matches, err := filepath.Glob(path + ".pre-library-*.db")
				if err != nil || len(matches) != 1 {
					t.Fatalf("invalid snapshot created more backups: %v %v", matches, err)
				}
			}
		})
	}
}

func TestLibraryMigrationSnapshotConcurrentCreationAndOldBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s := legacySnapshotFixture(t, path, "history")
	oldDest := path + ".pre-library-old-random.db"
	if err := s.Backup(context.Background(), oldDest); err != nil {
		t.Fatal(err)
	}
	oldBytes := snapshotBytes(t, oldDest)
	const starts = 8
	var wg sync.WaitGroup
	errs := make(chan error, starts)
	gate := make(chan struct{})
	for i := 0; i < starts; i++ {
		db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer db.Close()
			<-gate
			errs <- (&Store{db: db, path: path}).backupLegacyLibrary(context.Background())
		}()
	}
	close(gate)
	wg.Wait()
	close(errs)
	succeeded := 0
	for err := range errs {
		if err == nil {
			succeeded++
		}
		// Observing an in-progress exclusive destination may fail validation;
		// failing closed is safe, and a later retry must reuse the winner.
	}
	if succeeded == 0 {
		t.Fatal("no concurrent startup completed its snapshot")
	}
	if err := s.backupLegacyLibrary(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireSnapshotUnchanged(t, oldDest, oldBytes)
	matches, err := filepath.Glob(path + ".pre-library-*.db")
	if err != nil || len(matches) != 2 {
		t.Fatalf("concurrent startups produced snapshots=%v err=%v", matches, err)
	}
}

func TestLibraryMigrationStableSnapshotWithFileURI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy # backup?.db")
	s := legacySnapshotFixture(t, path, "URI history")
	if _, err := s.db.Exec(`DROP TABLE library_versions`); err != nil {
		t.Fatal(err)
	}
	uri := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw"}).String()
	for attempt := 0; attempt < 2; attempt++ {
		store, err := Open(uri)
		if err != nil {
			t.Fatal(err)
		}
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil || count != 2 {
			store.Close()
			t.Fatal(fmt.Sprintf("migration count=%d err=%v", count, err))
		}
		store.Close()
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*.pre-library-*.db"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("URI backups=%v err=%v", matches, err)
	}
}
