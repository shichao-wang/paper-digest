package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/digest"
	"github.com/shichao-wang/paper-digest/internal/papers"
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

var ErrInvalidQuery = errors.New("state: invalid browse query")

// 浏览 DTO 仅暴露论文与日报的展示数据。
type SummaryRecord struct {
	Text          string `json:"text"`
	Model         string `json:"model"`
	PromptVersion string `json:"promptVersion"`
}

type PaperRecord struct {
	ID          string         `json:"id"`
	Version     string         `json:"version"`
	Title       string         `json:"title"`
	Authors     []string       `json:"authors"`
	PublishedAt time.Time      `json:"publishedAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
	Abstract    string         `json:"abstract"`
	URL         string         `json:"url"`
	DigestDate  string         `json:"digestDate"`
	Position    int            `json:"position"`
	Status      string         `json:"status"`
	Summary     *SummaryRecord `json:"summary"`
}

type DigestRecord struct {
	Date         string `json:"date"`
	Status       string `json:"status"`
	PaperCount   int    `json:"paperCount"`
	SummaryCount int    `json:"summaryCount"`
}

type DigestDetail struct {
	DigestRecord
	Message string        `json:"message"`
	Items   []PaperRecord `json:"items"`
}

type PaperPage struct {
	Items    []PaperRecord `json:"items"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"pageSize"`
}

type DigestPage struct {
	Items    []DigestRecord `json:"items"`
	Total    int            `json:"total"`
	Page     int            `json:"page"`
	PageSize int            `json:"pageSize"`
}

type PageQuery struct {
	Page     int
	PageSize int
}

type PaperQuery struct {
	PageQuery
	Q       string
	Date    string
	Summary string
}

func ValidDate(date string) bool {
	parsed, err := time.Parse("2006-01-02", date)
	return err == nil && parsed.Format("2006-01-02") == date
}

func (q PageQuery) normalized() (PageQuery, error) {
	if q.Page == 0 {
		q.Page = 1
	}
	if q.PageSize == 0 {
		q.PageSize = DefaultPageSize
	}
	// 保证 LIMIT/OFFSET 在整数范围内，兼容 32 位平台。
	if q.Page < 1 || q.PageSize < 1 || q.PageSize > MaxPageSize || q.Page-1 > int(^uint(0)>>1)/q.PageSize {
		return q, ErrInvalidQuery
	}
	return q, nil
}

// 先选最新关联再筛选，避免旧日报的摘要或搜索结果混入当前版本。
const paperRepresentatives = `WITH ranked AS (
 SELECT jp.*, j.status, p.data,
 ROW_NUMBER() OVER (PARTITION BY jp.topic, jp.paper_id ORDER BY jp.date DESC) AS rank
 FROM job_papers jp JOIN papers p USING(topic, paper_id, version)
 JOIN jobs j USING(topic, date)
 WHERE jp.topic = ? AND (? = '' OR jp.date = ?)
), selected AS (SELECT * FROM ranked WHERE rank = 1)
`

func (s *Store) BrowsePapers(ctx context.Context, topic string, query PaperQuery) (PaperPage, error) {
	page, err := query.PageQuery.normalized()
	if err != nil {
		return PaperPage{}, err
	}
	if query.Date != "" && !ValidDate(query.Date) {
		return PaperPage{}, ErrInvalidQuery
	}
	if query.Summary == "" {
		query.Summary = "all"
	}
	if query.Summary != "all" && query.Summary != "available" && query.Summary != "missing" {
		return PaperPage{}, ErrInvalidQuery
	}
	where := ` WHERE (? = 'all' OR (? = 'available' AND summary IS NOT NULL) OR (? = 'missing' AND summary IS NULL))`
	args := []any{topic, query.Date, query.Date, query.Summary, query.Summary, query.Summary}
	if query.Q != "" {
		pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(query.Q) + "%"
		where += ` AND (json_extract(data, '$.Title') LIKE ? ESCAPE '\'
 OR json_extract(data, '$.Abstract') LIKE ? ESCAPE '\'
 OR EXISTS (SELECT 1 FROM json_each(json_extract(data, '$.Authors')) WHERE value LIKE ? ESCAPE '\')
 OR json_extract(summary, '$.Text') LIKE ? ESCAPE '\')`
		args = append(args, pattern, pattern, pattern, pattern)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PaperPage{}, fmt.Errorf("begin paper browse: %w", err)
	}
	defer tx.Rollback()
	result := PaperPage{Items: make([]PaperRecord, 0), Page: page.Page, PageSize: page.PageSize}
	if err := tx.QueryRowContext(ctx, paperRepresentatives+`SELECT COUNT(*) FROM selected`+where, args...).Scan(&result.Total); err != nil {
		return PaperPage{}, fmt.Errorf("count browse papers: %w", err)
	}
	rowArgs := append(append([]any{}, args...), page.PageSize, (page.Page-1)*page.PageSize)
	rows, err := tx.QueryContext(ctx, paperRepresentatives+`SELECT data, summary, date, position, status FROM selected`+where+` ORDER BY date DESC, position, paper_id LIMIT ? OFFSET ?`, rowArgs...)
	if err != nil {
		return PaperPage{}, fmt.Errorf("browse papers: %w", err)
	}
	result.Items, err = readPaperRows(rows)
	if err != nil {
		return PaperPage{}, err
	}
	if err := tx.Commit(); err != nil {
		return PaperPage{}, fmt.Errorf("finish paper browse: %w", err)
	}
	return result, nil
}

func decodePaper(data, summary []byte, date string, position int, status string) (PaperRecord, error) {
	var paper papers.Paper
	if err := json.Unmarshal(data, &paper); err != nil {
		return PaperRecord{}, fmt.Errorf("decode browse paper: %w", err)
	}
	authors := paper.Authors
	if authors == nil {
		authors = []string{}
	}
	record := PaperRecord{ID: paper.ID, Version: paper.Version, Title: paper.Title, Authors: authors,
		PublishedAt: paper.Published, UpdatedAt: paper.Updated, Abstract: paper.Abstract, URL: paper.URL,
		DigestDate: date, Position: position, Status: status}
	// 使用源版本；缺版本时的内部 hash 不是 arXiv 版本。
	if summary != nil {
		var saved digest.Summary
		if err := json.Unmarshal(summary, &saved); err != nil {
			return PaperRecord{}, fmt.Errorf("decode browse summary: %w", err)
		}
		record.Summary = &SummaryRecord{Text: saved.Text, Model: saved.Model, PromptVersion: saved.PromptVersion}
	}
	return record, nil
}

func readPaperRows(rows *sql.Rows) ([]PaperRecord, error) {
	defer rows.Close()
	items := make([]PaperRecord, 0)
	for rows.Next() {
		var data, summary []byte
		var date, status string
		var position int
		if err := rows.Scan(&data, &summary, &date, &position, &status); err != nil {
			return nil, fmt.Errorf("scan browse paper: %w", err)
		}
		item, err := decodePaper(data, summary, date, position, status)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read browse papers: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) PaperDetail(ctx context.Context, topic, id, date string) (PaperRecord, error) {
	if id == "" || !ValidDate(date) {
		return PaperRecord{}, ErrInvalidQuery
	}
	var data, summary []byte
	var position int
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT p.data, jp.summary, jp.position, j.status
 FROM job_papers jp JOIN papers p USING(topic, paper_id, version) JOIN jobs j USING(topic, date)
 WHERE jp.topic = ? AND jp.paper_id = ? AND jp.date = ?`, topic, id, date).Scan(&data, &summary, &position, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return PaperRecord{}, ErrPaperNotFound
	}
	if err != nil {
		return PaperRecord{}, fmt.Errorf("get browse paper: %w", err)
	}
	return decodePaper(data, summary, date, position, status)
}

func (s *Store) BrowseDigests(ctx context.Context, topic string, query PageQuery) (DigestPage, error) {
	page, err := query.normalized()
	if err != nil {
		return DigestPage{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DigestPage{}, fmt.Errorf("begin digest browse: %w", err)
	}
	defer tx.Rollback()
	result := DigestPage{Items: make([]DigestRecord, 0), Page: page.Page, PageSize: page.PageSize}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE topic = ?`, topic).Scan(&result.Total); err != nil {
		return DigestPage{}, fmt.Errorf("count digests: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT j.date, j.status, COUNT(jp.paper_id), COUNT(jp.summary)
 FROM jobs j LEFT JOIN job_papers jp USING(topic, date)
 WHERE j.topic = ? GROUP BY j.topic, j.date ORDER BY j.date DESC LIMIT ? OFFSET ?`, topic, page.PageSize, (page.Page-1)*page.PageSize)
	if err != nil {
		return DigestPage{}, fmt.Errorf("browse digests: %w", err)
	}
	for rows.Next() {
		var item DigestRecord
		if err := rows.Scan(&item.Date, &item.Status, &item.PaperCount, &item.SummaryCount); err != nil {
			rows.Close()
			return DigestPage{}, fmt.Errorf("scan browse digest: %w", err)
		}
		result.Items = append(result.Items, item)
	}
	rowErr := rows.Err()
	closeErr := rows.Close()
	if err := errors.Join(rowErr, closeErr); err != nil {
		return DigestPage{}, fmt.Errorf("read browse digests: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return DigestPage{}, fmt.Errorf("finish digest browse: %w", err)
	}
	return result, nil
}

func (s *Store) DigestDetail(ctx context.Context, topic, date string) (DigestDetail, error) {
	if !ValidDate(date) {
		return DigestDetail{}, ErrInvalidQuery
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DigestDetail{}, fmt.Errorf("begin digest detail: %w", err)
	}
	defer tx.Rollback()
	result := DigestDetail{}
	err = tx.QueryRowContext(ctx, `SELECT date, status, message FROM jobs WHERE topic = ? AND date = ?`, topic, date).Scan(&result.Date, &result.Status, &result.Message)
	if errors.Is(err, sql.ErrNoRows) {
		return DigestDetail{}, ErrJobNotFound
	}
	if err != nil {
		return DigestDetail{}, fmt.Errorf("get browse digest: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.data, jp.summary, jp.date, jp.position, j.status
 FROM job_papers jp JOIN papers p USING(topic, paper_id, version) JOIN jobs j USING(topic, date)
 WHERE jp.topic = ? AND jp.date = ? ORDER BY jp.position, jp.paper_id`, topic, date)
	if err != nil {
		return DigestDetail{}, fmt.Errorf("get digest papers: %w", err)
	}
	result.Items, err = readPaperRows(rows)
	if err != nil {
		return DigestDetail{}, err
	}
	result.PaperCount = len(result.Items)
	for _, item := range result.Items {
		if item.Summary != nil {
			result.SummaryCount++
		}
	}
	if err := tx.Commit(); err != nil {
		return DigestDetail{}, fmt.Errorf("finish digest detail: %w", err)
	}
	return result, nil
}

func (s *Store) Health(ctx context.Context) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&count); err != nil {
		return fmt.Errorf("query database health: %w", err)
	}
	return nil
}
