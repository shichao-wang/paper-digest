package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shichao-wang/paper-digest/internal/library"
)

const taskSelect = `SELECT t.id,t.source,t.paper_id,t.version,t.stage,t.generation,t.status,t.attempt,t.next_attempt,t.lease_token,t.lease_until,t.error,c.data FROM library_tasks t LEFT JOIN library_checkpoints c ON c.task_id=t.id`

type taskScanner interface{ Scan(...any) error }

func scanTask(row taskScanner) (library.Task, error) {
	var t library.Task
	var next, until int64
	var raw []byte
	err := row.Scan(&t.ID, &t.Source, &t.PaperID, &t.Version, &t.Stage, &t.Generation, &t.Status, &t.Attempt, &next, &t.LeaseToken, &until, &t.Error, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return t, library.ErrNotFound
	}
	t.NextAttemptAt = libraryTime(next)
	t.LeaseUntil = libraryTime(until)
	t.Checkpoint = json.RawMessage(raw)
	return t, err
}
func (s *Store) GetTask(ctx context.Context, id int64) (library.Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, taskSelect+` WHERE t.id=?`, id))
}
func (s *Store) ClaimTask(ctx context.Context, stage string, now time.Time, lease time.Duration) (library.Task, error) {
	return s.ClaimTaskQuery(ctx, stage, now, lease, library.Query{}, nil)
}

