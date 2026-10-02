package papers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/library"
)

func sourceFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "validation", "arxiv", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}
func syntheticFeed(entries string) string {
	return `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:arxiv="http://arxiv.org/schemas/atom" xmlns:dc="http://purl.org/dc/elements/1.1/"><updated>2026-10-02T04:00:00Z</updated>` + entries + `</feed>`
}
func syntheticEntry(id, kind, date string) string {
	published := ""
	if date != "" {
		published = `<published>` + date + `T00:00:00-04:00</published>`
	}
	return `<entry><id>oai:arXiv.org:` + id + `</id><title>Example &amp; result</title><updated>2026-10-02T04:00:03Z</updated>` + published + `<summary>arXiv:` + id + ` Announce Type: ` + kind + `&#10;Abstract: Research result.</summary><category term="cs.IR"/><arxiv:announce_type>` + kind + `</arxiv:announce_type><dc:creator>Alice Smith, Bob Jones</dc:creator></entry>`
}
func syntheticListing(date string, total int, sections map[string][]string, declared map[string]int) string {
	var out strings.Builder
	if date != "" {
		d, _ := time.Parse("2006-01-02", date)
		fmt.Fprintf(&out, "<h3>Showing new listings for %s</h3>", d.Format("Monday, 2 January 2006"))
	}
	fmt.Fprintf(&out, "Total of %d entries", total)
	for _, key := range []string{"new", "cross", "replacement"} {
		title := map[string]string{"new": "New", "cross": "Cross", "replacement": "Replacement"}[key]
		ids := sections[key]
		n := len(ids)
		if declared != nil {
			n = declared[key]
		}
		fmt.Fprintf(&out, "<h3>%s submissions (showing %d of %d entries)</h3>", title, len(ids), n)
		for _, id := range ids {
			fmt.Fprintf(&out, `<dt><a href="/abs/%s">arXiv:%s</a> [<a href="/pdf/%s">pdf</a>]</dt>`, id, id, id)
		}
	}
	return out.String()
}
func sourceServer(t *testing.T, feed, list string) (Source, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/atom/cs.IR":
			fmt.Fprint(w, feed)
		case "/list/cs.IR/new":
			if r.URL.Query().Get("show") != "1000" {
				t.Errorf("show must be 1000")
			}
			fmt.Fprint(w, list)
		default:
			http.NotFound(w, r)
		}
	}))
	source := Source{Client: server.Client(), FeedBase: server.URL + "/atom", ListBase: server.URL + "/list", APIBase: server.URL + "/api/query", ArtifactRoot: t.TempDir(), Now: func() time.Time { return time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC) }}
	return source, server
}
func checkArtifacts(t *testing.T, root string, artifacts []library.Artifact) {
	t.Helper()
	for _, artifact := range artifacts {
		if artifact.Path == "" || filepath.IsAbs(artifact.Path) || strings.Contains(artifact.Path, "..") || artifact.URL == "" || artifact.CapturedAt == "" {
			t.Fatalf("bad artifact: %#v", artifact)
		}
		body, err := os.ReadFile(filepath.Join(root, artifact.Path))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != artifact.SHA256 {
			t.Fatal("artifact hash mismatch")
		}
	}
}

