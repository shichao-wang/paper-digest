package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/eval"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/internal/state"
	"github.com/shichao-wang/paper-digest/skills"
)

func defaultEvalFetch(ctx context.Context, window papers.Window, searchQuery string) ([]papers.Paper, error) {
	return papers.FetchWindow(ctx, &http.Client{Timeout: 45 * time.Second}, "", window, searchQuery)
}

func (s *server) evalAPI(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/eval":
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.evalPreview(w, r)
	case "/api/eval/rules":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.evalRules(w, r)
	case "/api/eval/skill":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.evalSkill(w, r)
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

func (s *server) evalRules(w http.ResponseWriter, r *http.Request) {
	topic, rules, ok := s.requireSelection(w, r)
	if !ok {
		return
	}
	_ = rules
	writeJSON(w, http.StatusOK, s.selections[topic.ID])
}

func (s *server) evalSkill(w http.ResponseWriter, r *http.Request) {
	topic, _, ok := s.requireSelection(w, r)
	if !ok {
		return
	}
	skill, exists := s.screeningSkills[topic.ID]
	if !exists || strings.TrimSpace(skill.Body) == "" {
		writeError(w, http.StatusNotFound, "screening skill is not configured")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}{skill.Name, skill.Description, skill.Body})
}

func (s *server) evalPreview(w http.ResponseWriter, r *http.Request) {
	topic, active, ok := s.requireSelection(w, r)
	if !ok {
		return
	}
	var draft *papers.Rules
	var screen bool
	var screeningSkill string
	date, end := r.URL.Query().Get("date"), r.URL.Query().Get("to")
	if r.Method == http.MethodPost {
		if !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "same-origin request required")
			return
		}
		parsedDate, parsedEnd, compiled, wantScreen, skillText, hasDraft, err := decodeDraft(w, r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		date, end, screen, screeningSkill = parsedDate, parsedEnd, wantScreen, skillText
		if hasDraft {
			draft = &compiled
		}
	}
	dates, err := eval.Dates(date, end)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid query parameters")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	fetch := s.fetch
	if fetch == nil {
		fetch = defaultEvalFetch
	}
	query := active.Query
	if draft != nil {
		query = papers.OrQuery(active.Query, draft.Query)
	}
	fetched, err := papers.FetchEachBatch(ctx, dates, query, fetch)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	labels, sent, notes, err := s.evalContext(ctx, topic.ID, dates)
	if err != nil {
		writeStatus(w, err)
		return
	}
	if screen {
		notes, err = s.screenEval(ctx, topic.ID, dates, active, draft, fetched, notes, screeningSkill)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	report, err := eval.Build(topic.ID, dates, active, draft, fetched, sent, labels, notes)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func decodeDraft(w http.ResponseWriter, r *http.Request) (string, string, papers.Rules, bool, string, bool, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return "", "", papers.Rules{}, false, "", false, errors.New("application/json required")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	var body struct {
		Date           string          `json:"date"`
		To             string          `json:"to"`
		Rules          json.RawMessage `json:"rules"`
		Screen         bool            `json:"screen"`
		ScreeningSkill string          `json:"screeningSkill"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return "", "", papers.Rules{}, false, "", false, errors.New("invalid query parameters")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", "", papers.Rules{}, false, "", false, errors.New("invalid query parameters")
	}
	skillText := strings.TrimSpace(body.ScreeningSkill)
	if skillText != "" {
		if _, err := skills.Instruction(skillText, skills.Skill{}); err != nil {
			return "", "", papers.Rules{}, false, "", false, err
		}
	}
	trimmedRules := bytes.TrimSpace(body.Rules)
	if len(trimmedRules) == 0 || string(trimmedRules) == "null" {
		return body.Date, body.To, papers.Rules{}, body.Screen, skillText, false, nil
	}
	spec, err := papers.ParseSelection(body.Rules)
	if err != nil {
		return "", "", papers.Rules{}, false, "", false, err
	}
	compiled, err := papers.Compile(spec)
	if err != nil {
		return "", "", papers.Rules{}, false, "", false, err
	}
	return body.Date, body.To, compiled, body.Screen, skillText, true, nil
}

func (s *server) evalLabel(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "same-origin request required")
		return
	}
	topic, _, ok := s.requireSelection(w, r)
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
	topic, _, ok := s.requireSelection(w, r)
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

func (s *server) requireSelection(w http.ResponseWriter, r *http.Request) (topicRecord, papers.Rules, bool) {
	topic, ok := s.selectTopic(w, r)
	if !ok {
		return topicRecord{}, papers.Rules{}, false
	}
	rules, exists := s.rules[topic.ID]
	if !exists || !rules.Active() {
		writeError(w, http.StatusBadRequest, "selection rules are not configured for this topic")
		return topicRecord{}, papers.Rules{}, false
	}
	return topic, rules, true
}

func (s *server) evalContext(ctx context.Context, topic string, dates []string) (map[string]string, map[string][]eval.SentPaper, map[string]eval.DayNote, error) {
	rows, err := s.store.SelectionLabels(ctx, topic)
	if err != nil {
		return nil, nil, nil, err
	}
	labels := map[string]string{}
	for _, row := range rows {
		labels[row.PaperID] = row.Label
	}
	sent := map[string][]eval.SentPaper{}
	notes := map[string]eval.DayNote{}
	for _, date := range dates {
		picks, err := s.store.DigestPicks(ctx, topic, date)
		if err != nil {
			return nil, nil, nil, err
		}
		list := make([]eval.SentPaper, 0, len(picks))
		for _, pick := range picks {
			list = append(list, eval.SentPaper{ID: pick.ID, Title: pick.Title})
		}
		sent[date] = list
		current, decisions, err := s.store.DailyRecord(ctx, topic, date)
		if err != nil {
			return nil, nil, nil, err
		}
		note := eval.DayNote{Status: current.Status, Message: current.Message}
		if len(decisions) > 0 {
			note.Source = "stored"
			note.Decisions = make([]eval.ScreenNote, len(decisions))
			for i, decision := range decisions {
				note.Decisions[i] = eval.ScreenNote{ID: decision.PaperID, Relevant: decision.Relevant, Score: decision.Score, Reason: decision.Reason, Skill: decision.Skill}
			}
		}
		notes[date] = note
	}
	return labels, sent, notes, nil
}

func (s *server) screenEval(ctx context.Context, topic string, dates []string, active papers.Rules, draft *papers.Rules, fetched []papers.Paper, notes map[string]eval.DayNote, skillText string) (map[string]eval.DayNote, error) {
	if s.screen == nil {
		return nil, errors.New("精筛未配置，没有调用模型，也没有写入结果")
	}
	instruction := ""
	version := ""
	if skillText != "" {
		fallback := skills.Skill{}
		if skill, ok := s.screeningSkills[topic]; ok {
			fallback = skill
		}
		skill, err := skills.Instruction(skillText, fallback)
		if err != nil {
			return nil, err
		}
		instruction = skill.Body
		version = skill.Version()
	} else if skill, ok := s.screeningSkills[topic]; ok {
		instruction = skill.Body
		version = skill.Version()
	}
	if strings.TrimSpace(instruction) == "" {
		return nil, errors.New("精筛技能为空，没有调用模型，也没有写入结果")
	}
	if notes == nil {
		notes = map[string]eval.DayNote{}
	}
	for _, date := range dates {
		batch, ok, err := papers.DigestBatch(date)
		if err != nil {
			return nil, err
		}
		window := batch.Window
		if !ok {
			window = papers.Window{}
		}
		selected := eval.ScreenTargets(fetched, window, active, draft)
		note := notes[date]
		note.Source = "model"
		note.Decisions = nil
		if len(selected) > 0 {
			decisions, err := s.screen(ctx, instruction, selected)
			if err != nil {
				return nil, fmt.Errorf("精筛没有完成，没有写入结果: %w", err)
			}
			note.Decisions = make([]eval.ScreenNote, len(decisions))
			for i, decision := range decisions {
				if decision.Skill == "" {
					decision.Skill = version
				}
				note.Decisions[i] = eval.ScreenNote{ID: decision.ID, Relevant: decision.Relevant, Score: decision.Score, Reason: decision.Reason, Skill: decision.Skill}
			}
		}
		notes[date] = note
	}
	return notes, nil
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
