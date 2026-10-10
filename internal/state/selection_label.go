package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/papers"
)

const (
	LabelRelevant    = "relevant"
	LabelNotRelevant = "not_relevant"
)

type SelectionLabel struct {
	PaperID   string
	Label     string
	Snapshot  string
	UpdatedAt time.Time
}

type DigestPick struct {
	ID    string
	Title string
}

// SetSelectionLabel stores a manual judgment. It does not touch jobs, papers, or recommendations.
func (s *Store) SetSelectionLabel(ctx context.Context, topic, paperID, label, snapshot string) error {
	topic = strings.TrimSpace(topic)
	paperID = strings.TrimSpace(paperID)
	if topic == "" || paperID == "" || strings.ContainsAny(paperID, "\r\n") || len(paperID) > 200 {
		return ErrInvalidQuery
	}
	if label != LabelRelevant && label != LabelNotRelevant {
		return ErrInvalidQuery
	}
	if len(snapshot) > 64<<10 {
		return ErrInvalidQuery
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO selection_labels(topic, paper_id, label, snapshot, updated_at)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(topic, paper_id) DO UPDATE SET
	label = excluded.label,
	snapshot = CASE WHEN excluded.snapshot = '' THEN selection_labels.snapshot ELSE excluded.snapshot END,
	updated_at = excluded.updated_at`, topic, paperID, label, snapshot, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("save selection label: %w", err)
	}
	return nil
}

func (s *Store) ClearSelectionLabel(ctx context.Context, topic, paperID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM selection_labels WHERE topic = ? AND paper_id = ?`, topic, paperID); err != nil {
		return fmt.Errorf("clear selection label: %w", err)
	}
	return nil
}

func (s *Store) SelectionLabels(ctx context.Context, topic string) ([]SelectionLabel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT paper_id, label, snapshot, updated_at FROM selection_labels WHERE topic = ? ORDER BY paper_id`, topic)
	if err != nil {
		return nil, fmt.Errorf("list selection labels: %w", err)
	}
	defer rows.Close()
	var labels []SelectionLabel
	for rows.Next() {
		var item SelectionLabel
		var updated string
		if err := rows.Scan(&item.PaperID, &item.Label, &item.Snapshot, &updated); err != nil {
			return nil, fmt.Errorf("scan selection label: %w", err)
		}
		item.UpdatedAt, err = time.Parse(time.RFC3339, updated)
		if err != nil {
			return nil, fmt.Errorf("parse selection label time: %w", err)
		}
		labels = append(labels, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read selection labels: %w", err)
	}
	return labels, nil
}

// DigestPicks returns papers saved for a digest, in stored order.
// A missing day is an empty list, not a new job.
func (s *Store) DigestPicks(ctx context.Context, topic, date string) ([]DigestPick, error) {
	if !ValidDate(date) {
		return nil, ErrInvalidQuery
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE topic = ? AND date = ?)`, topic, date).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check digest picks: %w", err)
	}
	if exists == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT jp.paper_id, p.data FROM job_papers jp
JOIN papers p USING (topic, paper_id, version)
WHERE jp.topic = ? AND jp.date = ?
ORDER BY jp.position, jp.paper_id`, topic, date)
	if err != nil {
		return nil, fmt.Errorf("list digest picks: %w", err)
	}
	defer rows.Close()
	var picks []DigestPick
	for rows.Next() {
		var pick DigestPick
		var data []byte
		if err := rows.Scan(&pick.ID, &data); err != nil {
			return nil, fmt.Errorf("scan digest pick: %w", err)
		}
		var paper papers.Paper
		if err := json.Unmarshal(data, &paper); err != nil {
			return nil, fmt.Errorf("decode digest pick: %w", err)
		}
		pick.Title = paper.Title
		picks = append(picks, pick)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read digest picks: %w", err)
	}
	return picks, nil
}

// SelectionLabelSnapshot encodes the fields needed to rebuild a regression fixture.
func SelectionLabelSnapshot(paper papers.Paper) (string, error) {
	data, err := json.Marshal(paper)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func ParseSelectionSnapshot(snapshot string) (papers.Paper, error) {
	if strings.TrimSpace(snapshot) == "" {
		return papers.Paper{}, errors.New("empty snapshot")
	}
	var paper papers.Paper
	if err := json.Unmarshal([]byte(snapshot), &paper); err != nil {
		return papers.Paper{}, err
	}
	return paper, nil
}
