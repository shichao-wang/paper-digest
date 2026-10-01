package web

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/shichao-wang/paper-digest/internal/digest"
	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/internal/state"
)

func testHandler(t *testing.T) (*state.Store, http.Handler) {
	t.Helper()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	const date = "2026-09-29"
	if _, err := store.ClaimDay(ctx, job.Topic, date); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCandidates(ctx, job.Topic, date, []papers.Paper{{ID: "arxiv:hep-th/9901001", Version: "v1", Title: "fixture", Authors: []string{"A"}}, {ID: "missing", Title: "pending"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSummary(ctx, job.Topic, date, "arxiv:hep-th/9901001", digest.Summary{Text: "中文要点", Model: "fixture-model", PromptVersion: "fixture-v1"}); err != nil {
		t.Fatal(err)
	}
	handler, err := New(store, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>fixture SPA</html>")}, "assets/app.js": &fstest.MapFile{Data: []byte("console.log('fixture')")}})
	if err != nil {
		t.Fatal(err)
	}
	return store, handler
}

func request(handler http.Handler, method, target, accept string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Accept", accept)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestJSONContractAndOldArxivID(t *testing.T) {
	_, handler := testHandler(t)
	w := request(handler, "GET", "/api/papers", "application/json")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("response=%d %s", w.Code, w.Body.String())
	}
	var page map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"items", "total", "page", "pageSize"} {
		if page[key] == nil {
			t.Fatalf("missing %s: %s", key, w.Body.String())
		}
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(page["items"], &items); err != nil {
		t.Fatal(err)
	}
	want := []string{"id", "version", "title", "authors", "publishedAt", "updatedAt", "abstract", "url", "digestDate", "position", "status", "summary"}
	if len(items) != 2 || len(items[0]) != len(want) {
		t.Fatalf("items=%s", page["items"])
	}
	for _, key := range want {
		if items[0][key] == nil {
			t.Fatalf("missing %s: %s", key, page["items"])
		}
	}
	var summary map[string]json.RawMessage
	json.Unmarshal(items[0]["summary"], &summary)
	if len(summary) != 3 || summary["text"] == nil || summary["model"] == nil || summary["promptVersion"] == nil || string(items[1]["summary"]) != "null" {
		t.Fatalf("summaries=%s", page["items"])
	}
	detail := request(handler, "GET", "/api/papers/detail?id=arxiv%3Ahep-th%2F9901001&date=2026-09-29", "application/json")
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), `"id":"arxiv:hep-th/9901001"`) {
		t.Fatalf("old ID detail=%d %s", detail.Code, detail.Body.String())
	}
	digest := request(handler, "GET", "/api/digests/2026-09-29", "")
	if digest.Code != 200 || !strings.Contains(digest.Body.String(), `"paperCount":2`) || !strings.Contains(digest.Body.String(), `"summaryCount":1`) || !strings.Contains(digest.Body.String(), `"message":""`) {
		t.Fatalf("digest=%d %s", digest.Code, digest.Body.String())
	}
	history := request(handler, "GET", "/api/digests?pageSize=1", "")
	if history.Code != 200 || !strings.Contains(history.Body.String(), `"pageSize":1`) {
		t.Fatalf("history=%d %s", history.Code, history.Body.String())
	}
}

func TestAPIValidationErrorsMethodsAndIsolation(t *testing.T) {
	_, handler := testHandler(t)
	for _, target := range []string{"/api/papers?page=0", "/api/papers?page=-1", "/api/papers?page=oops", "/api/papers?page=999999999999999999999", "/api/papers?pageSize=0", "/api/papers?pageSize=101", "/api/papers?pageSize=", "/api/papers?date=2026-02-30", "/api/papers?summary=bad", "/api/papers/detail?id=x", "/api/papers/detail?date=2026-09-29", "/api/digests/invalid", "/api/digests?page=-1"} {
		w := request(handler, "GET", target, "")
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"error":`) {
			t.Errorf("target=%s response=%d %s", target, w.Code, w.Body.String())
		}
	}
	for _, target := range []string{"/api/unknown", "/api", "/api/digests/2026-09-01", "/api/papers/detail?id=no&date=2026-09-29", "/api/digests/2026-09-29/extra"} {
		w := request(handler, "GET", target, "text/html")
		if w.Code != 404 || strings.Contains(w.Body.String(), "fixture SPA") {
			t.Errorf("target=%s response=%d %s", target, w.Code, w.Body.String())
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE", "HEAD"} {
		w := request(handler, method, "/api/papers", "")
		if w.Code != 405 || w.Header().Get("Allow") != "GET" {
			t.Fatalf("method=%s response=%d", method, w.Code)
		}
	}
}

func TestStaticResourcesAndNavigationFallback(t *testing.T) {
	_, handler := testHandler(t)
	for _, tc := range []struct {
		target, accept string
		status         int
		spa            bool
	}{
		{"/", "", 200, true}, {"/papers/old", "text/html", 200, true}, {"/papers/old", "application/json", 404, false},
		{"/papers/old", "text/html;q=0", 404, false}, {"/assets/app.js", "", 200, false}, {"/assets/missing.js", "text/html", 404, false},
		{"/assets/missing", "text/html", 404, false}, {"/missing.css", "text/html", 404, false}, {"/../secret", "text/html", 404, false},
	} {
		w := request(handler, "GET", tc.target, tc.accept)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "fixture SPA") != tc.spa {
			t.Errorf("target=%s response=%d %s", tc.target, w.Code, w.Body.String())
		}
	}
	if w := request(handler, "HEAD", "/papers", "text/html"); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD response=%d %s", w.Code, w.Body.String())
	}
}

func TestHealthChecksDatabaseAndDoesNotRecoverSending(t *testing.T) {
	store, handler := testHandler(t)
	ctx := context.Background()
	if _, err := store.ClaimDay(ctx, job.Topic, "2026-09-30"); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(ctx, job.Topic, "2026-09-30", "message"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimSend(ctx, job.Topic, "2026-09-30"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/api/health", "/api/papers", "/api/digests", "/api/digests/2026-09-30"} {
		w := request(handler, "GET", target, "")
		if w.Code != 200 {
			t.Fatalf("target=%s response=%d %s", target, w.Code, w.Body.String())
		}
	}
	saved, err := store.GetJob(ctx, job.Topic, "2026-09-30")
	if err != nil || saved.Status != "sending" {
		t.Fatalf("read changed sending: %+v %v", saved, err)
	}
	store.Close()
	for _, target := range []string{"/api/health", "/api/papers"} {
		w := request(handler, "GET", target, "")
		if w.Code != 503 || w.Body.String() != "{\"error\":\"service unavailable\"}\n" {
			t.Fatalf("database error exposed: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestStaticIndexFollowsRebuiltAssets(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	files := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte(`<script src="/assets/old.js"></script>`)},
		"assets/old.js": &fstest.MapFile{Data: []byte("old")},
	}
	handler, err := New(store, files)
	if err != nil {
		t.Fatal(err)
	}
	files["index.html"] = &fstest.MapFile{Data: []byte(`<script src="/assets/new.js"></script>`)}
	files["assets/new.js"] = &fstest.MapFile{Data: []byte("new")}
	delete(files, "assets/old.js")
	for _, target := range []string{"/", "/papers"} {
		response := request(handler, "GET", target, "text/html")
		if response.Code != 200 || !strings.Contains(response.Body.String(), "/assets/new.js") || strings.Contains(response.Body.String(), "/assets/old.js") {
			t.Fatalf("重建后入口未更新：%d %s", response.Code, response.Body.String())
		}
	}
	if response := request(handler, "GET", "/assets/new.js", ""); response.Code != 200 {
		t.Fatalf("新资源不可用：%d", response.Code)
	}
	delete(files, "index.html")
	if response := request(handler, "GET", "/", ""); response.Code != 503 {
		t.Fatalf("入口缺失应明确失败：%d", response.Code)
	}
}

func TestStaticIndexRequiredAtConstruction(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, filesystem := range []fs.FS{nil, fstest.MapFS{}, fstest.MapFS{"index.html": &fstest.MapFile{Mode: fs.ModeDir}}} {
		if _, err := New(store, filesystem); err == nil {
			t.Fatal("missing index accepted")
		}
	}
}