func TestAnnouncementsRealEntryExtracts(t *testing.T) {
	real := sourceFixture(t, "real-entries.xml")
	source, server := sourceServer(t, string(real), string(sourceFixture(t, "real-cs.IR-list-extract.html")))
	defer server.Close()
	batches, err := source.FetchAnnouncements(context.Background(), []string{"cs.IR"})
	if err != nil {
		t.Fatal(err)
	}
	batch := batches[0]
	// These extracts deliberately mix cross-category duplicates and are not a
	// full batch. They must retain all four observed announcement event types.
	if batch.Completeness != "incomplete" || len(batch.Events) != 5 || len(batch.Versions) != 5 {
		t.Fatalf("unexpected real extract batch: %#v", batch)
	}
	for _, kind := range []string{"new", "cross", "replace", "replace-cross"} {
		if batch.Counts[kind] == 0 {
			t.Errorf("missing event type %s", kind)
		}
	}
	if batch.Date != "2026-10-01" || batch.BuiltAt != "2026-10-01T04:00:07.369047Z" {
		t.Fatalf("dates = %s / %s", batch.Date, batch.BuiltAt)
	}
	if !strings.Contains(batch.Reason, "duplicate stable ID") || !strings.Contains(batch.Reason, "stable IDs differ") {
		t.Fatalf("reason = %s", batch.Reason)
	}
	version := batch.Versions[0]
	if version.PaperID != "2609.38353" || version.Version != "v1" || len(version.Authors) != 3 || version.Authors[0] != "Yu-Su Chen" {
		t.Fatalf("version/authors = %#v", version)
	}
	if strings.HasPrefix(version.Abstract, "arXiv:") || !strings.HasPrefix(version.Abstract, "Long-term memory") {
		t.Fatalf("abstract prefix not stripped: %s", version.Abstract)
	}
	if version.PublishedAt != "" || version.UpdatedAt != "" || version.MetadataVerified {
		t.Fatal("RSS dates were treated as submission metadata")
	}
	if len(batch.Artifacts) != 2 {
		t.Fatalf("artifacts = %#v", batch.Artifacts)
	}
	checkArtifacts(t, source.ArtifactRoot, batch.Artifacts)
	listing := parseOfficialListing(sourceFixture(t, "real-cs.IR-list-extract.html"))
	if listing.Total != 41 || listing.Date != "2026-10-01" || len(listing.Reasons) != 0 || len(listing.Sections["new"].IDs) != 16 || len(listing.Sections["cross"].IDs) != 13 || len(listing.Sections["replacement"].IDs) != 12 {
		t.Fatalf("real official list = %#v", listing)
	}
}

