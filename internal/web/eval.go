package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/eval"
	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/internal/state"
)

func defaultEvalFetch(ctx context.Context, since time.Time) ([]papers.Paper, error) {
	return papers.FetchSince(ctx, &http.Client{Timeout: 45 * time.Second}, "", since)
}

func (s *server) evalAPI(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/eval":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.evalPreview(w, r)
	case "/api/eval/labels":
		if r.Method != http.MethodPut {
			w.Header().Set("Allow", "PUT")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		s.evalLabel(w, r.WithContext(ctx))
	default:
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		s.evalFixtures(w, r.WithContext(ctx))
	}
}

func (s *server) evalPreview(w http.ResponseWriter, r *http.Request) {
	topic, ok := s.requireJointTopic(w, r)
	if !ok {
		return
	}
	dates, err := eval.Dates(r.URL.Query().Get("date"), r.URL.Query().Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid query parameters")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	cutoff, err := eval.Cutoff(dates[0])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid query parameters")
		return
	}
	fetch := s.fetch
	if fetch == nil {
		fetch = defaultEvalFetch
	}
	since := cutoff.Add(-time.Duration(s.lookbackDays) * 24 * time.Hour)
	fetched, err := fetch(ctx, since)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	labels, sent, err := s.evalContext(ctx, topic.ID, dates)
	if err != nil {
		writeStatus(w, err)
		return
	}
	report, err := eval.Build(topic.ID, dates, s.lookbackDays, fetched, sent, labels)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *server) evalLabel(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "same-origin request required")
		return
	}
	topic, ok := s.requireJointTopic(w, r)
	if !ok {
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "application/json required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 96<<10)
	var body struct {
		PaperID  string          `json:"paperID"`
		Label    string          `json:"label"`
		Snapshot json.RawMessage `json:"snapshot"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid query parameters")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid query parameters")
		return
	}
	paperID := strings.TrimSpace(body.PaperID)
	if paperID == "" || strings.ContainsAny(paperID, "\r\n") || len(paperID) > 200 {
		writeError(w, http.StatusBadRequest, "invalid query parameters")
		return
	}
	if body.Label == "clear" {
		if err := s.store.ClearSelectionLabel(r.Context(), topic.ID, paperID); err != nil {
			writeStatus(w, err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Cleared bool `json:"cleared"`
		}{true})
		return
	}
	if body.Label != eval.LabelRelevant && body.Label != eval.LabelNotRelevant {
		writeError(w, http.StatusBadRequest, "invalid query parameters")
		return
	}
	snapshot, err := canonicalSnapshot(paperID, body.Snapshot)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid query parameters")
		return
	}
	if err := s.store.SetSelectionLabel(r.Context(), topic.ID, paperID, body.Label, snapshot); err != nil {
		writeStatus(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		PaperID string `json:"paperID"`
		Label   string `json:"label"`
	}{paperID, body.Label})
}

func (s *server) evalFixtures(w http.ResponseWriter, r *http.Request) {
	topic, ok := s.requireJointTopic(w, r)
	if !ok {
		return
	}
	rows, err := s.store.SelectionLabels(r.Context(), topic.ID)
	if err != nil {
		writeStatus(w, err)
		return
	}
	stored := make([]eval.StoredLabel, 0, len(rows))
	for _, row := range rows {
		stored = append(stored, eval.StoredLabel{PaperID: row.PaperID, Label: row.Label, Snapshot: row.Snapshot})
	}
	fixtures, _ := eval.Fixtures(stored)
	if fixtures == nil {
		fixtures = []eval.Fixture{}
	}
	w.Header().Set("Content-Disposition", `attachment; filename="selection-fixtures.json"`)
	writeJSON(w, http.StatusOK, fixtures)
}

func (s *server) requireJointTopic(w http.ResponseWriter, r *http.Request) (topicRecord, bool) {
	topic, ok := s.selectTopic(w, r)
	if !ok {
		return topicRecord{}, false
	}
	if topic.ID != job.Topic {
		writeError(w, http.StatusBadRequest, "selection rules apply only to the joint topic")
		return topicRecord{}, false
	}
	return topic, true
}

func (s *server) evalContext(ctx context.Context, topic string, dates []string) (map[string]string, map[string][]eval.SentPaper, error) {
	rows, err := s.store.SelectionLabels(ctx, topic)
	if err != nil {
		return nil, nil, err
	}
	labels := map[string]string{}
	for _, row := range rows {
		labels[row.PaperID] = row.Label
	}
	sent := map[string][]eval.SentPaper{}
	for _, date := range dates {
		picks, err := s.store.DigestPicks(ctx, topic, date)
		if err != nil {
			return nil, nil, err
		}
		list := make([]eval.SentPaper, 0, len(picks))
		for _, pick := range picks {
			list = append(list, eval.SentPaper{ID: pick.ID, Title: pick.Title})
		}
		sent[date] = list
	}
	return labels, sent, nil
}

func canonicalSnapshot(paperID string, raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil
	}
	var paper papers.Paper
	if err := json.Unmarshal(trimmed, &paper); err != nil {
		return "", err
	}
	if paper.ID != "" && paper.ID != paperID {
		return "", errors.New("snapshot id mismatch")
	}
	if paper.ID == "" && strings.TrimSpace(paper.Title) == "" && strings.TrimSpace(paper.Abstract) == "" && len(paper.Categories) == 0 {
		return "", nil
	}
	if paper.ID == "" {
		paper.ID = paperID
	}
	data, err := json.Marshal(paper)
	if err != nil || len(data) > 64<<10 {
		return "", errors.New("snapshot too large")
	}
	return string(data), nil
}

func writeStatus(w http.ResponseWriter, err error) {
	if errors.Is(err, state.ErrInvalidQuery) {
		writeError(w, http.StatusBadRequest, "invalid query parameters")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "service unavailable")
}