func (s *Store) ClaimTaskQuery(ctx context.Context, stage string, now time.Time, lease time.Duration, query library.Query, identity *library.Identity) (library.Task, error) {
	if !validStage(stage) || lease <= 0 || now.IsZero() {
		return library.Task{}, library.ErrInvalid
	}
	if query.Relevance == "" {
		query.Relevance = "all"
	}
	where, filterArgs, err := libraryFilters(query)
	if err != nil {
		return library.Task{}, err
	}
	where += ` AND v.source=t.source AND v.paper_id=t.paper_id AND v.version=t.version`
	if identity != nil {
		id := canonicalIdentity(*identity)
		if id.Validate() != nil {
			return library.Task{}, library.ErrInvalid
		}
		where += ` AND v.source=? AND v.paper_id=? AND v.version=?`
		filterArgs = append(filterArgs, identityArgs(id)...)
	}
	args := append([]any{stage, now.UnixNano(), now.UnixNano()}, filterArgs...)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return library.Task{}, err
	}
	defer tx.Rollback()
	t, err := scanTask(tx.QueryRowContext(ctx, taskSelect+` WHERE t.stage=? AND ((t.status IN ('queued','retry_wait') AND t.next_attempt<=?) OR (t.status='running' AND t.lease_until<=?)) AND NOT EXISTS(SELECT 1 FROM library_tasks newer WHERE newer.source=t.source AND newer.paper_id=t.paper_id AND newer.version=t.version AND newer.generation>t.generation) AND EXISTS(SELECT 1`+libraryListFrom+where+`) ORDER BY t.id LIMIT 1`, args...))
	if err != nil {
		return library.Task{}, err
	}
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return library.Task{}, err
	}
	t.LeaseToken = hex.EncodeToString(token[:])
	t.LeaseUntil = library.Timestamp(now.Add(lease))
	t.Status = "running"
	t.Attempt++
	if _, err := tx.ExecContext(ctx, `UPDATE library_tasks SET status='running',attempt=attempt+1,lease_token=?,lease_until=? WHERE id=?`, t.LeaseToken, now.Add(lease).UnixNano(), t.ID); err != nil {
		return library.Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return library.Task{}, err
	}
	return t, nil
}
func verifyLease(ctx context.Context, tx *sql.Tx, t library.Task, now time.Time) error {
	if t.ID <= 0 || t.LeaseToken == "" || now.IsZero() {
		return library.ErrLease
	}
	var ok int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM library_tasks WHERE id=? AND lease_token=? AND status='running' AND lease_until>? AND source=? AND paper_id=? AND version=? AND stage=? AND generation=?)`, t.ID, t.LeaseToken, now.UnixNano(), t.Source, t.PaperID, t.Version, t.Stage, t.Generation).Scan(&ok)
	if err != nil {
		return err
	}
	if ok == 0 {
		return library.ErrLease
	}
	return nil
}
func (s *Store) RenewLease(ctx context.Context, t library.Task, now time.Time, lease time.Duration) error {
	if lease <= 0 {
		return library.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifyLease(ctx, tx, t, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE library_tasks SET lease_until=MAX(lease_until,?) WHERE id=?`, now.Add(lease).UnixNano(), t.ID); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) SaveCheckpoint(ctx context.Context, t library.Task, raw json.RawMessage, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifyLease(ctx, tx, t, now); err != nil {
		return err
	}
	if !json.Valid(raw) {
		return library.ErrInvalid
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO library_checkpoints(task_id,data) VALUES(?,?) ON CONFLICT(task_id) DO UPDATE SET data=excluded.data`, t.ID, []byte(raw)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Chunks(ctx context.Context, t library.Task) ([]library.Chunk, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM library_chunks WHERE task_id=? ORDER BY document_id,document_hash,block_id`, t.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []library.Chunk{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var c library.Chunk
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}
func (s *Store) SaveChunk(ctx context.Context, t library.Task, c library.Chunk, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifyLease(ctx, tx, t, now); err != nil {
		return err
	}
	if c.DocumentID == "" || c.DocumentHash == "" || c.BlockID == "" {
		return library.ErrInvalid
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	var old []byte
	err = tx.QueryRowContext(ctx, `SELECT data FROM library_chunks WHERE task_id=? AND document_id=? AND document_hash=? AND block_id=?`, t.ID, c.DocumentID, c.DocumentHash, c.BlockID).Scan(&old)
	if err == nil {
		if !bytes.Equal(old, raw) {
			return fmt.Errorf("chunk is immutable: %w", library.ErrInvalid)
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO library_chunks(task_id,document_id,document_hash,block_id,data) VALUES(?,?,?,?,?)`, t.ID, c.DocumentID, c.DocumentHash, c.BlockID, raw); err != nil {
		return err
	}
	return tx.Commit()
}

// 前版文档归属严格的 v(N-1)，记录当前任务为来源，不推进前版指针或队列。
func saveReferenceDocument(ctx context.Context, tx *sql.Tx, t library.Task, document library.Document) error {
	document.Identity = canonicalIdentity(document.Identity)
	previous, ok := t.Identity.Previous()
	if !ok || document.Identity != previous || document.ID == "" {
		return fmt.Errorf("reference document must be the direct previous version: %w", library.ErrInvalid)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return err
	}
	var saved []byte
	err = tx.QueryRowContext(ctx, `SELECT data FROM library_outputs WHERE task_id=? AND kind='document' AND source=? AND paper_id=? AND version=?`, t.ID, document.Identity.Source, document.PaperID, document.Version).Scan(&saved)
	if err == nil {
		if !bytes.Equal(saved, raw) {
			return fmt.Errorf("reference document is immutable: %w", library.ErrInvalid)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	ref := t
	ref.Identity = document.Identity
	_, err = insertOutput(ctx, tx, ref, "document", document)
	return err
}

// SaveReferenceDocument 在模型调用前保存对照文档，暂停及检查点恢复期间仍可完整备份。
func (s *Store) SaveReferenceDocument(ctx context.Context, t library.Task, document library.Document, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifyLease(ctx, tx, t, now); err != nil {
		return err
	}
	if err := saveReferenceDocument(ctx, tx, t, document); err != nil {
		return err
	}
	return tx.Commit()
}

func saveTaskDocument(ctx context.Context, tx *sql.Tx, t library.Task, document library.Document) error {
	document.Identity = canonicalIdentity(document.Identity)
	if document.Identity != t.Identity || document.ID == "" {
		return library.ErrInvalid
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return err
	}
	var saved []byte
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id,data FROM library_outputs WHERE task_id=? AND kind='document' AND source=? AND paper_id=? AND version=?`, t.ID, t.Source, t.PaperID, t.Version).Scan(&id, &saved)
	if err == nil {
		if !bytes.Equal(saved, raw) {
			return fmt.Errorf("document is immutable: %w", library.ErrInvalid)
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		id, err = insertOutput(ctx, tx, t, "document", document)
		if err != nil {
			return err
		}
	} else {
		return err
	}
	return updatePointer(ctx, tx, t, "document", id)
}

// SaveTaskDocument 保存已发布的任务文档及质量信息，之后可将任务置为 blocked。
func (s *Store) SaveTaskDocument(ctx context.Context, t library.Task, document library.Document, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifyLease(ctx, tx, t, now); err != nil {
		return err
	}
	if err := saveTaskDocument(ctx, tx, t, document); err != nil {
		return err
	}
	return tx.Commit()
}

func insertOutput(ctx context.Context, tx *sql.Tx, t library.Task, kind string, value any) (int64, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO library_outputs(task_id,kind,source,paper_id,version,generation,data) VALUES(?,?,?,?,?,?,?)`, t.ID, kind, t.Source, t.PaperID, t.Version, t.Generation, raw)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// 代次属于整个论文版本，而非单个阶段。新代次排队后保留已有展示结果，
// 但旧代次后续完成只能保存历史，不能发布输出或推进后继阶段。
func currentGeneration(ctx context.Context, tx *sql.Tx, t library.Task) (bool, error) {
	var current bool
	err := tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM library_tasks WHERE source=? AND paper_id=? AND version=? AND generation>?)`, t.Source, t.PaperID, t.Version, t.Generation).Scan(&current)
	return current, err
}

func updatePointer(ctx context.Context, tx *sql.Tx, t library.Task, kind string, id int64) error {
	current, err := currentGeneration(ctx, tx, t)
	if err != nil || !current {
		return err
	}
	// kind 只能来自内部固定输出类型。
	column := kind + "_id"
	generation := kind + "_generation"
	_, err = tx.ExecContext(ctx, `UPDATE library_versions SET `+column+`=?,`+generation+`=? WHERE source=? AND paper_id=? AND version=? AND `+generation+`<=?`, id, t.Generation, t.Source, t.PaperID, t.Version, t.Generation)
	return err
}
func (s *Store) CompleteTask(ctx context.Context, t library.Task, c library.Completion, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifyLease(ctx, tx, t, now); err != nil {
		return err
	}
	current, err := currentGeneration(ctx, tx, t)
	if err != nil {
		return err
	}
	if c.Version != nil {
		v := *c.Version
		v.Identity = canonicalIdentity(v.Identity)
		if v.Identity != t.Identity {
			return library.ErrInvalid
		}
		var generation int
		if err := tx.QueryRowContext(ctx, `SELECT metadata_generation FROM library_versions WHERE source=? AND paper_id=? AND version=?`, identityArgs(t.Identity)...).Scan(&generation); err != nil {
			return err
		}
		if current && t.Generation >= generation {
			if _, err := upsertVersion(ctx, tx, v); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE library_versions SET metadata_generation=? WHERE source=? AND paper_id=? AND version=?`, t.Generation, t.Source, t.PaperID, t.Version); err != nil {
				return err
			}
		}
		if _, err := insertOutput(ctx, tx, t, "metadata", v); err != nil {
			return err
		}
	}
	if c.Relevance != nil {
		if err := c.Relevance.Validate(); err != nil {
			return err
		}
		id, err := insertOutput(ctx, tx, t, "relevance", c.Relevance)
		if err != nil {
			return err
		}
		if err := updatePointer(ctx, tx, t, "relevance", id); err != nil {
			return err
		}
		if t.Stage == "relevance" && current && !c.Relevance.DirectlyRelated {
			// 筛选成功终止当前代次时撤销旧展示指针；不可变输出仍供历史查询。
			if _, err := tx.ExecContext(ctx, `UPDATE library_versions SET analysis_id=NULL,analysis_generation=MAX(analysis_generation,?),comparison_id=NULL,comparison_generation=MAX(comparison_generation,?) WHERE source=? AND paper_id=? AND version=? AND relevance_id=?`, t.Generation, t.Generation, t.Source, t.PaperID, t.Version, id); err != nil {
				return err
			}
		}
	}
	if c.Document != nil {
		if err := saveTaskDocument(ctx, tx, t, *c.Document); err != nil {
			return err
		}
	}
	for _, document := range c.ReferenceDocuments {
		if err := saveReferenceDocument(ctx, tx, t, document); err != nil {
			return err
		}
	}
	var newAnalysisID int64
	if c.Analysis != nil {
		if c.Analysis.PaperVersionID != t.Identity.Key() {
			return fmt.Errorf("analysis identity: %w", library.ErrInvalid)
		}
		id, err := insertOutput(ctx, tx, t, "analysis", c.Analysis)
		if err != nil {
			return err
		}
		newAnalysisID = id
		if err := updatePointer(ctx, tx, t, "analysis", id); err != nil {
			return err
		}
	}
	if c.Comparison != nil {
		cmp := *c.Comparison
		if cmp.AnalysisID == 0 && newAnalysisID != 0 {
			cmp.AnalysisID = newAnalysisID
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM library_outputs WHERE id=? AND kind='analysis' AND source=? AND paper_id=? AND version=? AND generation=?)`, cmp.AnalysisID, t.Source, t.PaperID, t.Version, t.Generation).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("comparison analysis identity: %w", library.ErrInvalid)
		}
		prev, ok := t.Identity.Previous()
		if ok && cmp.PreviousVersion != prev.Version {
			return library.ErrInvalid
		}
		if !ok && cmp.Status != "not_applicable" {
			return library.ErrInvalid
		}
		id, err := insertOutput(ctx, tx, t, "comparison", cmp)
		if err != nil {
			return err
		}
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(analysis_id,0) FROM library_versions WHERE source=? AND paper_id=? AND version=?`, identityArgs(t.Identity)...).Scan(&current); err != nil {
			return err
		}
		if current == cmp.AnalysisID {
			if err := updatePointer(ctx, tx, t, "comparison", id); err != nil {
				return err
			}
		}
	}
	raw, err := json.Marshal(c.Run)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO library_runs(task_id,data) VALUES(?,?)`, t.ID, raw); err != nil {
		return err
	}
	if current {
		for _, stage := range c.Next {
			if err := enqueueTask(ctx, tx, t.Identity, stage, t.Generation); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE library_tasks SET status='succeeded',lease_token='',lease_until=0,error='' WHERE id=?`, t.ID); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) FailTask(ctx context.Context, t library.Task, status, reason string, next, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifyLease(ctx, tx, t, now); err != nil {
		return err
	}
	switch status {
	case "retry_wait", "failed", "paused", "blocked", "cancelled":
	default:
		return library.ErrInvalid
	}
	ns := int64(0)
	if !next.IsZero() {
		ns = next.UnixNano()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE library_tasks SET status=?,error=?,next_attempt=?,lease_token='',lease_until=0 WHERE id=?`, status, reason, ns, t.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// 显式重试保留 generation、已读块和完整预算检查点。
func (s *Store) RetryTask(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM library_tasks WHERE id=?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return library.ErrNotFound
	}
	if err != nil {
		return err
	}
	switch status {
	case "failed", "paused", "blocked", "retry_wait", "cancelled":
	default:
		return library.ErrInvalid
	}
	if _, err := tx.ExecContext(ctx, `UPDATE library_tasks SET status='queued',next_attempt=0,lease_token='',lease_until=0,error='' WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