func TestAnnouncementsFourTypesCompleteAndArtifactsImmutable(t *testing.T) {
	feed := syntheticFeed(syntheticEntry("2610.00001v1", "new", "2026-10-02") + syntheticEntry("2610.00002v2", "cross", "2026-10-02") + syntheticEntry("2609.00003v3", "replace", "2026-10-02") + syntheticEntry("2609.00004v2", "replace-cross", "2026-10-02"))
	list := syntheticListing("2026-10-02", 4, map[string][]string{"new": {"2610.00001"}, "cross": {"2610.00002"}, "replacement": {"2609.00003", "2609.00004"}}, nil)
	source, server := sourceServer(t, feed, list)
	defer server.Close()
	for n := 0; n < 2; n++ {
		batches, err := source.FetchAnnouncements(context.Background(), []string{"cs.IR"})
		if err != nil {
			t.Fatal(err)
		}
		batch := batches[0]
		if batch.Completeness != "complete" || batch.Reason != "" || len(batch.Events) != 4 {
			t.Fatalf("batch = %#v", batch)
		}
		for _, v := range batch.Versions {
			if v.AnnouncementDate != "2026-10-02" || v.PublishedAt != "" || len(v.Authors) != 2 {
				t.Fatalf("version = %#v", v)
			}
		}
		checkArtifacts(t, source.ArtifactRoot, batch.Artifacts)
		for _, a := range batch.Artifacts {
			info, _ := os.Stat(filepath.Join(source.ArtifactRoot, a.Path))
			if info.Mode().Perm()&0222 != 0 {
				t.Fatal("artifact is writable")
			}
		}
	}
	entries, err := filepath.Glob(filepath.Join(source.ArtifactRoot, "artifacts", "arxiv", "*", "*.bin"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("immutable artifacts duplicated: %v %v", entries, err)
	}
}

func TestAnnouncementsEmptyDateAndIncompleteEvidence(t *testing.T) {
	cases := []struct {
		name, feed, list, wantDate, reason string
		complete                           bool
		events                             int
	}{
		{name: "empty with zero list", feed: string(sourceFixture(t, "synthetic-empty.xml")), list: syntheticListing("2026-10-01", 0, nil, nil), wantDate: "2026-10-01", complete: true},
		{name: "empty missing date", feed: string(sourceFixture(t, "synthetic-empty.xml")), list: syntheticListing("", 0, nil, nil), reason: "no announcement date"},
		{name: "empty nonzero list", feed: string(sourceFixture(t, "synthetic-empty.xml")), list: syntheticListing("2026-10-01", 1, map[string][]string{"new": {"2610.00001"}}, nil), wantDate: "2026-10-01", reason: "empty Atom lacks"},
		{name: "missing entry date from list", feed: syntheticFeed(syntheticEntry("2610.00001v1", "new", "")), list: syntheticListing("2026-10-02", 1, map[string][]string{"new": {"2610.00001"}}, nil), wantDate: "2026-10-02", complete: true, events: 1},
		{name: "mismatched date", feed: syntheticFeed(syntheticEntry("2610.00001v1", "new", "2026-10-01")), list: syntheticListing("2026-10-02", 1, map[string][]string{"new": {"2610.00001"}}, nil), wantDate: "2026-10-01", reason: "dates differ", events: 1},
		{name: "multiple dates", feed: syntheticFeed(syntheticEntry("2610.00001v1", "new", "2026-10-01") + syntheticEntry("2610.00002v1", "new", "2026-10-02")), list: syntheticListing("2026-10-02", 2, map[string][]string{"new": {"2610.00001", "2610.00002"}}, nil), reason: "different announcement dates", events: 2},
		{name: "same ID different versions", feed: syntheticFeed(syntheticEntry("2610.00001v1", "new", "2026-10-02") + syntheticEntry("2610.00001v2", "replace", "2026-10-02")), list: syntheticListing("2026-10-02", 2, map[string][]string{"new": {"2610.00001"}, "replacement": {"2610.00001"}}, nil), wantDate: "2026-10-02", reason: "ambiguous duplicate stable ID", events: 2},
		{name: "unknown type", feed: syntheticFeed(syntheticEntry("2610.00001v1", "other", "2026-10-02")), list: syntheticListing("2026-10-02", 1, map[string][]string{"new": {"2610.00001"}}, nil), wantDate: "2026-10-02", reason: "unknown announcement type", events: 1},
		{name: "unknown list structure", feed: syntheticFeed(syntheticEntry("2610.00001v1", "new", "2026-10-02")), list: `<h3>Showing new listings for Friday, 2 October 2026</h3>Total of 1 entries<h3>Changed layout</h3><dt><a href="/abs/2610.00001">id</a></dt>`, wantDate: "2026-10-02", reason: "unknown official three-group structure", events: 1},
		{name: "wrong group", feed: syntheticFeed(syntheticEntry("2610.00001v1", "cross", "2026-10-02")), list: syntheticListing("2026-10-02", 1, map[string][]string{"new": {"2610.00001"}}, nil), wantDate: "2026-10-02", reason: "stable IDs differ", events: 1},
		{name: "missing namespace", feed: strings.ReplaceAll(syntheticFeed(syntheticEntry("2610.00001v1", "new", "2026-10-02")), "http://arxiv.org/schemas/atom", "http://arxiv.org/schemas/atom/"), list: syntheticListing("2026-10-02", 1, map[string][]string{"new": {"2610.00001"}}, nil), wantDate: "2026-10-02", reason: "unknown announcement type", events: 1},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			source, server := sourceServer(t, tt.feed, tt.list)
			defer server.Close()
			batches, err := source.FetchAnnouncements(context.Background(), []string{"cs.IR"})
			if err != nil {
				t.Fatal(err)
			}
			batch := batches[0]
			if (batch.Completeness == "complete") != tt.complete || batch.Date != tt.wantDate || len(batch.Events) != tt.events {
				t.Fatalf("batch = %#v", batch)
			}
			if tt.reason != "" && !strings.Contains(batch.Reason, tt.reason) {
				t.Fatalf("reason = %s, want %s", batch.Reason, tt.reason)
			}
			checkArtifacts(t, source.ArtifactRoot, batch.Artifacts)
		})
	}
}

func TestAnnouncementsTruncatedKeepsCapturedCandidates(t *testing.T) {
	feed := strings.TrimSuffix(syntheticFeed(syntheticEntry("2610.00001v1", "new", "2026-10-02")), "</feed>") + "<entry><id>"
	source, server := sourceServer(t, feed, syntheticListing("2026-10-02", 1, map[string][]string{"new": {"2610.00001"}}, nil))
	defer server.Close()
	batches, err := source.FetchAnnouncements(context.Background(), []string{"cs.IR"})
	if err != nil {
		t.Fatal(err)
	}
	if batches[0].Completeness != "incomplete" || len(batches[0].Events) != 1 || !strings.Contains(batches[0].Reason, "Atom structure") {
		t.Fatalf("batch = %#v", batches[0])
	}
	checkArtifacts(t, source.ArtifactRoot, batches[0].Artifacts)
	if _, err := decodeSourceFeed(sourceFixture(t, "synthetic-truncated.xml")); err == nil {
		t.Fatal("real truncated fixture accepted")
	}
}

