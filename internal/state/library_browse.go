package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/shichao-wang/paper-digest/internal/library"
)

const libraryListFrom = ` FROM library_versions v LEFT JOIN library_outputs r ON r.id=v.relevance_id LEFT JOIN library_outputs a ON a.id=v.analysis_id`

func libraryFilters(query library.Query) (string, []any, error) {
	where := ` WHERE 1=1`
	args := []any{}
	rel := query.Relevance
	if rel == "" {
		rel = "direct"
	}
	switch rel {
	case "all":
	case "pending":
		where += ` AND r.id IS NULL`
	case "direct", "unrelated", "uncertain":
		where += ` AND json_extract(r.data,'$.relevance_level')=?`
		args = append(args, rel)
	default:
		return "", nil, library.ErrInvalid
	}
	if query.Topic != "" {
		switch query.Topic {
		case "recommendation", "advertising", "search":
		default:
			return "", nil, library.ErrInvalid
		}
		where += ` AND EXISTS(SELECT 1 FROM json_each(json_extract(r.data,'$.topics')) WHERE value=?)`
		args = append(args, query.Topic)
	}
	if query.Batch != "" {
		category, date, hasCategory := strings.Cut(query.Batch, "/")
		if !hasCategory {
			date = category
		}
		if !ValidDate(date) || (hasCategory && category == "") {
			return "", nil, library.ErrInvalid
		}
		where += ` AND EXISTS(SELECT 1 FROM library_batch_versions b WHERE b.source=v.source AND b.paper_id=v.paper_id AND b.version=v.version AND b.date=?`
		args = append(args, date)
		if hasCategory {
			where += ` AND b.category=?`
			args = append(args, category)
		}
		where += `)`
	}
	if query.Status != "" {
		stage, status, hasStage := strings.Cut(query.Status, ":")
		if !hasStage {
			status = stage
		}
		switch status {
		case "queued", "running", "succeeded", "retry_wait", "failed", "paused", "blocked", "cancelled":
		default:
			return "", nil, library.ErrInvalid
		}
		if hasStage && !validStage(stage) {
			return "", nil, library.ErrInvalid
		}
		where += ` AND EXISTS(SELECT 1 FROM library_tasks t WHERE t.source=v.source AND t.paper_id=v.paper_id AND t.version=v.version AND t.status=? AND NOT EXISTS(SELECT 1 FROM library_tasks newer WHERE newer.source=t.source AND newer.paper_id=t.paper_id AND newer.version=t.version AND newer.stage=t.stage AND newer.generation>t.generation)`
		args = append(args, status)
		if hasStage {
			where += ` AND t.stage=?`
			args = append(args, stage)
		}
		where += `)`
	}
	if query.Q != "" {
		pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(query.Q) + "%"
		where += ` AND (v.paper_id LIKE ? ESCAPE '\' OR json_extract(v.data,'$.title') LIKE ? ESCAPE '\' OR json_extract(v.data,'$.abstract') LIKE ? ESCAPE '\' OR EXISTS(SELECT 1 FROM json_each(json_extract(v.data,'$.authors')) WHERE value LIKE ? ESCAPE '\') OR json_extract(a.data,'$.content.title_zh') LIKE ? ESCAPE '\' OR json_extract(a.data,'$.content.summary_zh.text') LIKE ? ESCAPE '\' OR EXISTS(SELECT 1 FROM json_each(json_extract(v.data,'$.author_keywords')) WHERE value LIKE ? ESCAPE '\') OR EXISTS(SELECT 1 FROM json_each(json_extract(r.data,'$.extracted_keywords')) WHERE value LIKE ? ESCAPE '\'))`
		for i := 0; i < 8; i++ {
			args = append(args, pattern)
		}
	}
	return where, args, nil
}
func listTasks(ctx context.Context, q libraryReader, id library.Identity) ([]library.Task, error) {
	rows, err := q.QueryContext(ctx, taskSelect+` WHERE t.source=? AND t.paper_id=? AND t.version=? ORDER BY t.generation DESC,t.stage,t.id`, identityArgs(id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []library.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}
func (s *Store) BrowseLibrary(ctx context.Context, query library.Query) (library.ResultPage, error) {
	page, err := (PageQuery{Page: query.Page, PageSize: query.PageSize}).normalized()
	if err != nil {
		return library.ResultPage{}, library.ErrInvalid
	}
	where, args, err := libraryFilters(query)
	if err != nil {
		return library.ResultPage{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return library.ResultPage{}, err
	}
	defer tx.Rollback()
	result := library.ResultPage{Items: []library.ListItem{}, Page: page.Page, PageSize: page.PageSize}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)`+libraryListFrom+where, args...).Scan(&result.Total); err != nil {
		return result, err
	}
	rowArgs := append(append([]any{}, args...), page.PageSize, (page.Page-1)*page.PageSize)
	rows, err := tx.QueryContext(ctx, `SELECT v.data,r.data,COALESCE(v.analysis_id,0)`+libraryListFrom+where+` ORDER BY COALESCE(json_extract(v.data,'$.announcement_date'),'') DESC,COALESCE(json_extract(v.data,'$.updated_at'),'') DESC,v.paper_id,v.number DESC LIMIT ? OFFSET ?`, rowArgs...)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var item library.ListItem
		var v, r []byte
		if err := rows.Scan(&v, &r, &item.AnalysisID); err != nil {
			rows.Close()
			return result, err
		}
		if err := json.Unmarshal(v, &item.Version); err != nil {
			rows.Close()
			return result, err
		}
		if r != nil {
			item.Relevance = &library.Relevance{}
			if err := json.Unmarshal(r, item.Relevance); err != nil {
				rows.Close()
				return result, err
			}
		}
		result.Items = append(result.Items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for i := range result.Items {
		tasks, err := listTasks(ctx, tx, result.Items[i].Version.Identity)
		if err != nil {
			return result, err
		}
		result.Items[i].Tasks = tasks
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}
func (s *Store) LibraryDetail(ctx context.Context, id library.Identity) (library.Detail, error) {
	id = canonicalIdentity(id)
	if id.Validate() != nil {
		return library.Detail{}, library.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return library.Detail{}, err
	}
	defer tx.Rollback()
	result := library.Detail{Versions: []library.Identity{}, RelevanceHistory: []library.Relevance{}, Documents: []library.Document{}, Runs: []library.Run{}}
	result.Version, err = readVersion(ctx, tx, id)
	if err != nil {
		return result, err
	}
	result.Tasks, err = listTasks(ctx, tx, id)
	if err != nil {
		return result, err
	}
	var rid, cid int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(relevance_id,0),COALESCE(analysis_id,0),COALESCE(comparison_id,0) FROM library_versions WHERE source=? AND paper_id=? AND version=?`, identityArgs(id)...).Scan(&rid, &result.AnalysisID, &cid); err != nil {
		return result, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT version FROM library_versions WHERE source=? AND paper_id=? ORDER BY number`, id.Source, id.PaperID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		other := id
		if err := rows.Scan(&other.Version); err != nil {
			rows.Close()
			return result, err
		}
		result.Versions = append(result.Versions, other)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,kind,data FROM library_outputs WHERE source=? AND paper_id=? AND version=? ORDER BY generation,id`, identityArgs(id)...)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var oid int64
		var kind string
		var raw []byte
		if err := rows.Scan(&oid, &kind, &raw); err != nil {
			rows.Close()
			return result, err
		}
		switch kind {
		case "relevance":
			var r library.Relevance
			if err := json.Unmarshal(raw, &r); err != nil {
				rows.Close()
				return result, err
			}
			result.RelevanceHistory = append(result.RelevanceHistory, r)
			if oid == rid {
				result.Relevance = &r
			}
		case "document":
			var d library.Document
			if err := json.Unmarshal(raw, &d); err != nil {
				rows.Close()
				return result, err
			}
			result.Documents = append(result.Documents, d)
		case "analysis":
			if oid == result.AnalysisID {
				a := &library.Analysis{}
				if err := json.Unmarshal(raw, a); err != nil {
					rows.Close()
					return result, err
				}
				result.Analysis = a
			}
		case "comparison":
			var cmp library.Comparison
			if err := json.Unmarshal(raw, &cmp); err != nil {
				rows.Close()
				return result, err
			}
			if oid == cid && cmp.AnalysisID == result.AnalysisID {
				result.Comparison = &cmp
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	if result.Comparison == nil {
		status := "not_applicable"
		previous := ""
		if prev, ok := id.Previous(); ok {
			status = "pending"
			previous = prev.Version
		}
		result.Comparison = &library.Comparison{AnalysisID: result.AnalysisID, PreviousVersion: previous, Status: status, DocumentHashes: []string{}, Content: library.ComparisonContent{Changes: []library.Change{}, Evidence: []library.Evidence{}}}
		if status == "pending" {
			var generation int
			if err := tx.QueryRowContext(ctx, `SELECT analysis_generation FROM library_versions WHERE source=? AND paper_id=? AND version=?`, identityArgs(id)...).Scan(&generation); err != nil {
				return result, err
			}
			for _, task := range result.Tasks {
				if task.Stage != "compare" || task.Generation != generation {
					continue
				}
				switch task.Status {
				case "blocked", "failed", "cancelled":
					result.Comparison.Status = "blocked"
				case "paused":
					result.Comparison.Status = "paused"
				}
				result.Comparison.Reason = task.Error
				break
			}
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT r.data FROM library_runs r JOIN library_tasks t ON t.id=r.task_id WHERE t.source=? AND t.paper_id=? AND t.version=? ORDER BY r.id`, identityArgs(id)...)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var raw []byte
		var run library.Run
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return result, err
		}
		if err := json.Unmarshal(raw, &run); err != nil {
			rows.Close()
			return result, err
		}
		result.Runs = append(result.Runs, run)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}
func (s *Store) LibraryStatus(ctx context.Context) (library.Status, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return library.Status{}, err
	}
	defer tx.Rollback()
	result := library.Status{Tasks: map[string]int{}, Batches: []library.CategoryBatch{}, Gaps: []library.Gap{}}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM library_versions`).Scan(&result.Versions); err != nil {
		return result, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT status,COUNT(*) FROM library_tasks GROUP BY status`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return result, err
		}
		result.Tasks[status] = n
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	// 状态查询通过 JSON 投影排除全部候选，减少读取和报告体积。
	rows, err = tx.QueryContext(ctx, `SELECT json_remove(data,'$.events','$.versions') FROM (SELECT data,date,category,captured_at FROM (SELECT data,date,category,json_extract(data,'$.captured_at') AS captured_at FROM library_batches) UNION ALL SELECT data,'' AS date,category,captured_at FROM library_source_observations) ORDER BY date DESC,category,captured_at DESC`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var raw []byte
		var b library.CategoryBatch
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return result, err
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			rows.Close()
			return result, err
		}
		result.Batches = append(result.Batches, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT data FROM library_gaps ORDER BY before_date DESC,category,after_date`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var raw []byte
		var g library.Gap
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return result, err
		}
		if err := json.Unmarshal(raw, &g); err != nil {
			rows.Close()
			return result, err
		}
		result.Gaps = append(result.Gaps, g)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}
