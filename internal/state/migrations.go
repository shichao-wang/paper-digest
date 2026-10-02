package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/papers"
)

// 创建新表之前备份旧库；database_list 同时支持普通路径和 file: URI，内存库没有文件名。
func (s *Store) backupLegacyLibrary(ctx context.Context) error {
	var legacy, migrated int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='jobs'), EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='schema_migrations')`).Scan(&legacy, &migrated); err != nil {
		return fmt.Errorf("inspect legacy schema: %w", err)
	}
	if legacy == 0 || migrated != 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA database_list`)
	if err != nil {
		return err
	}
	var filename string
	for rows.Next() {
		var seq int
		var name, path string
		if err := rows.Scan(&seq, &name, &path); err != nil {
			rows.Close()
			return err
		}
		if name == "main" {
			filename = path
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if filename == "" {
		return nil
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	dest := filename + ".pre-library-" + hex.EncodeToString(nonce[:]) + ".db"
	if err := s.Backup(ctx, dest); err != nil {
		return fmt.Errorf("backup before library migration: %w", err)
	}
	return nil
}

const librarySchema = `
CREATE TABLE library_versions (
 source TEXT NOT NULL, paper_id TEXT NOT NULL, version TEXT NOT NULL, number INTEGER NOT NULL,
 data BLOB NOT NULL, relevance_id INTEGER, relevance_generation INTEGER NOT NULL DEFAULT -1,
 document_id INTEGER, document_generation INTEGER NOT NULL DEFAULT -1,
 analysis_id INTEGER, analysis_generation INTEGER NOT NULL DEFAULT -1,
 comparison_id INTEGER, comparison_generation INTEGER NOT NULL DEFAULT -1,
 metadata_generation INTEGER NOT NULL DEFAULT -1,
 PRIMARY KEY(source,paper_id,version)
);
CREATE TABLE library_batches (category TEXT NOT NULL,date TEXT NOT NULL,data BLOB NOT NULL,PRIMARY KEY(category,date));
CREATE TABLE library_batch_versions (
 category TEXT NOT NULL,date TEXT NOT NULL,source TEXT NOT NULL,paper_id TEXT NOT NULL,version TEXT NOT NULL,
 PRIMARY KEY(category,date,source,paper_id,version),
 FOREIGN KEY(category,date) REFERENCES library_batches(category,date),
 FOREIGN KEY(source,paper_id,version) REFERENCES library_versions(source,paper_id,version)
);
CREATE TABLE library_events (
 category TEXT NOT NULL,date TEXT NOT NULL,source TEXT NOT NULL,paper_id TEXT NOT NULL,version TEXT NOT NULL,type TEXT NOT NULL,data BLOB NOT NULL,
 PRIMARY KEY(category,date,source,paper_id,version,type),
 FOREIGN KEY(category,date) REFERENCES library_batches(category,date),
 FOREIGN KEY(source,paper_id,version) REFERENCES library_versions(source,paper_id,version)
);
CREATE TABLE library_gaps (category TEXT NOT NULL,after_date TEXT NOT NULL,before_date TEXT NOT NULL,reason TEXT NOT NULL,data BLOB NOT NULL,PRIMARY KEY(category,after_date,before_date,reason));
CREATE TABLE library_tasks (
 id INTEGER PRIMARY KEY,source TEXT NOT NULL,paper_id TEXT NOT NULL,version TEXT NOT NULL,
 stage TEXT NOT NULL CHECK(stage IN ('metadata','relevance','document','analyze','compare')),
 generation INTEGER NOT NULL CHECK(generation>=0),
 status TEXT NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','running','succeeded','retry_wait','failed','paused','blocked','cancelled')),
 attempt INTEGER NOT NULL DEFAULT 0,next_attempt INTEGER NOT NULL DEFAULT 0,
 lease_token TEXT NOT NULL DEFAULT '',lease_until INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '',
 UNIQUE(source,paper_id,version,stage,generation),
 FOREIGN KEY(source,paper_id,version) REFERENCES library_versions(source,paper_id,version)
);
CREATE INDEX library_tasks_claim ON library_tasks(stage,status,next_attempt,lease_until,id);
CREATE TABLE library_checkpoints (task_id INTEGER PRIMARY KEY,data BLOB NOT NULL,FOREIGN KEY(task_id) REFERENCES library_tasks(id));
CREATE TABLE library_chunks (
 task_id INTEGER NOT NULL,document_id TEXT NOT NULL,document_hash TEXT NOT NULL,block_id TEXT NOT NULL,data BLOB NOT NULL,
 PRIMARY KEY(task_id,document_id,document_hash,block_id),FOREIGN KEY(task_id) REFERENCES library_tasks(id)
);
CREATE TABLE library_outputs (
 id INTEGER PRIMARY KEY,task_id INTEGER NOT NULL,kind TEXT NOT NULL,source TEXT NOT NULL,paper_id TEXT NOT NULL,version TEXT NOT NULL,
 generation INTEGER NOT NULL,data BLOB NOT NULL,
 UNIQUE(task_id,kind,source,paper_id,version),FOREIGN KEY(task_id) REFERENCES library_tasks(id)
);
CREATE INDEX library_outputs_version ON library_outputs(source,paper_id,version,kind,id);
CREATE TABLE library_runs (id INTEGER PRIMARY KEY,task_id INTEGER NOT NULL UNIQUE,data BLOB NOT NULL,FOREIGN KEY(task_id) REFERENCES library_tasks(id));
`

func (s *Store) migrateLibrary(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=1)`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if _, err := tx.ExecContext(ctx, librarySchema); err != nil {
			return fmt.Errorf("create library schema: %w", err)
		}
		if err := importLegacyVersions(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(1,?)`, library.Timestamp(time.Now())); err != nil {
			return err
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=2)`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE library_source_observations (id TEXT PRIMARY KEY, category TEXT NOT NULL, captured_at TEXT NOT NULL, data BLOB NOT NULL)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(2,?)`, library.Timestamp(time.Now())); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// 只从 papers.data 导入真实来源版本；旧 SQL version 列可能是内部 hash，历史摘要保留在旧表。
func importLegacyVersions(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT data FROM papers ORDER BY topic,paper_id,version`)
	if err != nil {
		return err
	}
	versions := []library.Version{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var p papers.Paper
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		vnum := strings.TrimPrefix(p.Version, "v")
		n, err := strconv.Atoi(vnum)
		if err != nil || n <= 0 {
			continue
		}
		v := library.Version{Identity: library.Identity{Source: "arxiv", PaperID: strings.TrimPrefix(p.ID, "arxiv:"), Version: fmt.Sprintf("v%d", n)}, Title: p.Title, Authors: p.Authors, Abstract: p.Abstract, Origin: "legacy"}
		if v.Identity.Validate() != nil {
			continue
		}
		if !p.Published.IsZero() {
			v.PublishedAt = library.Timestamp(p.Published)
		}
		if !p.Updated.IsZero() {
			v.UpdatedAt = library.Timestamp(p.Updated)
		}
		versions = append(versions, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range versions {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO library_versions(source,paper_id,version,number,data) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, v.Source, v.PaperID, v.Version, v.Number(), raw); err != nil {
			return err
		}
	}
	return nil
}