func TestAnnouncementsPaginationCapturesAllAndDetectsChanges(t *testing.T) {
	for _, mode := range []string{"complete", "repeated", "different-date", "no-progress"} {
		t.Run(mode, func(t *testing.T) {
			feed := syntheticFeed(syntheticEntry("2610.00001v1", "new", "2026-10-02") + syntheticEntry("2610.00002v1", "new", "2026-10-02"))
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/atom/") {
					fmt.Fprint(w, feed)
					return
				}
				requests++
				skip := r.URL.Query().Get("skip")
				if r.URL.Query().Get("show") != "1000" {
					t.Errorf("show=%s", r.URL.Query().Get("show"))
				}
				ids := []string{"2610.00001"}
				date := "2026-10-02"
				if skip != "0" {
					if skip != "1" {
						t.Errorf("skip=%s", skip)
					}
					ids = []string{"2610.00002"}
					switch mode {
					case "repeated":
						ids = []string{"2610.00001"}
					case "different-date":
						date = "2026-10-01"
					case "no-progress":
						ids = nil
					}
				}
				fmt.Fprint(w, syntheticListing(date, 2, map[string][]string{"new": ids}, map[string]int{"new": 2}))
			}))
			defer server.Close()
			source := Source{Client: server.Client(), FeedBase: server.URL + "/atom", ListBase: server.URL + "/list", ArtifactRoot: t.TempDir()}
			batches, err := source.FetchAnnouncements(context.Background(), []string{"cs.IR"})
			if err != nil {
				t.Fatal(err)
			}
			if requests != 2 || (batches[0].Completeness == "complete") != (mode == "complete") || len(batches[0].Artifacts) != 3 {
				t.Fatalf("mode %s requests=%d batch=%#v", mode, requests, batches[0])
			}
			checkArtifacts(t, source.ArtifactRoot, batches[0].Artifacts)
		})
	}
}

func TestAnnouncementsAllFiveCategoriesAndNoBusinessCap(t *testing.T) {
	var entries strings.Builder
	ids := []string{}
	for i := 1; i <= 1105; i++ {
		id := fmt.Sprintf("2610.%05dv1", i)
		entries.WriteString(syntheticEntry(id, "new", "2026-10-02"))
		ids = append(ids, strings.TrimSuffix(id, "v1"))
	}
	feed := syntheticFeed(entries.String())
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		category := parts[2]
		seen[category]++
		if parts[1] == "atom" {
			fmt.Fprint(w, strings.ReplaceAll(feed, `<category term="cs.IR"/>`, `<category term="`+category+`"/>`))
			return
		}
		fmt.Fprint(w, syntheticListing("2026-10-02", len(ids), map[string][]string{"new": ids}, nil))
	}))
	defer server.Close()
	source := Source{Client: server.Client(), FeedBase: server.URL + "/atom", ListBase: server.URL + "/list", ArtifactRoot: t.TempDir()}
	categories := []string{"cs.IR", "cs.LG", "cs.AI", "cs.CL", "stat.ML"}
	batches, err := source.FetchAnnouncements(context.Background(), categories)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 5 {
		t.Fatalf("got %d categories", len(batches))
	}
	for _, batch := range batches {
		if batch.Completeness != "complete" || len(batch.Events) != 1105 || seen[batch.Category] != 2 {
			t.Fatalf("category %s complete=%s count=%d requests=%d", batch.Category, batch.Completeness, len(batch.Events), seen[batch.Category])
		}
	}
}

