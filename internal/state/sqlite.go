package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/shichao-wang/paper-digest/internal/digest"
	"github.com/shichao-wang/paper-digest/internal/papers"
	_ "modernc.org/sqlite"
)

const (
	statusNew        = "new"
	statusProcessing = "processing"
	statusReady      = "ready"
	statusSending    = "sending"
	statusSent       = "sent"
	statusUnknown    = "unknown"
	statusMissed     = "missed"
)

var (
	ErrJobNotFound   = errors.New("state: job not found")
	ErrPaperNotFound = errors.New("state: paper not found")
	ErrInvalidState  = errors.New("state: invalid job state")
)

type Job struct {
	Topic   string
	Date    string
	Status  string
	Message string
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn, err := sqliteDSN(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func sqliteDSN(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("state: database path is empty")
	}
	var u *url.URL
	if path == ":memory:" {
		u = &url.URL{Scheme: "file", Opaque: ":memory:"}
	} else if strings.HasPrefix(path, "file:") {
		parsed, err := url.Parse(path)
		if err != nil {
			return "", fmt.Errorf("parse sqlite path: %w", err)
		}
		u = parsed
	} else {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve sqlite path: %w", err)
		}
		u = &url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	}

	query := u.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	if path != ":memory:" {
		query.Add("_pragma", "journal_mode(WAL)")
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func (s *Store) initialize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		return fmt.Errorf("set sqlite busy timeout: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		return fmt.Errorf("enable sqlite WAL: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enable sqlite foreign keys: %w", err)
	}
	const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	topic TEXT NOT NULL,
	date TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('new', 'processing', 'ready', 'sending', 'sent', 'unknown', 'missed')),
	message TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (topic, date)
);
CREATE TABLE IF NOT EXISTS papers (
	topic TEXT NOT NULL,
	paper_id TEXT NOT NULL,
	version TEXT NOT NULL,
	data BLOB NOT NULL,
	PRIMARY KEY (topic, paper_id, version)
);
CREATE TABLE IF NOT EXISTS job_papers (
	topic TEXT NOT NULL,
	date TEXT NOT NULL,
	paper_id TEXT NOT NULL,
	version TEXT NOT NULL,
	position INTEGER NOT NULL,
	summary BLOB,
	PRIMARY KEY (topic, date, paper_id),
	FOREIGN KEY (topic, date) REFERENCES jobs(topic, date) ON DELETE CASCADE,
	FOREIGN KEY (topic, paper_id, version) REFERENCES papers(topic, paper_id, version)
);
CREATE TABLE IF NOT EXISTS recommendations (
	topic TEXT NOT NULL,
	paper_id TEXT NOT NULL,
	date TEXT NOT NULL,
	version TEXT NOT NULL,
	PRIMARY KEY (topic, paper_id)
);
CREATE INDEX IF NOT EXISTS job_papers_pending
	ON job_papers(topic, date, position) WHERE summary IS NULL;
CREATE INDEX IF NOT EXISTS jobs_pending
	ON jobs(date, topic) WHERE status IN ('new', 'processing', 'ready');
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("initialize sqlite schema: %w", err)
	}

	// 进程可能在记录发送意图后、记录结果前退出；重启后将结果标记为未知，禁止自动重发。
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ? WHERE status = ?`, statusUnknown, statusSending); err != nil {
		return fmt.Errorf("recover interrupted sends: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Backup(ctx context.Context, dest string) error {
	if strings.TrimSpace(dest) == "" {
		return errors.New("state: backup destination is empty")
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("state: backup destination already exists: %s", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check backup destination: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("backup sqlite database: %w", err)
	}
	return nil
}

func (s *Store) ClaimDay(ctx context.Context, topic, date string) (Job, error) {
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO jobs(topic, date, status) VALUES(?, ?, ?)
ON CONFLICT(topic, date) DO NOTHING`, topic, date, statusNew); err != nil {
		return Job{}, fmt.Errorf("claim daily job: %w", err)
	}
	return s.GetJob(ctx, topic, date)
}

func (s *Store) GetJob(ctx context.Context, topic, date string) (Job, error) {
	var job Job
	err := s.db.QueryRowContext(ctx, `
SELECT topic, date, status, message FROM jobs WHERE topic = ? AND date = ?`, topic, date).
		Scan(&job.Topic, &job.Date, &job.Status, &job.Message)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("get daily job: %w", err)
	}
	return job, nil
}

