package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/eval"
	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/internal/state"
)

func TestEvalPreviewAndLabelsDoNotMutateTheDigest(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	cutoff, err := eval.Cutoff("2026-10-09")
	if err != nil {
		t.Fatal(err)
	}
	strong := papers.Paper{ID: "arxiv:strong", Title: "Sequential Recommendation", Published: cutoff.Add(-48 * time.Hour), Categories: []string{"cs.IR"}, URL: "https://arxiv.org/abs/strong"}
	weak := papers.Paper{ID: "arxiv:weak", Title: "Cross community", Abstract: "social", Published: cutoff.Add(-time.Hour), Categories: []string{"cs.IR"}, URL: "https://arxiv.org/abs/weak"}
	if _, err := store.ClaimDay(ctx, job.Topic, "2026-09-29"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimDay(ctx, job.Topic, "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCandidates(ctx, job.Topic, "2026-10-09", []papers.Paper{weak, strong}); err != nil {
		t.Fatal(err)
	}
	before, err := store.GetJob(ctx, job.Topic, "2026-10-09")
	if err != nil || before.Status != "processing" {
		t.Fatal(before, err)
	}

	calls := 0
	var gotSince time.Time
	handler, err := New(store, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("fixture")}}, Options{
		Topics:       []config.Topic{{ID: job.Topic}, {ID: "another"}},
		LookbackDays: 0,
		Fetch: func(_ context.Context, since time.Time, _ string) ([]papers.Paper, error) {
			calls++
			gotSince = since
			return []papers.Paper{weak, strong, {ID: "arxiv:plain", Title: "Unrelated physics", Published: cutoff.Add(-30 * time.Minute)}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	denied := httptest.NewRequest(http.MethodGet, "/api/eval?date=2026-10-09&topic=another", nil)
	deniedRec := httptest.NewRecorder()
	handler.ServeHTTP(deniedRec, denied)
	if deniedRec.Code != http.StatusBadRequest || calls != 0 {
		t.Fatalf("other topic status=%d calls=%d body=%s", deniedRec.Code, calls, deniedRec.Body.String())
	}
	for _, target := range []string{"/api/eval?date=2026-02-30", "/api/eval?date=2026-10-01&to=2026-10-20"} {
		w := request(handler, http.MethodGet, target, "")
		if w.Code != http.StatusBadRequest || calls != 0 {
			t.Fatalf("%s status=%d calls=%d", target, w.Code, calls)
		}
	}

	preview := request(handler, http.MethodGet, "/api/eval?date=2026-10-09", "")
	if preview.Code != http.StatusOK || calls != 1 || !gotSince.Equal(cutoff.Add(-7*24*time.Hour)) {
		t.Fatalf("preview=%d calls=%d since=%s body=%s", preview.Code, calls, gotSince, preview.Body.String())
	}
	var report eval.Report
	if err := json.Unmarshal(preview.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Days) != 1 || len(report.Days[0].Selected) != 1 || report.Days[0].Selected[0].ID != strong.ID || report.Days[0].Score.Precision != nil {
		t.Fatalf("report = %s", preview.Body.String())
	}
	if len(report.Days[0].OnlyLoose) != 1 || report.Days[0].OnlyLoose[0] != weak.ID || len(report.Days[0].OnlySent) != 1 || report.Days[0].OnlySent[0] != weak.ID {
		t.Fatalf("diff = loose %v sent %v", report.Days[0].OnlyLoose, report.Days[0].OnlySent)
	}

	labelBody := `{"paperID":"arxiv:strong","label":"relevant","snapshot":{"ID":"arxiv:strong","Title":"Sequential Recommendation","Abstract":"session logs","categories":["cs.IR"],"Published":"2026-10-07T01:00:00Z"}}`
	okLabel := putLabel(handler, "127.0.0.1:8081", "http://127.0.0.1:8081", labelBody)
	if okLabel.Code != http.StatusOK {
		t.Fatalf("label=%d %s", okLabel.Code, okLabel.Body.String())
	}
	cross := putLabel(handler, "attacker.invalid", "http://attacker.invalid", `{"paperID":"arxiv:strong","label":"not_relevant"}`)
	if cross.Code != http.StatusForbidden {
		t.Fatalf("cross-origin label=%d %s", cross.Code, cross.Body.String())
	}
	again := request(handler, http.MethodGet, "/api/eval?date=2026-10-09", "")
	if again.Code != http.StatusOK || calls != 2 {
		t.Fatalf("second preview=%d calls=%d", again.Code, calls)
	}
	if err := json.Unmarshal(again.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Days[0].Score.Precision == nil || *report.Days[0].Score.Precision != 1 || report.Days[0].Selected[0].Label != "relevant" {
		t.Fatalf("labeled score = %+v", report.Days[0].Score)
	}

	fixtures := request(handler, http.MethodGet, "/api/eval/fixtures", "")
	if fixtures.Code != http.StatusOK || !strings.Contains(fixtures.Header().Get("Content-Disposition"), "selection-fixtures.json") || !strings.Contains(fixtures.Body.String(), `"role":"positive"`) || !strings.Contains(fixtures.Body.String(), "Sequential Recommendation") {
		t.Fatalf("fixtures=%d %s %s", fixtures.Code, fixtures.Header().Get("Content-Disposition"), fixtures.Body.String())
	}

	cleared := putLabel(handler, "localhost", "", `{"paperID":"arxiv:strong","label":"clear"}`)
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear=%d %s", cleared.Code, cleared.Body.String())
	}
	rows, err := store.SelectionLabels(ctx, job.Topic)
	if err != nil || len(rows) != 0 {
		t.Fatalf("labels after clear = %+v %v", rows, err)
	}
	after, err := store.GetJob(ctx, job.Topic, "2026-10-09")
	if err != nil || after.Status != before.Status {
		t.Fatalf("digest status changed: %+v %v", after, err)
	}
	if _, err := store.GetJob(ctx, job.Topic, "2026-10-10"); !errors.Is(err, state.ErrJobNotFound) {
		t.Fatalf("eval created a job: %v", err)
	}
	seen, err := store.Seen(ctx, job.Topic, strong.ID)
	if err != nil || seen {
		t.Fatalf("eval marked seen: %v %v", seen, err)
	}
	kept, err := store.GetJob(ctx, job.Topic, "2026-09-29")
	if err != nil || kept.Status != "new" {
		t.Fatalf("other digest changed: %+v %v", kept, err)
	}

	for _, tc := range []struct {
		method, path, allow string
	}{
		{http.MethodPut, "/api/eval", "GET, POST"},
		{http.MethodGet, "/api/eval/labels", "PUT"},
		{http.MethodPost, "/api/eval/fixtures", "GET"},
	} {
		w := request(handler, tc.method, tc.path, "")
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != tc.allow {
			t.Fatalf("%s %s = %d allow %s", tc.method, tc.path, w.Code, w.Header().Get("Allow"))
		}
	}
}

func TestEvalDraftDoesNotChangeLiveRulesOrDigest(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	cutoff, err := eval.Cutoff("2026-10-09")
	if err != nil {
		t.Fatal(err)
	}
	strong := papers.Paper{ID: "arxiv:strong", Title: "Sequential Recommendation", Published: cutoff.Add(-48 * time.Hour), Categories: []string{"cs.IR"}}
	if _, err := store.ClaimDay(ctx, job.Topic, "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	queries := []string{}
	handler, err := New(store, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("fixture")}}, Options{
		Topics:       []config.Topic{{ID: job.Topic}},
		LookbackDays: 7,
		Fetch: func(_ context.Context, _ time.Time, query string) ([]papers.Paper, error) {
			queries = append(queries, query)
			return []papers.Paper{strong}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	current := request(handler, http.MethodGet, "/api/eval/rules", "")
	if current.Code != http.StatusOK || !strings.Contains(current.Body.String(), `"max_papers":5`) || strings.Contains(current.Body.String(), "zzzz-no-match") {
		t.Fatalf("rules = %d %s", current.Code, current.Body.String())
	}
	secret := "super-secret-pattern"
	bad := `{"date":"2026-10-09","rules":{"max_papers":5,"min_tier":1,"query":{"categories":["cs.IR"]},"signals":[{"name":"title:hit","pattern":"(` + secret + `","fields":["title"],"tier":2}],"decisions":[{"tier":2,"reason":"title match","min_signal_tier":2}],"fallback_reason":"no topic signal"}}`
	rejected := postEval(handler, "127.0.0.1:8081", "http://127.0.0.1:8081", bad)
	if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), "正则") || strings.Contains(rejected.Body.String(), secret) || len(queries) != 0 {
		t.Fatalf("bad rules = %d %s queries=%d", rejected.Code, rejected.Body.String(), len(queries))
	}
	cross := postEval(handler, "attacker.invalid", "http://attacker.invalid", `{"date":"2026-10-09","rules":{}}`)
	if cross.Code != http.StatusForbidden || len(queries) != 0 {
		t.Fatalf("cross-origin draft = %d %s", cross.Code, cross.Body.String())
	}
	draft := `{"date":"2026-10-09","rules":{"max_papers":5,"min_tier":1,"query":{"categories":["cs.LG"]},"signals":[{"name":"title:hit","pattern":"\\bzzzz-no-match\\b","fields":["title"],"tier":2}],"decisions":[{"tier":2,"reason":"title match","min_signal_tier":2}],"fallback_reason":"no topic signal"}}`
	okDraft := postEval(handler, "127.0.0.1:8081", "http://127.0.0.1:8081", draft)
	if okDraft.Code != http.StatusOK || len(queries) != 1 || !strings.Contains(queries[0], "cat:cs.IR") || !strings.Contains(queries[0], "cat:cs.LG") {
		t.Fatalf("draft = %d query=%v body=%s", okDraft.Code, queries, okDraft.Body.String())
	}
	var report eval.Report
	if err := json.Unmarshal(okDraft.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	day := report.Days[0]
	if day.Draft == nil || len(day.Selected) != 1 || len(day.Draft.Selected) != 0 || len(day.Draft.OnlyActive) != 1 || day.Draft.OnlyActive[0] != strong.ID || day.Candidates[0].DraftReason == "" {
		t.Fatalf("comparison = selected %d draft %+v", len(day.Selected), day.Draft)
	}
	again := request(handler, http.MethodGet, "/api/eval/rules", "")
	if again.Body.String() != current.Body.String() {
		t.Fatal("draft changed the active rules")
	}
	if _, err := store.GetJob(ctx, job.Topic, "2026-10-10"); !errors.Is(err, state.ErrJobNotFound) {
		t.Fatalf("draft created a job: %v", err)
	}
	seen, err := store.Seen(ctx, job.Topic, strong.ID)
	if err != nil || seen {
		t.Fatalf("draft marked seen: %v %v", seen, err)
	}
}

func postEval(handler http.Handler, host, origin, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/eval", strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func putLabel(handler http.Handler, host, origin, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/eval/labels", strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}