func TestVersionMetadataExactIdentityAndSubmissionDates(t *testing.T) {
	for _, mode := range []string{"correct", "wrong-version", "wrong-id", "unversioned", "missing-updated", "missing-primary", "bad-primary", "multiple"} {
		t.Run(mode, func(t *testing.T) {
			id := "2603.02561v2"
			switch mode {
			case "wrong-version":
				id = "2603.02561v3"
			case "wrong-id":
				id = "2603.02562v2"
			case "unversioned":
				id = "2603.02561"
			case "error-entry":
				id = "http://arxiv.org/api/errors"
			}
			apiEntry := `<entry><id>http://arxiv.org/abs/` + id + `</id><title>Verified title</title><published>2026-03-03T12:34:56Z</published><updated>2026-09-30T20:00:00Z</updated><summary>Submission abstract.</summary><author><name>Alice Smith</name></author><category term="cs.IR"/><category term="cs.LG"/><arxiv:primary_category term="cs.IR"/><arxiv:doi>10.1234/example</arxiv:doi><arxiv:comment>12 pages</arxiv:comment><arxiv:journal_ref>Example 2026</arxiv:journal_ref></entry>`
			if mode == "missing-updated" {
				apiEntry = strings.ReplaceAll(apiEntry, "<updated>2026-09-30T20:00:00Z</updated>", "")
			}
			if mode == "missing-primary" {
				apiEntry = strings.ReplaceAll(apiEntry, `<arxiv:primary_category term="cs.IR"/>`, "")
			}
			if mode == "bad-primary" {
				apiEntry = strings.ReplaceAll(apiEntry, `<arxiv:primary_category term="cs.IR"/>`, `<arxiv:primary_category term="cs.AI"/>`)
			}
			if mode == "multiple" {
				apiEntry += apiEntry
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("id_list") != "2603.02561v2" || r.URL.Query().Get("max_results") != "1" || r.URL.Query().Get("search_query") != "" {
					t.Errorf("incorrect exact query %s", r.URL.RawQuery)
				}
				fmt.Fprint(w, syntheticFeed(apiEntry))
			}))
			defer server.Close()
			source := Source{Client: server.Client(), APIBase: server.URL + "/api/query?search_query=cat%3Acs.IR&start=99&sortBy=submittedDate", ArtifactRoot: t.TempDir()}
			version, artifacts, err := source.FetchVersionMetadata(context.Background(), library.Identity{Source: "arxiv", PaperID: "arxiv:2603.02561", Version: "v2"})
			if (err == nil) != (mode == "correct") {
				t.Fatalf("mode %s err=%v version=%#v", mode, err, version)
			}
			if len(artifacts) != 1 {
				t.Fatal("API raw response not preserved")
			}
			checkArtifacts(t, source.ArtifactRoot, artifacts)
			if mode == "correct" && (version.PaperID != "2603.02561" || version.PublishedAt != "2026-03-03T12:34:56Z" || version.UpdatedAt != "2026-09-30T20:00:00Z" || version.AnnouncementDate != "" || !version.MetadataVerified || version.PrimaryCategory != "cs.IR" || len(version.Categories) != 2 || version.DOI != "10.1234/example" || version.JournalRef != "Example 2026" || version.Comment != "12 pages" || len(version.MetadataArtifacts) != 1) {
				t.Fatalf("metadata = %#v", version)
			}
		})
	}
}

func TestAnnouncementCategoryFailureDoesNotStopOtherCategories(t *testing.T) {
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		category := parts[2]
		calls[category]++
		if parts[1] == "atom" && category == "cs.IR" {
			http.Error(w, "synthetic failure", 503)
			return
		}
		if parts[1] == "atom" {
			fmt.Fprint(w, syntheticFeed(""))
			return
		}
		fmt.Fprint(w, syntheticListing("2026-10-02", 0, nil, nil))
	}))
	defer server.Close()
	source := Source{Client: server.Client(), FeedBase: server.URL + "/atom", ListBase: server.URL + "/list", ArtifactRoot: t.TempDir()}
	batches, err := source.FetchAnnouncements(context.Background(), []string{"cs.IR", "cs.LG", "cs.AI", "cs.CL", "stat.ML"})
	if err == nil || len(batches) != 5 || batches[0].Completeness != "incomplete" {
		t.Fatalf("batches=%#v err=%v", batches, err)
	}
	for _, batch := range batches[1:] {
		if batch.Completeness != "complete" || calls[batch.Category] != 2 {
			t.Fatalf("later category did not finish: %#v", batch)
		}
	}
}

