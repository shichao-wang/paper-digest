package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
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
	// 固定目标由 Backup 独占创建。失败重启复用完整且内容相符的快照，
	// 不按次数生成新文件，也不覆盖已有快照（包括旧的随机名称备份）。
	dest := filename + ".pre-library-v1.db"
	if _, err := os.Lstat(dest); errors.Is(err, os.ErrNotExist) {
		if err := s.Backup(ctx, dest); err != nil {
			// 另一个启动者可能已独占创建目标；仅在其完整且匹配时复用。
			if _, statErr := os.Lstat(dest); statErr != nil {
				return fmt.Errorf("backup before library migration: %w", err)
			}
		}
	} else if err != nil {
		return fmt.Errorf("inspect pre-library backup: %w", err)
	}
	if err := s.validateLegacySnapshot(ctx, dest); err != nil {
		return fmt.Errorf("validate pre-library backup %s (preserved; restore or move it before retrying): %w", dest, err)
	}
	return nil
}

func compactLegacySchema(definition string) string {
	var result strings.Builder
	var quote rune
	for _, char := range definition {
		if quote != 0 {
			result.WriteRune(char)
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"', '`':
			quote = char
		case ' ', '\t', '\n', '\r':
			continue
		}
		result.WriteRune(char)
	}
	return result.String()
}

func sqliteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// 内容比较在同一只读事务内执行；immutable 快照不生成侧文件，必须是
// VACUUM INTO 产出的独立数据库。不能仅以文件存在或 jobs 表存在认定有效。
func (s *Store) validateLegacySnapshot(ctx context.Context, dest string) error {
	info, err := os.Lstat(dest)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("snapshot must be a regular 0600 file")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(dest + suffix); !errors.Is(err, os.ErrNotExist) {
			return errors.New("snapshot must be a standalone database without side files")
		}
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	u := url.URL{Scheme: "file", Path: dest, RawQuery: "mode=ro&immutable=1"}
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS pre_library_snapshot`, u.String()); err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanupCtx, `DETACH DATABASE pre_library_snapshot`); err != nil {
			// 不将仍附着快照的连接放回池；Raw 返回 ErrBadConn 让
			// database/sql 丢弃它，后续重试获得干净连接。
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var integrity string
	if err := tx.QueryRowContext(ctx, `PRAGMA pre_library_snapshot.integrity_check`).Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return errors.New("snapshot failed SQLite integrity check")
	}
	var legacy, migrated, mismatch int
	if err := tx.QueryRowContext(ctx, `SELECT
	 EXISTS(SELECT 1 FROM pre_library_snapshot.sqlite_master WHERE type='table' AND name='jobs'),
	 EXISTS(SELECT 1 FROM pre_library_snapshot.sqlite_master WHERE name='schema_migrations'),
	 EXISTS(SELECT type,name,tbl_name,sql FROM pre_library_snapshot.sqlite_master
	 EXCEPT SELECT type,name,tbl_name,sql FROM main.sqlite_master)`).Scan(&legacy, &migrated, &mismatch); err != nil {
		return err
	}
	if legacy == 0 || migrated != 0 || mismatch != 0 {
		return errors.New("snapshot does not match the pre-library schema")
	}
	rows, err := tx.QueryContext(ctx, `SELECT type,name,tbl_name,sql FROM main.sqlite_master
	 WHERE name NOT IN (SELECT name FROM pre_library_snapshot.sqlite_master)`)
	if err != nil {
		return err
	}
	type schemaObject struct {
		kind, name, table string
		definition        sql.NullString
	}
	var added []schemaObject
	for rows.Next() {
		var object schemaObject
		if err := rows.Scan(&object.kind, &object.name, &object.table, &object.definition); err != nil {
			rows.Close()
			return err
		}
		added = append(added, object)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// initialize 在迁移事务之前可能补齐旧表及索引；空表不含需要备份的
	// 新数据。只允许这些已知新增对象，其他差异要求人工核实。
	allowedTables := map[string]string{
		"topic_webhooks":  `CREATE TABLE topic_webhooks (topic TEXT PRIMARY KEY NOT NULL, webhook_url TEXT NOT NULL)`,
		"papers":          `CREATE TABLE papers (topic TEXT NOT NULL, paper_id TEXT NOT NULL, version TEXT NOT NULL, data BLOB NOT NULL, PRIMARY KEY (topic, paper_id, version))`,
		"job_papers":      `CREATE TABLE job_papers (topic TEXT NOT NULL, date TEXT NOT NULL, paper_id TEXT NOT NULL, version TEXT NOT NULL, position INTEGER NOT NULL, summary BLOB, PRIMARY KEY (topic, date, paper_id), FOREIGN KEY (topic, date) REFERENCES jobs(topic, date) ON DELETE CASCADE, FOREIGN KEY (topic, paper_id, version) REFERENCES papers(topic, paper_id, version))`,
		"recommendations": `CREATE TABLE recommendations (topic TEXT NOT NULL, paper_id TEXT NOT NULL, date TEXT NOT NULL, version TEXT NOT NULL, PRIMARY KEY (topic, paper_id))`,
	}
	allowedIndexes := map[string]string{
		"job_papers_pending": `CREATE INDEX job_papers_pending ON job_papers(topic, date, position) WHERE summary IS NULL`,
		"jobs_pending":       `CREATE INDEX jobs_pending ON jobs(date, topic) WHERE status IN ('new', 'processing', 'ready')`,
	}
	for _, object := range added {
		switch object.kind {
		case "table":
			if expected, ok := allowedTables[object.name]; ok && object.table == object.name && compactLegacySchema(object.definition.String) == compactLegacySchema(expected) {
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.`+sqliteIdentifier(object.name)+`)`).Scan(&mismatch); err != nil {
					return err
				}
				if mismatch == 0 {
					continue
				}
			}
		case "index":
			if expected, ok := allowedIndexes[object.name]; ok && compactLegacySchema(object.definition.String) == compactLegacySchema(expected) {
				continue
			}
			if _, ok := allowedTables[object.table]; ok && object.name == "sqlite_autoindex_"+object.table+"_1" && !object.definition.Valid {
				// 自动索引仅允许与本次补齐的、定义完全一致的旧表一起出现。
				allowed := false
				for _, table := range added {
					if table.kind == "table" && table.name == object.table && compactLegacySchema(table.definition.String) == compactLegacySchema(allowedTables[object.table]) {
						allowed = true
						break
					}
				}
				if allowed {
					continue
				}
			}
		}
		return fmt.Errorf("snapshot is missing current schema or data: %s", object.name)
	}
	rows, err = tx.QueryContext(ctx, `SELECT name FROM pre_library_snapshot.sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, table := range tables {
		// table_xinfo 包括生成列；比较全部列及重复行计数，覆盖任意旧表。
		rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_xinfo(?, 'pre_library_snapshot') WHERE hidden != 1 ORDER BY cid`, table)
		if err != nil {
			return err
		}
		var columns []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			column := sqliteIdentifier(name)
			// 编码值保留类型及字节，避免 NOCASE 等列排序规则把不同内容
			// 当成相等；浮点数采用 SQLite 可往返的 26 位格式。
			columns = append(columns, `typeof(`+column+`)`, `CASE WHEN typeof(`+column+`)='real' THEN printf('%!.26g',`+column+`) ELSE hex(CAST(`+column+` AS BLOB)) END`)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(columns) == 0 {
			return fmt.Errorf("snapshot table has no comparable columns: %s", table)
		}
		group := strings.Join(columns, ",")
		current := `SELECT COUNT(*),` + group + ` FROM main.` + sqliteIdentifier(table) + ` GROUP BY ` + group
		snapshot := `SELECT COUNT(*),` + group + ` FROM pre_library_snapshot.` + sqliteIdentifier(table) + ` GROUP BY ` + group
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(`+current+` EXCEPT `+snapshot+`), EXISTS(`+snapshot+` EXCEPT `+current+`)`).Scan(&legacy, &mismatch); err != nil {
			return err
		}
		if legacy != 0 || mismatch != 0 {
			return fmt.Errorf("snapshot data does not match current table: %s", table)
		}
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
