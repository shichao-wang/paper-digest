package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/shichao-wang/paper-digest/internal/document"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/state"
)

type libraryFixture struct {
	store           *state.Store
	handler         http.Handler
	verifiedHandler http.Handler
	repository      *document.Repository
	id              library.Identity
	doc             library.Document
	running         library.Task
}

func newLibraryFixture(t *testing.T) libraryFixture {
	t.Helper()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	id := library.Identity{Source: "arxiv", PaperID: "hep-th/9901001", Version: "v2"}
	previous := id
	previous.Version = "v1"
	unrelated := library.Identity{Source: "arxiv", PaperID: "2610.00002", Version: "v1"}
	pending := library.Identity{Source: "arxiv", PaperID: "2610.00003", Version: "v1"}
	batch := library.CategoryBatch{
		Category: "cs.IR", Date: "2026-10-02", Completeness: "partial", Reason: "fixture category incomplete",
		Counts: map[string]int{"new": 3, "replace": 1},
		Versions: []library.Version{
			{Identity: previous, Title: "Previous recommendation model", Authors: []string{"Ada"}, Abstract: "old abstract"},
			{Identity: id, Title: "Current recommendation model", Authors: []string{"Ada"}, Abstract: "ranking abstract", AuthorKeywords: []string{"ranking-fixture"}, MetadataVerified: true},
			{Identity: unrelated, Title: "Unrelated physics", Authors: []string{"Bo"}},
			{Identity: pending, Title: "Pending relevance", Authors: []string{"Cy"}},
		},
		Events: []library.Announcement{{Identity: id, Type: "replace"}},
	}
	if err := store.SaveCategoryBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGap(ctx, library.Gap{Category: "cs.IR", After: "2026-09-29", Before: "2026-10-02", Reason: "fixture gap"}); err != nil {
		t.Fatal(err)
	}
	complete := func(identity library.Identity, stage string, c library.Completion) {
		t.Helper()
		if err := store.EnqueueTask(ctx, identity, stage, 0); err != nil {
			t.Fatal(err)
		}
		task, err := store.ClaimTask(ctx, stage, now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if task.Identity != identity {
			t.Fatalf("claimed wrong task: %+v", task)
		}
		if err := store.CompleteTask(ctx, task, c, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, identity := range []library.Identity{previous, id, unrelated} {
		relevance := library.Relevance{Level: "direct", DirectlyRelated: true, Topics: []string{"recommendation"}, Rationale: "fixture rationale", ExtractedKeywords: []string{"retrieval-fixture"}}
		if identity == unrelated {
			relevance.Level = "unrelated"
			relevance.DirectlyRelated = false
			relevance.Topics = []string{}
		}
		complete(identity, "relevance", library.Completion{Relevance: &relevance})
	}
	repository := &document.Repository{Root: t.TempDir(), BlockBytes: 48}
	doc, err := repository.SaveFixture(id, []string{
		"This synthetic recommendation document retains all page text and evidence. 中文证据坐标完整保留，验证字节范围与 hash。\nAppendix details and experimental settings remain available.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	complete(id, "document", library.Completion{Document: &doc})
	analysis := library.Analysis{SchemaVersion: library.SchemaVersion, PaperVersionID: id.Key(), Model: "fixture-model", PromptVersion: library.PromptVersion,
		DocumentHashes: []string{doc.Text.SHA256}, Content: library.AnalysisContent{TitleZH: "中文推荐论文", SummaryZH: library.Claim{Text: "fixture 中文摘要"}}}
	complete(id, "analyze", library.Completion{Analysis: &analysis, Run: library.Run{Model: "fixture-model", Requests: 2}})
	if err := store.EnqueueTask(ctx, id, "compare", 0); err != nil {
		t.Fatal(err)
	}
	running, err := store.ClaimTask(ctx, "compare", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCheckpoint(ctx, running, json.RawMessage(`{"private_model_context":"checkpoint-secret"}`), now); err != nil {
		t.Fatal(err)
	}
	staticFS := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("fixture SPA")}}
	handler, err := New(store, staticFS)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := NewWithDocuments(store, staticFS, repository)
	if err != nil {
		t.Fatal(err)
	}
	return libraryFixture{store: store, handler: handler, verifiedHandler: verified, repository: repository, id: id, doc: doc, running: running}
}

func libraryTarget(route string, id library.Identity) string {
	return route + "?" + url.Values{"id": {"arxiv:" + id.PaperID}, "version": {id.Version}}.Encode()
}

func evidenceTarget(id library.Identity, documentID, blockID string) string {
	return "/api/library/evidence?" + url.Values{"id": {"arxiv:" + id.PaperID}, "version": {id.Version}, "document": {documentID}, "block": {blockID}}.Encode()
}

func assertPrivateTaskFieldsAbsent(t *testing.T, body string, token string) {
	t.Helper()
	for _, secret := range []string{`"checkpoint"`, `"lease_token"`, "checkpoint-secret", "private_model_context", token} {
		if secret != "" && strings.Contains(body, secret) {
			t.Fatalf("private task field exposed (%s): %s", secret, body)
		}
	}
}

func TestLibraryListQueryContractAndPrivateTaskFields(t *testing.T) {
	fixture := newLibraryFixture(t)
	for _, tc := range []struct {
		query                    string
		total, items, page, size int
	}{
		{"", 2, 2, 1, state.DefaultPageSize},
		{"relevance=all&page=2&pageSize=2", 4, 2, 2, 2},
		{"relevance=all&page=9&pageSize=2", 4, 0, 9, 2},
		{"relevance=unrelated", 1, 1, 1, state.DefaultPageSize},
		{"relevance=pending", 1, 1, 1, state.DefaultPageSize},
		{"relevance=uncertain", 0, 0, 1, state.DefaultPageSize},
		{"topic=recommendation", 2, 2, 1, state.DefaultPageSize},
		{"topic=search", 0, 0, 1, state.DefaultPageSize},
		{"batch=cs.IR%2F2026-10-02&relevance=all", 4, 4, 1, state.DefaultPageSize},
		{"batch=2026-10-02&relevance=all", 4, 4, 1, state.DefaultPageSize},
		{"batch=cs.LG%2F2026-10-02&relevance=all", 0, 0, 1, state.DefaultPageSize},
		{"status=compare%3Arunning", 1, 1, 1, state.DefaultPageSize},
		{"status=running", 1, 1, 1, state.DefaultPageSize},
		{"status=queued&relevance=all", 4, 4, 1, state.DefaultPageSize},
		{"status=retry_wait&relevance=all", 0, 0, 1, state.DefaultPageSize},
		{"q=Current", 1, 1, 1, state.DefaultPageSize},
		{"q=Ada", 2, 2, 1, state.DefaultPageSize},
		{"q=ranking-fixture", 1, 1, 1, state.DefaultPageSize},
		{"q=retrieval-fixture", 2, 2, 1, state.DefaultPageSize},
		{"q=%E4%B8%AD%E6%96%87", 1, 1, 1, state.DefaultPageSize},
	} {
		t.Run(tc.query, func(t *testing.T) {
			w := request(fixture.handler, "GET", "/api/library/papers?"+tc.query, "application/json")
			if w.Code != 200 {
				t.Fatalf("response=%d %s", w.Code, w.Body.String())
			}
			var page libraryPage
			if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if page.Total != tc.total || len(page.Items) != tc.items || page.Page != tc.page || page.PageSize != tc.size || page.Items == nil {
				t.Fatalf("page=%+v", page)
			}
			for _, item := range page.Items {
				if item.Tasks == nil {
					t.Fatal("tasks must be an array")
				}
			}
			assertPrivateTaskFieldsAbsent(t, w.Body.String(), fixture.running.LeaseToken)
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("missing API headers")
			}
		})
	}
}

func TestLibraryDetailVersionAndDocumentProjection(t *testing.T) {
	fixture := newLibraryFixture(t)
	w := request(fixture.handler, "GET", libraryTarget("/api/library/papers/detail", fixture.id), "")
	if w.Code != 200 {
		t.Fatalf("response=%d %s", w.Code, w.Body.String())
	}
	var detail libraryDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Version.Identity != fixture.id || len(detail.Versions) != 2 || detail.Analysis == nil || detail.Analysis.Model != "fixture-model" || detail.Analysis.Content.SummaryZH.Text != "fixture 中文摘要" || detail.AnalysisID == 0 || len(detail.Runs) == 0 || len(detail.RelevanceHistory) != 1 {
		t.Fatalf("detail lost persisted outputs: %+v", detail)
	}
	if detail.Comparison == nil || detail.Comparison.PreviousVersion != "v1" || detail.Comparison.Status != "pending" {
		t.Fatalf("comparison=%+v", detail.Comparison)
	}
	if len(detail.Documents) != 1 {
		t.Fatalf("documents=%+v", detail.Documents)
	}
	doc := detail.Documents[0]
	if doc.ID != fixture.doc.ID || doc.Identity != fixture.id || doc.Source != fixture.doc.Source || doc.Text != fixture.doc.Text || doc.Extractor != fixture.doc.Extractor || doc.Quality != fixture.doc.Quality || !reflect.DeepEqual(doc.Issues, fixture.doc.Issues) || len(doc.Pages) != 1 || len(doc.Blocks) != len(fixture.doc.Blocks) {
		t.Fatalf("document metadata changed: %+v", doc)
	}
	var raw struct {
		Documents []struct {
			Pages  []map[string]json.RawMessage `json:"pages"`
			Blocks []map[string]json.RawMessage `json:"blocks"`
		} `json:"documents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, page := range raw.Documents[0].Pages {
		if page["text"] != nil {
			t.Fatal("full page text duplicated in detail")
		}
	}
	for _, block := range raw.Documents[0].Blocks {
		if block["text"] != nil {
			t.Fatal("full block text duplicated in detail")
		}
	}
	assertPrivateTaskFieldsAbsent(t, w.Body.String(), fixture.running.LeaseToken)
	previous := fixture.id
	previous.Version = "v1"
	w = request(fixture.handler, "GET", libraryTarget("/api/library/papers/detail", previous), "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Version.Version != "v1" || detail.Analysis != nil || len(detail.Documents) != 0 || detail.Comparison.Status != "not_applicable" {
		t.Fatalf("wrong version detail: %+v", detail)
	}
}

func TestLibraryEvidenceUsesPersistedDocumentsAndVerifiesDisk(t *testing.T) {
	fixture := newLibraryFixture(t)
	for _, handler := range []http.Handler{fixture.handler, fixture.verifiedHandler} {
		for _, want := range fixture.doc.Blocks {
			w := request(handler, "GET", evidenceTarget(fixture.id, fixture.doc.ID, want.ID), "")
			if w.Code != 200 {
				t.Fatalf("response=%d %s", w.Code, w.Body.String())
			}
			var block library.Block
			if err := json.Unmarshal(w.Body.Bytes(), &block); err != nil {
				t.Fatal(err)
			}
			if block != want {
				t.Fatalf("block=%+v want=%+v", block, want)
			}
		}
	}
	// A valid disk document must still be attached to this exact version in SQLite.
	unattached := library.Identity{Source: "arxiv", PaperID: "2610.00004", Version: "v1"}
	diskOnly, err := fixture.repository.SaveFixture(unattached, []string{strings.Repeat("synthetic fixture text for evidence ", 5)})
	if err != nil {
		t.Fatal(err)
	}
	w := request(fixture.verifiedHandler, "GET", evidenceTarget(unattached, diskOnly.ID, diskOnly.Blocks[0].ID), "")
	if w.Code != 404 {
		t.Fatalf("unattached disk document exposed: %d %s", w.Code, w.Body.String())
	}
	// Previous-version documents retained by a comparison task stay addressable by their own identity.
	previous := fixture.id
	previous.Version = "v1"
	previousDoc, err := fixture.repository.SaveFixture(previous, []string{strings.Repeat("previous version persisted reference document ", 5)})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.SaveReferenceDocument(context.Background(), fixture.running, previousDoc, time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	w = request(fixture.verifiedHandler, "GET", evidenceTarget(previous, previousDoc.ID, previousDoc.Blocks[0].ID), "")
	if w.Code != 200 {
		t.Fatalf("persisted previous-version document unavailable: %d %s", w.Code, w.Body.String())
	}
	before, err := fixture.store.GetTask(context.Background(), fixture.running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.repository.Root, filepath.FromSlash(fixture.doc.Text.Path)), []byte("corrupted-private-artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	w = request(fixture.verifiedHandler, "GET", evidenceTarget(fixture.id, fixture.doc.ID, fixture.doc.Blocks[0].ID), "")
	if w.Code != 503 || w.Body.String() != "{\"error\":\"service unavailable\"}\n" {
		t.Fatalf("corrupt disk evidence=%d %s", w.Code, w.Body.String())
	}
	after, err := fixture.store.GetTask(context.Background(), fixture.running.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("evidence request changed task: %+v %+v %v", before, after, err)
	}
}

func TestLibraryEvidenceRejectsCorruptedPersistedBlock(t *testing.T) {
	fixture := newLibraryFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	corrupt := fixture.doc
	corrupt.Blocks = append([]library.Block{}, fixture.doc.Blocks...)
	corrupt.Blocks[0].SHA256 = strings.Repeat("0", 64)
	if err := fixture.store.EnqueueTask(ctx, fixture.id, "document", 1); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.store.ClaimTask(ctx, "document", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.CompleteTask(ctx, task, library.Completion{Document: &corrupt}, now); err != nil {
		t.Fatal(err)
	}
	for _, handler := range []http.Handler{fixture.handler, fixture.verifiedHandler} {
		w := request(handler, "GET", evidenceTarget(fixture.id, corrupt.ID, corrupt.Blocks[0].ID), "")
		if w.Code != 503 || w.Body.String() != "{\"error\":\"service unavailable\"}\n" {
			t.Fatalf("corrupt persisted block exposed: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestLibraryStatusRetainsCompletenessAndGapWithoutChangingTasks(t *testing.T) {
	fixture := newLibraryFixture(t)
	before, err := fixture.store.GetTask(context.Background(), fixture.running.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/api/library/status", "/api/library/papers?relevance=all", libraryTarget("/api/library/papers/detail", fixture.id)} {
		w := request(fixture.handler, "GET", target, "")
		if w.Code != 200 {
			t.Fatalf("response=%d %s", w.Code, w.Body.String())
		}
		assertPrivateTaskFieldsAbsent(t, w.Body.String(), fixture.running.LeaseToken)
		if target == "/api/library/status" {
			var status library.Status
			if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.Versions != 4 || status.Tasks["running"] != 1 || len(status.Batches) != 1 || status.Batches[0].Completeness != "partial" || status.Batches[0].Reason != "fixture category incomplete" || status.Batches[0].Versions != nil || status.Batches[0].Events != nil || len(status.Gaps) != 1 {
				t.Fatalf("status=%+v", status)
			}
		}
	}
	after, err := fixture.store.GetTask(context.Background(), fixture.running.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("read API changed task: %+v %+v %v", before, after, err)
	}
}

func TestLibraryAPIValidationNotFoundAndMethods(t *testing.T) {
	fixture := newLibraryFixture(t)
	for _, target := range []string{
		"/api/library/papers?page=0", "/api/library/papers?page=oops", "/api/library/papers?pageSize=101", "/api/library/papers?pageSize=", "/api/library/papers?page=9223372036854775807&pageSize=2",
		"/api/library/papers?batch=2026-02-30", "/api/library/papers?batch=%2F2026-10-02", "/api/library/papers?topic=invalid", "/api/library/papers?relevance=invalid", "/api/library/papers?status=invalid", "/api/library/papers?status=invalid%3Arunning",
		"/api/library/papers/detail", "/api/library/papers/detail?id=hep-th%2F9901001", "/api/library/papers/detail?id=bad&version=v1", "/api/library/papers/detail?id=2610.00001&version=1", "/api/library/papers/detail?id=2610.00001&version=v0",
		"/api/library/evidence", "/api/library/evidence?id=2610.00001&version=v1", "/api/library/evidence?id=2610.00001&version=v1&document=x", "/api/library/evidence?id=2610.00001&version=v1&block=x", "/api/library/evidence?id=bad&version=v1&document=x&block=x",
	} {
		w := request(fixture.handler, "GET", target, "")
		if w.Code != 400 || w.Body.String() != "{\"error\":\"invalid query parameters\"}\n" {
			t.Errorf("target=%s response=%d %s", target, w.Code, w.Body.String())
		}
	}
	otherVersion := fixture.id
	otherVersion.Version = "v1"
	otherPaper := library.Identity{Source: "arxiv", PaperID: "2610.99999", Version: "v2"}
	for _, target := range []string{
		libraryTarget("/api/library/papers/detail", otherPaper),
		evidenceTarget(otherVersion, fixture.doc.ID, fixture.doc.Blocks[0].ID),
		evidenceTarget(otherPaper, fixture.doc.ID, fixture.doc.Blocks[0].ID),
		evidenceTarget(fixture.id, "unknown-document", fixture.doc.Blocks[0].ID),
		evidenceTarget(fixture.id, fixture.doc.ID, "unknown-block"),
		"/api/library/unknown", "/api/library/papers/extra",
	} {
		w := request(fixture.handler, "GET", target, "text/html")
		if w.Code != 404 || w.Body.String() != "{\"error\":\"not found\"}\n" {
			t.Errorf("target=%s response=%d %s", target, w.Code, w.Body.String())
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE", "HEAD"} {
		for _, target := range []string{"/api/library/papers", "/api/library/papers/detail", "/api/library/status", "/api/library/evidence"} {
			w := request(fixture.handler, method, target, "")
			if w.Code != 405 || w.Header().Get("Allow") != "GET" {
				t.Errorf("method=%s target=%s response=%d", method, target, w.Code)
			}
		}
	}
	fixture.store.Close()
	for _, target := range []string{"/api/library/papers", libraryTarget("/api/library/papers/detail", fixture.id), "/api/library/status", evidenceTarget(fixture.id, fixture.doc.ID, fixture.doc.Blocks[0].ID)} {
		w := request(fixture.handler, "GET", target, "")
		if w.Code != 503 || w.Body.String() != "{\"error\":\"service unavailable\"}\n" {
			t.Errorf("closed DB target=%s response=%d %s", target, w.Code, w.Body.String())
		}
	}
}
