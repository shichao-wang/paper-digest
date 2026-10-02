package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shichao-wang/paper-digest/internal/archive"
	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/document"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/state"
)

func demoTestConfig(t *testing.T) config.Config {
	t.Helper()
	// macOS's temporary path may start at the /var symlink. Seed destinations
	// deliberately require resolved local paths with no symlink components.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	cfg.Database.Path = filepath.Join(root, "demo.db")
	cfg.Library.DocumentDir = filepath.Join(root, "documents")
	cfg.Topics = []config.Topic{{ID: "demo"}}
	return cfg
}

type demoRejectNetwork struct{ calls int }

func (r *demoRejectNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	return nil, errors.New("demo tests prohibit network access")
}

func TestSeedDemoOfflineLibraryAndLegacy(t *testing.T) {
	ctx := context.Background()
	cfg := demoTestConfig(t)
	transport := &demoRejectNetwork{}
	oldTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	if err := seedDemo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if transport.calls != 0 {
		t.Fatalf("demo made %d network requests", transport.calls)
	}
	store, err := state.Open(cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	validateDemoLibrary(t, ctx, store, &document.Repository{Root: cfg.Library.DocumentDir})
	validateDemoLegacy(t, ctx, store, cfg.Topics[0].ID)

	before, err := os.ReadFile(cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedDemo(ctx, cfg); err == nil {
		t.Fatal("second seed must refuse existing destinations")
	}
	after, err := os.ReadFile(cfg.Database.Path)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected seed changed the database: %v", err)
	}
}

func validateDemoLibrary(t *testing.T, ctx context.Context, store *state.Store, repo *document.Repository) {
	t.Helper()
	status, err := store.LibraryStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Versions < 25 {
		t.Fatalf("need at least 25 synthetic versions, got %d", status.Versions)
	}
	for _, taskStatus := range []string{"succeeded", "paused", "blocked", "queued", "retry_wait"} {
		if status.Tasks[taskStatus] == 0 {
			t.Errorf("missing %s task demo: %#v", taskStatus, status.Tasks)
		}
	}
	batchStatus := map[string]bool{}
	for _, batch := range status.Batches {
		batchStatus[batch.Completeness] = true
		if !strings.Contains(batch.Reason, "合成") {
			t.Errorf("batch does not declare synthetic provenance: %#v", batch)
		}
	}
	if !batchStatus["complete"] || !batchStatus["incomplete"] || len(status.Gaps) == 0 {
		t.Fatalf("missing complete/incomplete batch or gap: %#v", status)
	}
	first, err := store.BrowseLibrary(ctx, library.Query{Relevance: "all"})
	if err != nil || len(first.Items) != 20 || first.Total < 25 {
		t.Fatalf("pagination first page: %#v, %v", first, err)
	}
	second, err := store.BrowseLibrary(ctx, library.Query{Relevance: "all", Page: 2})
	if err != nil || len(second.Items) == 0 {
		t.Fatalf("pagination second page: %#v, %v", second, err)
	}
	for _, rel := range []string{"direct", "unrelated", "uncertain"} {
		page, err := store.BrowseLibrary(ctx, library.Query{Relevance: rel})
		if err != nil || page.Total == 0 {
			t.Fatalf("missing relevance %s: %#v, %v", rel, page, err)
		}
		for _, item := range page.Items {
			if item.Relevance == nil || item.Relevance.Rationale == "" {
				t.Errorf("missing relevance rationale: %#v", item)
			}
		}
	}
	all, err := store.BrowseLibrary(ctx, library.Query{Relevance: "all", PageSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	comparisons := map[string]bool{}
	analyses := 0
	for _, item := range all.Items {
		if item.Version.Origin != "synthetic_demo" || !strings.Contains(item.Version.Title, demoLabel) || item.Version.MetadataVerified {
			t.Errorf("version lacks honest synthetic marking: %#v", item.Version)
		}
		detail, err := store.LibraryDetail(ctx, item.Version.Identity)
		if err != nil {
			t.Fatal(err)
		}
		comparisons[detail.Comparison.Status] = true
		if len(detail.Documents) == 0 {
			t.Fatalf("missing full document for %s", item.Version.Key())
		}
		loaded := map[string]library.Document{}
		for _, doc := range detail.Documents {
			full, err := repo.Load(ctx, doc)
			if err != nil {
				t.Fatalf("load %s: %v", doc.ID, err)
			}
			if full.Source.Kind != "synthetic" || full.Source.URL != "" || full.Extractor != "synthetic-fixture-v1" || len(full.Pages) != 3 || len(full.Blocks) < 3 {
				t.Errorf("document is not complete synthetic fixture: %#v", full)
			}
			if full.Identity != item.Version.Identity {
				t.Errorf("document version mixed: %s vs %s", full.Key(), item.Version.Key())
			}
			loaded[full.ID] = full
		}
		if detail.Analysis != nil {
			analyses++
			a := detail.Analysis
			if a.PaperVersionID != item.Version.Key() || !strings.Contains(a.Model, "synthetic_demo") || len(a.DocumentHashes) != 2 {
				t.Errorf("analysis identity/provenance mismatch: %#v", a)
			}
			validateDemoContent(t, a.Content)
			validateDemoEvidence(t, a.Content.Evidence, loaded)
			for _, task := range detail.Tasks {
				if task.Stage != "analyze" {
					continue
				}
				chunks, err := store.Chunks(ctx, task)
				if err != nil || len(chunks) != len(detail.Documents[0].Blocks) {
					t.Fatalf("complete analysis must retain every read block: %d chunks, %v", len(chunks), err)
				}
				for _, chunk := range chunks {
					if !chunk.Read || chunk.DocumentHash != detail.Documents[0].Text.SHA256 {
						t.Errorf("read block/hash mismatch: %#v", chunk)
					}
					validateDemoEvidence(t, chunk.Evidence, loaded)
				}
			}
		}
		for _, run := range detail.Runs {
			if run.Requests != 0 || run.PromptTokens != 0 || run.CompletionTokens != 0 {
				t.Errorf("synthetic run must not invent model usage: %#v", run)
			}
		}
		if detail.Comparison.Status == "completed" {
			prev, ok := item.Version.Previous()
			if !ok {
				t.Fatal("v1 comparison cannot be completed")
			}
			previous, err := store.LibraryDetail(ctx, prev)
			if err != nil {
				t.Fatal(err)
			}
			for _, doc := range previous.Documents {
				loaded[doc.ID] = doc
			}
			if len(detail.Comparison.Content.Changes) == 0 || len(detail.Comparison.DocumentHashes) != 4 {
				t.Errorf("completed comparison lacks changes/hashes: %#v", detail.Comparison)
			}
			validateDemoEvidence(t, detail.Comparison.Content.Evidence, loaded)
			description := detail.Comparison.Content.Changes[0].Description
			if detail.Comparison.PreviousVersion != prev.Version || !strings.Contains(description, prev.Version+" → "+item.Version.Version) {
				t.Errorf("comparison must describe its direct predecessor: %q", description)
			}
			if item.Version.Version == "v2" && !strings.Contains(description, "由 0.420 改为 0.460") {
				t.Errorf("v1/v2 numeric change missing: %q", description)
			}
			if item.Version.Version == "v10" && (strings.Contains(description, "0.420") || strings.Contains(description, "v2") || !strings.Contains(description, "双方示例结果均为 0.460，数值保持不变，版本文本更新")) {
				t.Errorf("v9/v10 must retain 0.460 and describe only their text update: %q", description)
			}
		}
	}
	if analyses <= 20 {
		t.Errorf("need >20 complete analyses, got %d", analyses)
	}
	for _, status := range []string{"completed", "pending", "blocked", "not_applicable"} {
		if !comparisons[status] {
			t.Errorf("missing comparison %s", status)
		}
	}
	v1, err := store.LibraryDetail(ctx, library.Identity{Source: "arxiv", PaperID: "2609.00001", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := store.LibraryDetail(ctx, library.Identity{Source: "arxiv", PaperID: "2609.00001", Version: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, id := range v1.Versions {
		got = append(got, id.Version)
	}
	if !reflect.DeepEqual(got, []string{"v1", "v2", "v9", "v10"}) {
		t.Errorf("versions must sort numerically: %v", got)
	}
	if v1.AnalysisID == v2.AnalysisID || v1.Documents[0].ID == v2.Documents[0].ID || v1.Analysis.Content.Results[0].Value != "0.420" || v2.Analysis.Content.Results[0].Value != "0.460" {
		t.Error("v1/v2 must retain independent documents, analyses and numeric results")
	}
}

func validateDemoEvidence(t *testing.T, evidence []library.Evidence, docs map[string]library.Document) {
	t.Helper()
	for _, e := range evidence {
		d, ok := docs[e.DocumentID]
		if !ok || d.Version != e.Version || e.Page < 1 || e.Page > len(d.Pages) || e.Section == "" {
			t.Fatalf("evidence document/version/page missing: %#v", e)
		}
		text := d.Pages[e.Page-1].Text
		if e.Start < 0 || e.End <= e.Start || e.End > len(text) || text[e.Start:e.End] != e.Quote {
			t.Fatalf("quote does not match complete source text at exact byte range: %#v", e)
		}
		found := false
		for _, block := range d.Blocks {
			if block.ID == e.BlockID && e.Page == block.Page && e.Start >= block.Start && e.End <= block.End {
				found = true
			}
		}
		if !found {
			t.Errorf("evidence does not locate a saved complete block: %#v", e)
		}
	}
}

func validateDemoContent(t *testing.T, c library.AnalysisContent) {
	t.Helper()
	if c.TitleZH == "" || c.Problem == nil || c.Motivation == nil || c.Method == nil || c.SummaryZH.Text == "" || c.Relevance.Validate() != nil || len(c.Relevance.EvidenceIDs) == 0 {
		t.Fatalf("missing required readable analysis fields: %#v", c)
	}
	for name, claims := range map[string][]library.Claim{"作者关键词": c.AuthorKeywords, "资源说明": c.ResourceLinks, "贡献": c.Contributions, "数据集": c.Datasets, "基线": c.Baselines, "局限": c.Limitations, "阅读者评估": c.AgentAssessment, "关键点": c.KeyPoints} {
		if len(claims) == 0 {
			t.Errorf("missing analysis field %s", name)
		}
	}
	if len(c.ExtractedKeywords) == 0 || len(c.Results) == 0 || len(c.AuthorClaims) == 0 || len(c.AgentInferences) == 0 || len(c.MissingFields) == 0 || len(c.Evidence) == 0 {
		t.Fatal("missing structured analysis fields")
	}
	ev := map[string]library.Evidence{}
	for _, e := range c.Evidence {
		ev[e.ID] = e
	}
	// All claims, including nested fields, must reference retained evidence.
	var visit func(reflect.Value)
	visit = func(v reflect.Value) {
		if v.Kind() == reflect.Pointer {
			if !v.IsNil() {
				visit(v.Elem())
			}
			return
		}
		if v.Type() == reflect.TypeOf(library.Claim{}) {
			claim := v.Interface().(library.Claim)
			if claim.Text == "" || len(claim.EvidenceIDs) == 0 {
				t.Errorf("empty or ungrounded claim: %#v", claim)
			}
			for _, id := range claim.EvidenceIDs {
				if ev[id].ID == "" {
					t.Errorf("unknown evidence ref %s", id)
				}
			}
			return
		}
		switch v.Kind() {
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				visit(v.Field(i))
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				visit(v.Index(i))
			}
		}
	}
	visit(reflect.ValueOf(c))
	for _, result := range c.Results {
		if result.Dataset == nil || result.Method == nil || result.Baseline == nil || result.Unit == nil || result.Setting == nil || len(result.EvidenceIDs) == 0 {
			t.Fatal("demo numeric result must demonstrate all context fields")
		}
		quote := ev[result.EvidenceIDs[0]].Quote
		for _, value := range []string{result.Value, result.Metric, *result.Dataset, *result.Method, *result.Baseline, *result.Unit, *result.Setting} {
			if !strings.Contains(quote, value) {
				t.Errorf("numeric context %q absent from source quote", value)
			}
		}
	}
}

func validateDemoLegacy(t *testing.T, ctx context.Context, store *state.Store, topic string) {
	t.Helper()
	page, err := store.BrowseDigests(ctx, topic, state.PageQuery{})
	if err != nil || page.Total != 3 {
		t.Fatalf("old digest history: %#v, %v", page, err)
	}
	for n, date := range []string{"2026-09-27", "2026-09-28", "2026-09-29"} {
		detail, err := store.DigestDetail(ctx, topic, date)
		if err != nil {
			t.Fatal(err)
		}
		wantStatus := "sent"
		if n == 1 {
			wantStatus = "unknown"
		}
		if detail.Status != wantStatus || !strings.Contains(detail.Message, demoLabel) || !strings.Contains(detail.Message, "未真实发送") {
			t.Errorf("old demo status/message mismatch: %#v", detail)
		}
		if n == 2 {
			if detail.PaperCount != 0 || detail.SummaryCount != 0 || len(detail.Items) != 0 {
				t.Errorf("empty digest has papers: %#v", detail)
			}
			continue
		}
		if detail.PaperCount != 1 || detail.SummaryCount != 1 || detail.Items[0].Summary == nil || detail.Items[0].Summary.Model != "synthetic_demo" {
			t.Errorf("old digest summary missing: %#v", detail)
		}
		completed, err := store.Completed(ctx, topic, date)
		if err != nil || len(completed) != 1 {
			t.Fatalf("old Completed interface unavailable: %#v %v", completed, err)
		}
		seen, err := store.Seen(ctx, topic, detail.Items[0].ID)
		if err != nil || !seen { // same stable ID was simulated as sent on September 27
			t.Fatalf("old recommendation history unavailable: %v %v", seen, err)
		}
	}
}

func TestSeedDemoBackupRestore(t *testing.T) {
	ctx := context.Background()
	cfg := demoTestConfig(t)
	if err := seedDemo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := filepath.Dir(cfg.Database.Path)
	backup, restored := filepath.Join(root, "backup"), filepath.Join(root, "restored")
	if err := archive.Backup(ctx, store, cfg.Library.DocumentDir, backup); err != nil {
		t.Fatalf("self-contained demo backup: %v", err)
	}
	if err := archive.Restore(ctx, backup, restored); err != nil {
		t.Fatalf("restore demo backup: %v", err)
	}
	// Restoration must work using only the archive after source artifacts vanish.
	if err := os.RemoveAll(cfg.Library.DocumentDir); err != nil {
		t.Fatal(err)
	}
	restoredStore, err := state.Open(filepath.Join(restored, "database.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restoredStore.Close()
	validateDemoLibrary(t, ctx, restoredStore, &document.Repository{Root: filepath.Join(restored, "artifacts")})
	validateDemoLegacy(t, ctx, restoredStore, cfg.Topics[0].ID)
}

func TestSeedDemoRejectsUnsafeConfigurationBeforeCreating(t *testing.T) {
	cases := map[string]func(*config.Config){
		"delivery":                 func(c *config.Config) { c.Delivery.Enabled = true },
		"collect":                  func(c *config.Config) { c.Library.CollectEnabled = true },
		"process":                  func(c *config.Config) { c.Library.ProcessEnabled = true },
		"api key":                  func(c *config.Config) { c.Anthropic.APIKey = "synthetic-secret" },
		"webhook":                  func(c *config.Config) { c.Topics[0].WebhookURL = "https://example.invalid/hook" },
		"another webhook":          func(c *config.Config) { c.Topics = append(c.Topics, config.Topic{ID: "other", WebhookURL: " "}) },
		"no topic":                 func(c *config.Config) { c.Topics = nil },
		"memory":                   func(c *config.Config) { c.Database.Path = ":memory:" },
		"sqlite URI":               func(c *config.Config) { c.Database.Path = "file:demo.db?mode=rw" },
		"empty DB":                 func(c *config.Config) { c.Database.Path = "" },
		"empty documents":          func(c *config.Config) { c.Library.DocumentDir = "" },
		"same paths":               func(c *config.Config) { c.Library.DocumentDir = c.Database.Path },
		"database under documents": func(c *config.Config) { c.Database.Path = filepath.Join(c.Library.DocumentDir, "demo.db") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := demoTestConfig(t)
			db, docs := cfg.Database.Path, cfg.Library.DocumentDir
			mutate(&cfg)
			if err := seedDemo(context.Background(), cfg); err == nil {
				t.Fatal("unsafe config was accepted")
			}
			for _, path := range []string{db, docs} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("rejected config created %s: %v", path, err)
				}
			}
		})
	}
}

func TestSeedDemoNeverOverwritesExistingPaths(t *testing.T) {
	for _, name := range []string{"database", "documents", "WAL", "symlink"} {
		t.Run(name, func(t *testing.T) {
			cfg := demoTestConfig(t)
			path := cfg.Database.Path
			sentinel := []byte("existing content must survive")
			switch name {
			case "documents":
				if err := os.Mkdir(cfg.Library.DocumentDir, 0700); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(cfg.Library.DocumentDir, "sentinel")
			case "WAL":
				path += "-wal"
			case "symlink":
				path = filepath.Join(filepath.Dir(cfg.Database.Path), "existing")
				if err := os.WriteFile(path, sentinel, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path, cfg.Database.Path); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, sentinel, 0600); err != nil {
				t.Fatal(err)
			}
			if err := seedDemo(context.Background(), cfg); err == nil {
				t.Fatal("existing target was accepted")
			}
			got, err := os.ReadFile(path)
			if err != nil || !reflect.DeepEqual(got, sentinel) {
				t.Fatalf("seed modified existing bytes: %q %v", got, err)
			}
		})
	}
}

func TestSeedDemoCancelledBeforeCreating(t *testing.T) {
	cfg := demoTestConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := seedDemo(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled seed: %v", err)
	}
	if _, err := os.Lstat(cfg.Database.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled seed created DB: %v", err)
	}
}