func TestSourceHTTPResourceAndURLBounds(t *testing.T) {
	t.Run("status preserves artifact", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) }))
		defer server.Close()
		source := Source{Client: server.Client(), FeedBase: server.URL, ListBase: server.URL, ArtifactRoot: t.TempDir()}
		batches, err := source.FetchAnnouncements(context.Background(), []string{"cs.IR"})
		if err == nil || !strings.Contains(err.Error(), "503") || len(batches) != 1 || batches[0].Completeness != "incomplete" || len(batches[0].Artifacts) != 2 {
			t.Fatalf("batches=%#v err=%v", batches, err)
		}
		checkArtifacts(t, source.ArtifactRoot, batches[0].Artifacts)
	})
	t.Run("body limit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", maxSourceBody+10)) }))
		defer server.Close()
		source := Source{Client: server.Client(), APIBase: server.URL, ArtifactRoot: t.TempDir()}
		_, artifacts, err := source.FetchVersionMetadata(context.Background(), library.Identity{Source: "arxiv", PaperID: "arxiv:2610.00001", Version: "v1"})
		if err == nil || !strings.Contains(err.Error(), "byte limit") || len(artifacts) != 1 {
			t.Fatalf("artifacts=%#v err=%v", artifacts, err)
		}
		checkArtifacts(t, source.ArtifactRoot, artifacts)
	})
	t.Run("context cancellation", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer server.Close()
		source := Source{Client: server.Client(), APIBase: server.URL, ArtifactRoot: t.TempDir()}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, _, err := source.FetchVersionMetadata(ctx, library.Identity{Source: "arxiv", PaperID: "2610.00001", Version: "v1"})
		if err == nil || !strings.Contains(err.Error(), "context deadline") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("foreign redirect", func(t *testing.T) {
		calls := 0
		foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, "wrong") }))
		defer foreign.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, foreign.URL, 302) }))
		defer server.Close()
		source := Source{Client: server.Client(), APIBase: server.URL, ArtifactRoot: t.TempDir()}
		_, artifacts, err := source.FetchVersionMetadata(context.Background(), library.Identity{Source: "arxiv", PaperID: "2610.00001", Version: "v1"})
		if err == nil || calls != 0 || !strings.Contains(err.Error(), "302") || len(artifacts) != 1 {
			t.Fatalf("error=%v calls=%d artifacts=%v", err, calls, artifacts)
		}
		checkArtifacts(t, source.ArtifactRoot, artifacts)
	})
	for _, base := range []string{"https://evil.example/api", "http://arxiv.org/list", "https://arxiv.org/list/../pdf", "https://arxiv.org:443/list", "https://user@arxiv.org/list", "https://arxiv.org/list#fragment"} {
		if _, err := sourceBase(base, "https://arxiv.org/list"); err == nil {
			t.Fatalf("unsafe base accepted %s", base)
		}
	}
	if _, err := (Source{ArtifactRoot: t.TempDir()}).FetchAnnouncements(context.Background(), []string{"../pdf"}); err == nil {
		t.Fatal("unsafe category accepted")
	}
	if _, _, err := (Source{ArtifactRoot: t.TempDir()}).FetchVersionMetadata(context.Background(), library.Identity{Source: "arxiv", PaperID: "https://evil.example/abs/2610.00001", Version: "v1"}); err == nil {
		t.Fatal("unsafe identity accepted")
	}
}

func TestPaperExportedCategoriesAndExtensions(t *testing.T) {
	var entry atomEntry
	if err := xml.Unmarshal([]byte(`<entry xmlns="http://www.w3.org/2005/Atom" xmlns:arxiv="http://arxiv.org/schemas/atom"><id>http://arxiv.org/abs/2610.00001v1</id><published>2026-10-01T12:00:00Z</published><category term="cs.IR"/><arxiv:primary_category term="cs.IR"/><arxiv:doi>10.1234/foo</arxiv:doi><arxiv:journal_ref>Journal reference</arxiv:journal_ref><arxiv:comment>Some comment</arxiv:comment></entry>`), &entry); err != nil {
		t.Fatal(err)
	}
	paper, err := parseEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	if len(paper.Categories) != 1 || len(paper.categories) != 1 || paper.PrimaryCategory != "cs.IR" || paper.DOI != "10.1234/foo" || paper.JournalRef != "Journal reference" || paper.Comment != "Some comment" {
		t.Fatalf("paper = %#v", paper)
	}
	for _, raw := range []string{"oai:arXiv.org:2610.00001v2", "hep-th/9901001v2"} {
		if _, v, err := parseID(raw); err != nil || v != "v2" {
			t.Fatalf("ID %s version=%s error=%v", raw, v, err)
		}
	}
	for _, raw := range []string{"https://evil.example/abs/2610.00001v2", "https://arxiv.org/pdf/2610.00001v2", "https://arxiv.org/abs/2610.00001v2?download=1", "https://arxiv.org/abs/2610.00001v2#fragment", "http://user@arxiv.org/abs/2610.00001v2"} {
		if _, _, err := parseID(raw); err == nil {
			t.Fatalf("nonofficial/arbitrary ID URL accepted %s", raw)
		}
	}
}
