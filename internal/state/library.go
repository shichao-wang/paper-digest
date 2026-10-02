package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/library"
)

func canonicalIdentity(id library.Identity) library.Identity {
	id.PaperID = strings.TrimPrefix(id.PaperID, "arxiv:")
	return id
}
func identityArgs(id library.Identity) []any { return []any{id.Source, id.PaperID, id.Version} }

type libraryReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readVersion(ctx context.Context, q libraryReader, id library.Identity) (library.Version, error) {
	id = canonicalIdentity(id)
	if err := id.Validate(); err != nil {
		return library.Version{}, err
	}
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT data FROM library_versions WHERE source=? AND paper_id=? AND version=?`, identityArgs(id)...).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return library.Version{}, library.ErrNotFound
	}
	if err != nil {
		return library.Version{}, err
	}
	var v library.Version
	err = json.Unmarshal(raw, &v)
	return v, err
}
func (s *Store) GetVersion(ctx context.Context, id library.Identity) (library.Version, error) {
	return readVersion(ctx, s.db, id)
}

// 更新同一来源版本的元信息时保留首次采集、公告来源及已有响应快照。
func mergeVersion(old, v library.Version) library.Version {
	if old.Origin != "" {
		v.Origin = old.Origin
	}
	if old.AnnouncementDate != "" {
		v.AnnouncementDate = old.AnnouncementDate
	}
	if old.CapturedAt != "" {
		v.CapturedAt = old.CapturedAt
	}
	if v.Title == "" {
		v.Title = old.Title
	}
	if v.Abstract == "" {
		v.Abstract = old.Abstract
	}
	if v.Authors == nil {
		v.Authors = old.Authors
	}
	if v.Categories == nil {
		v.Categories = old.Categories
	}
	if v.PrimaryCategory == "" {
		v.PrimaryCategory = old.PrimaryCategory
	}
	if v.PublishedAt == "" {
		v.PublishedAt = old.PublishedAt
	}
	if v.UpdatedAt == "" {
		v.UpdatedAt = old.UpdatedAt
	}
	if v.DOI == "" {
		v.DOI = old.DOI
	}
	if v.JournalRef == "" {
		v.JournalRef = old.JournalRef
	}
	if v.Comment == "" {
		v.Comment = old.Comment
	}
	if v.AuthorKeywords == nil {
		v.AuthorKeywords = old.AuthorKeywords
	}
	v.MetadataVerified = v.MetadataVerified || old.MetadataVerified
	v.MetadataArtifacts = mergeArtifacts(old.MetadataArtifacts, v.MetadataArtifacts)
	return v
}

func mergeArtifacts(old, current []library.Artifact) []library.Artifact {
	artifacts := append([]library.Artifact{}, old...)
	for _, a := range current {
		found := false
		for _, b := range artifacts {
			if a.Path == b.Path && a.SHA256 == b.SHA256 && a.URL == b.URL {
				found = true
				break
			}
		}
		if !found {
			artifacts = append(artifacts, a)
		}
	}
	return artifacts
}
func upsertVersion(ctx context.Context, tx *sql.Tx, v library.Version) (bool, error) {
	v.Identity = canonicalIdentity(v.Identity)
	if err := v.Identity.Validate(); err != nil {
		return false, err
	}
	old, err := readVersion(ctx, tx, v.Identity)
	isNew := errors.Is(err, library.ErrNotFound)
	if err != nil && !isNew {
		return false, err
	}
	if !isNew {
		v = mergeVersion(old, v)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO library_versions(source,paper_id,version,number,data) VALUES(?,?,?,?,?) ON CONFLICT(source,paper_id,version) DO UPDATE SET data=excluded.data`, v.Source, v.PaperID, v.Version, v.Number(), raw)
	return isNew, err
}
func (s *Store) UpsertVersion(ctx context.Context, v library.Version) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := upsertVersion(ctx, tx, v); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) SaveCategoryBatch(ctx context.Context, b library.CategoryBatch) error {
	if strings.TrimSpace(b.Category) == "" || !ValidDate(b.Date) {
		return library.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previousRaw []byte
	err = tx.QueryRowContext(ctx, `SELECT data FROM library_batches WHERE category=? AND date=?`, b.Category, b.Date).Scan(&previousRaw)
	if err == nil {
		var previous library.CategoryBatch
		if err := json.Unmarshal(previousRaw, &previous); err != nil {
			return err
		}
		b.Artifacts = mergeArtifacts(previous.Artifacts, b.Artifacts)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO library_batches(category,date,data) VALUES(?,?,?) ON CONFLICT(category,date) DO UPDATE SET data=excluded.data`, b.Category, b.Date, raw); err != nil {
		return err
	}
	for _, v := range b.Versions {
		v.Identity = canonicalIdentity(v.Identity)
		if v.Origin == "" {
			v.Origin = "announcement"
		}
		if v.AnnouncementDate == "" {
			v.AnnouncementDate = b.Date
		}
		if v.CapturedAt == "" {
			v.CapturedAt = b.CapturedAt
		}
		_, err := upsertVersion(ctx, tx, v)
		if err != nil {
			return fmt.Errorf("save batch version: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO library_batch_versions(category,date,source,paper_id,version) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, b.Category, b.Date, v.Source, v.PaperID, v.Version); err != nil {
			return err
		}
		// 对照回补或迁移可能已保存该版本；首次进入真实公告仍须排元信息任务，UNIQUE 保持幂等。
		if err := enqueueTask(ctx, tx, v.Identity, "metadata", 0); err != nil {
			return err
		}
	}
	for _, e := range b.Events {
		e.Identity = canonicalIdentity(e.Identity)
		if err := e.Identity.Validate(); err != nil {
			return err
		}
		if e.Category == "" {
			e.Category = b.Category
		}
		if e.Date == "" {
			e.Date = b.Date
		}
		if e.Category != b.Category || e.Date != b.Date || e.Type == "" {
			return library.ErrInvalid
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO library_events(category,date,source,paper_id,version,type,data) VALUES(?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, e.Category, e.Date, e.Source, e.PaperID, e.Version, e.Type, raw); err != nil {
			return fmt.Errorf("save batch event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO library_batch_versions(category,date,source,paper_id,version) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, b.Category, b.Date, e.Source, e.PaperID, e.Version); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveSourceObservation 保存公告日期未核实的采集快照和候选，不创建公告或日期；相同采集幂等。
func (s *Store) SaveSourceObservation(ctx context.Context, b library.CategoryBatch) error {
	if strings.TrimSpace(b.Category) == "" || b.Date != "" {
		return library.ErrInvalid
	}
	b.Completeness = "incomplete"
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(raw)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO library_source_observations(id,category,captured_at,data) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, hex.EncodeToString(hash[:]), b.Category, b.CapturedAt, raw); err != nil {
		return err
	}
	for _, v := range b.Versions {
		v.Identity = canonicalIdentity(v.Identity)
		v.Origin = "announcement_unverified"
		v.AnnouncementDate = ""
		if v.CapturedAt == "" {
			v.CapturedAt = b.CapturedAt
		}
		if _, err := upsertVersion(ctx, tx, v); err != nil {
			return err
		}
		if err := enqueueTask(ctx, tx, v.Identity, "metadata", 0); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SaveGap(ctx context.Context, g library.Gap) error {
	if g.Category == "" || !ValidDate(g.After) || !ValidDate(g.Before) || g.After >= g.Before || g.Reason == "" {
		return library.ErrInvalid
	}
	raw, err := json.Marshal(g)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO library_gaps(category,after_date,before_date,reason,data) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, g.Category, g.After, g.Before, g.Reason, raw)
	return err
}
func validStage(stage string) bool {
	switch stage {
	case "metadata", "relevance", "document", "analyze", "compare":
		return true
	}
	return false
}
func enqueueTask(ctx context.Context, tx *sql.Tx, id library.Identity, stage string, generation int) error {
	id = canonicalIdentity(id)
	if id.Validate() != nil || !validStage(stage) || generation < 0 {
		return library.ErrInvalid
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO library_tasks(source,paper_id,version,stage,generation) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, id.Source, id.PaperID, id.Version, stage, generation)
	return err
}
func (s *Store) EnqueueTask(ctx context.Context, id library.Identity, stage string, generation int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := enqueueTask(ctx, tx, id, stage, generation); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Documents(ctx context.Context, id library.Identity) ([]library.Document, error) {
	id = canonicalIdentity(id)
	if id.Validate() != nil {
		return nil, library.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM library_outputs WHERE source=? AND paper_id=? AND version=? AND kind='document' ORDER BY generation DESC,id DESC`, identityArgs(id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []library.Document{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var d library.Document
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
func (s *Store) Analysis(ctx context.Context, id library.Identity) (*library.Analysis, int64, error) {
	id = canonicalIdentity(id)
	if id.Validate() != nil {
		return nil, 0, library.ErrInvalid
	}
	var raw []byte
	var aid int64
	err := s.db.QueryRowContext(ctx, `SELECT o.id,o.data FROM library_versions v JOIN library_outputs o ON o.id=v.analysis_id WHERE v.source=? AND v.paper_id=? AND v.version=?`, identityArgs(id)...).Scan(&aid, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, library.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	var a library.Analysis
	err = json.Unmarshal(raw, &a)
	return &a, aid, err
}

// AnalysisForTask 读取任务自身代次的不可变分析，不跟随当前展示指针。
func (s *Store) AnalysisForTask(ctx context.Context, t library.Task) (*library.Analysis, int64, error) {
	id := canonicalIdentity(t.Identity)
	if id.Validate() != nil || t.Generation < 0 {
		return nil, 0, library.ErrInvalid
	}
	var raw []byte
	var aid int64
	err := s.db.QueryRowContext(ctx, `SELECT id,data FROM library_outputs WHERE source=? AND paper_id=? AND version=? AND kind='analysis' AND generation=? ORDER BY id DESC LIMIT 1`, id.Source, id.PaperID, id.Version, t.Generation).Scan(&aid, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, library.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	var a library.Analysis
	err = json.Unmarshal(raw, &a)
	return &a, aid, err
}

// DocumentsForGeneration 只复用该版本自身任务在目标代次或更早发布的文档。
// 其他版本比较任务保存的前版文档不参与当前版本的输入选择。
func (s *Store) DocumentsForGeneration(ctx context.Context, id library.Identity, generation int) ([]library.Document, error) {
	id = canonicalIdentity(id)
	if id.Validate() != nil || generation < 0 {
		return nil, library.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT o.data FROM library_outputs o JOIN library_tasks t ON t.id=o.task_id WHERE o.source=? AND o.paper_id=? AND o.version=? AND o.kind='document' AND o.generation<=? AND t.source=o.source AND t.paper_id=o.paper_id AND t.version=o.version ORDER BY o.generation DESC,o.id DESC`, id.Source, id.PaperID, id.Version, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []library.Document{}
	for rows.Next() {
		var raw []byte
		var doc library.Document
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		result = append(result, doc)
	}
	return result, rows.Err()
}

// TaskDocument 读取模型调用前已绑定到任务的当前或前版文档。
func (s *Store) TaskDocument(ctx context.Context, t library.Task, id library.Identity) (library.Document, error) {
	id = canonicalIdentity(id)
	if id.Validate() != nil {
		return library.Document{}, library.ErrInvalid
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM library_outputs WHERE task_id=? AND kind='document' AND source=? AND paper_id=? AND version=?`, t.ID, id.Source, id.PaperID, id.Version).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return library.Document{}, library.ErrNotFound
	}
	if err != nil {
		return library.Document{}, err
	}
	var doc library.Document
	err = json.Unmarshal(raw, &doc)
	return doc, err
}

func libraryTime(ns int64) string {
	if ns == 0 {
		return ""
	}
	return library.Timestamp(time.Unix(0, ns))
}