func (s *Store) ListPending(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT topic, date, status, message FROM jobs
WHERE status IN (?, ?, ?)
ORDER BY date, topic`, statusNew, statusProcessing, statusReady)
	if err != nil {
		return nil, fmt.Errorf("list pending jobs: %w", err)
	}
	defer rows.Close()

	jobs := make([]Job, 0)
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.Topic, &job.Date, &job.Status, &job.Message); err != nil {
			return nil, fmt.Errorf("scan pending job: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending jobs: %w", err)
	}
	return jobs, nil
}

func (s *Store) SaveCandidates(ctx context.Context, topic, date string, candidates []papers.Paper) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin candidate save: %w", err)
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM jobs WHERE topic = ? AND date = ?`, topic, date).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrJobNotFound
		}
		return fmt.Errorf("read candidate job: %w", err)
	}
	if status == statusProcessing {
		return tx.Commit()
	}
	if status != statusNew {
		return fmt.Errorf("save candidates in %q state: %w", status, ErrInvalidState)
	}

	for i, paper := range candidates {
		if paper.ID == "" {
			return fmt.Errorf("candidate %d has an empty paper ID", i)
		}
		data, err := json.Marshal(paper)
		if err != nil {
			return fmt.Errorf("encode paper %s: %w", paper.ID, err)
		}
		version := paper.Version
		if version == "" {
			sum := sha256.Sum256(data)
			version = hex.EncodeToString(sum[:])
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO papers(topic, paper_id, version, data) VALUES(?, ?, ?, ?)
ON CONFLICT(topic, paper_id, version) DO NOTHING`, topic, paper.ID, version, data); err != nil {
			return fmt.Errorf("save paper %s version %s: %w", paper.ID, version, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO job_papers(topic, date, paper_id, version, position)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(topic, date, paper_id) DO NOTHING`, topic, date, paper.ID, version, i); err != nil {
			return fmt.Errorf("save daily paper %s: %w", paper.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = ? WHERE topic = ? AND date = ? AND status = ?`, statusProcessing, topic, date, statusNew); err != nil {
		return fmt.Errorf("mark daily job processing: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit candidate save: %w", err)
	}
	return nil
}

func (s *Store) PendingPapers(ctx context.Context, topic, date string) ([]papers.Paper, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT p.data FROM job_papers AS jp
JOIN papers AS p USING (topic, paper_id, version)
WHERE jp.topic = ? AND jp.date = ? AND jp.summary IS NULL
ORDER BY jp.position`, topic, date)
	if err != nil {
		return nil, fmt.Errorf("list pending papers: %w", err)
	}
	defer rows.Close()

	pending := make([]papers.Paper, 0)
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("scan pending paper: %w", err)
		}
		var paper papers.Paper
		if err := json.Unmarshal(data, &paper); err != nil {
			return nil, fmt.Errorf("decode pending paper: %w", err)
		}
		pending = append(pending, paper)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending papers: %w", err)
	}
	return pending, nil
}

func (s *Store) Completed(ctx context.Context, topic, date string) ([]digest.Item, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM jobs WHERE topic = ? AND date = ?)`, topic, date).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check completed job: %w", err)
	}
	if exists == 0 {
		return nil, ErrJobNotFound
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT p.data, jp.summary FROM job_papers AS jp
JOIN papers AS p USING (topic, paper_id, version)
WHERE jp.topic = ? AND jp.date = ? AND jp.summary IS NOT NULL
ORDER BY jp.position`, topic, date)
	if err != nil {
		return nil, fmt.Errorf("list completed papers: %w", err)
	}
	defer rows.Close()

	completed := make([]digest.Item, 0)
	for rows.Next() {
		var paperData, summaryData []byte
		if err := rows.Scan(&paperData, &summaryData); err != nil {
			return nil, fmt.Errorf("scan completed paper: %w", err)
		}
		var item digest.Item
		if err := json.Unmarshal(paperData, &item.Paper); err != nil {
			return nil, fmt.Errorf("decode completed paper: %w", err)
		}
		if err := json.Unmarshal(summaryData, &item.Summary); err != nil {
			return nil, fmt.Errorf("decode summary for paper %s: %w", item.Paper.ID, err)
		}
		completed = append(completed, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read completed papers: %w", err)
	}
	return completed, nil
}

func (s *Store) SaveSummary(ctx context.Context, topic, date, id string, summary digest.Summary) error {
	data, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("encode summary for paper %s: %w", id, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin summary save: %w", err)
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM jobs WHERE topic = ? AND date = ?`, topic, date).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrJobNotFound
		}
		return fmt.Errorf("read job before saving summary: %w", err)
	}
	if status != statusProcessing {
		return fmt.Errorf("save summary in %q state: %w", status, ErrInvalidState)
	}

	result, err := tx.ExecContext(ctx, `
UPDATE job_papers SET summary = ?
WHERE topic = ? AND date = ? AND paper_id = ? AND summary IS NULL`, data, topic, date, id)
	if err != nil {
		return fmt.Errorf("save summary for paper %s: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check summary save for paper %s: %w", id, err)
	}
	if changed == 1 {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit summary for paper %s: %w", id, err)
		}
		return nil
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM job_papers WHERE topic = ? AND date = ? AND paper_id = ?)`, topic, date, id).Scan(&exists); err != nil {
		return fmt.Errorf("check paper %s: %w", id, err)
	}
	if exists == 0 {
		return fmt.Errorf("paper %s: %w", id, ErrPaperNotFound)
	}
	// 已保存的摘要不可变；崩溃后重试时复用原结果。
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit summary retry for paper %s: %w", id, err)
	}
	return nil
}

func (s *Store) Ready(ctx context.Context, topic, date, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ready transition: %w", err)
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM jobs WHERE topic = ? AND date = ?`, topic, date).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrJobNotFound
		}
		return fmt.Errorf("read job before ready: %w", err)
	}
	if status != statusNew && status != statusProcessing && status != statusReady {
		return fmt.Errorf("mark job ready from %q state: %w", status, ErrInvalidState)
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM job_papers WHERE topic = ? AND date = ? AND summary IS NULL`, topic, date).Scan(&pending); err != nil {
		return fmt.Errorf("check incomplete summaries: %w", err)
	}
	if pending > 0 {
		return fmt.Errorf("cannot mark ready with %d unsummarized papers", pending)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = ?, message = ? WHERE topic = ? AND date = ?`, statusReady, message, topic, date); err != nil {
		return fmt.Errorf("mark job ready: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ready transition: %w", err)
	}
	return nil
}

func (s *Store) MarkMissed(ctx context.Context, topic, date string) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE jobs SET status = ? WHERE topic = ? AND date = ? AND status IN (?, ?, ?, ?)`,
		statusMissed, topic, date, statusNew, statusProcessing, statusReady, statusMissed)
	if err != nil {
		return fmt.Errorf("mark daily job missed: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check missed transition: %w", err)
	}
	if changed == 0 {
		if _, err := s.GetJob(ctx, topic, date); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ClaimSend(ctx context.Context, topic, date string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE jobs SET status = ? WHERE topic = ? AND date = ? AND status = ?`,
		statusSending, topic, date, statusReady)
	if err != nil {
		return false, fmt.Errorf("claim message send: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check send claim: %w", err)
	}
	return changed == 1, nil
}

func (s *Store) MarkSent(ctx context.Context, topic, date string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sent transition: %w", err)
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM jobs WHERE topic = ? AND date = ?`, topic, date).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrJobNotFound
		}
		return fmt.Errorf("read job before sent: %w", err)
	}
	if status == statusSent {
		return tx.Commit()
	}
	if status != statusSending {
		return fmt.Errorf("mark job sent from %q state: %w", status, ErrInvalidState)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO recommendations(topic, paper_id, date, version)
SELECT topic, paper_id, date, version FROM job_papers WHERE topic = ? AND date = ?
ON CONFLICT(topic, paper_id) DO NOTHING`, topic, date); err != nil {
		return fmt.Errorf("record sent recommendations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = ? WHERE topic = ? AND date = ? AND status = ?`, statusSent, topic, date, statusSending); err != nil {
		return fmt.Errorf("mark job sent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sent transition: %w", err)
	}
	return nil
}

func (s *Store) MarkUnknown(ctx context.Context, topic, date string) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE jobs SET status = ? WHERE topic = ? AND date = ? AND status IN (?, ?)`,
		statusUnknown, topic, date, statusSending, statusUnknown)
	if err != nil {
		return fmt.Errorf("mark send outcome unknown: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check unknown transition: %w", err)
	}
	if changed == 0 {
		if _, err := s.GetJob(ctx, topic, date); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Seen(ctx context.Context, topic, id string) (bool, error) {
	var seen int
	if err := s.db.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM recommendations WHERE topic = ? AND paper_id = ?)`, topic, id).Scan(&seen); err != nil {
		return false, fmt.Errorf("check recommendation history: %w", err)
	}
	return seen == 1, nil
}
