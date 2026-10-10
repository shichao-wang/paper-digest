package papers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestFetchParsesAtomFeedAndQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("request method = %q, want GET", r.Method)
		}
		if got := r.URL.Query().Get("max_results"); got != "10" {
			t.Errorf("max_results = %q, want 10", got)
		}
		if got := r.URL.Query().Get("sortBy"); got != "submittedDate" {
			t.Errorf("sortBy = %q, want submittedDate", got)
		}
		if got := r.URL.Query().Get("sortOrder"); got != "descending" {
			t.Errorf("sortOrder = %q, want descending", got)
		}
		if got := r.URL.Query().Get("keep"); got != "yes" {
			t.Errorf("existing query parameter = %q, want yes", got)
		}
		if got := r.URL.Query().Get("search_query"); !strings.Contains(got, "cat:cs.IR") || !strings.Contains(got, "ti:recommendation") || !strings.Contains(got, "abs:advertising") || strings.Contains(got, "all:") || strings.Contains(got, "econ.GN") {
			t.Errorf("search_query = %q, want title, abstract, and cs.IR terms without full-text search", got)
		}
		w.Header().Set("Content-Type", "application/atom+xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>http://arxiv.org/abs/2401.01234v2</id>
    <updated>2024-01-04T12:30:00Z</updated>
    <published>2024-01-03T12:00:00Z</published>
    <title>  A  paper
 title </title>
    <summary> An abstract with &amp; entity. </summary>
    <author><name>Alice</name></author>
    <author><name>  Bob  Smith </name></author>
    <category term="cs.IR"/>
    <category term="cs.AI"/>
  </entry>
  <entry>
    <id>http://arxiv.org/abs/2401.01235</id>
    <published>2024-01-02T12:00:00Z</published>
    <title>Without a version</title>
    <summary>Plain abstract.</summary>
  </entry>
</feed>`)
	}))
	defer server.Close()

	base, err := url.Parse(server.URL + "/api/query?keep=yes")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Fetch(context.Background(), server.Client(), base.String(), 10)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Fetch() returned %d papers, want 2", len(got))
	}

	first := got[0]
	if first.ID != "arxiv:2401.01234" || first.Version != "v2" {
		t.Errorf("ID/version = %q/%q, want arxiv:2401.01234/v2", first.ID, first.Version)
	}
	if first.Title != "A paper title" || first.Abstract != "An abstract with & entity." {
		t.Errorf("title/abstract normalization failed: %q / %q", first.Title, first.Abstract)
	}
	if strings.Join(first.Authors, ",") != "Alice,Bob Smith" {
		t.Errorf("authors = %#v", first.Authors)
	}
	if first.Updated.Format(time.RFC3339) != "2024-01-04T12:30:00Z" {
		t.Errorf("Updated = %s", first.Updated.Format(time.RFC3339))
	}
	if first.URL != "https://arxiv.org/abs/2401.01234v2" {
		t.Errorf("URL = %q", first.URL)
	}
	if len(first.categories) != 2 || first.categories[0] != "cs.IR" {
		t.Errorf("categories = %#v", first.categories)
	}
	if got[1].Version != "" || got[1].URL != "https://arxiv.org/abs/2401.01235" {
		t.Errorf("unversioned paper = %#v", got[1])
	}
}

func TestParseID(t *testing.T) {
	tests := []struct {
		input   string
		id      string
		version string
		wantErr bool
	}{
		{input: "http://arxiv.org/abs/2401.12345v3", id: "arxiv:2401.12345", version: "v3"},
		{input: "https://arxiv.org/abs/2401.1234", id: "arxiv:2401.1234"},
		{input: "http://arxiv.org/abs/hep-th/9901001v2", id: "arxiv:hep-th/9901001", version: "v2"},
		{input: "arxiv:2401.12345v1", id: "arxiv:2401.12345", version: "v1"},
		{input: "not-an-arxiv-id", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			id, version, err := parseID(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseID() error = %v, wantErr %t", err, tt.wantErr)
			}
			if err == nil && (id != tt.id || version != tt.version) {
				t.Errorf("parseID() = %q, %q; want %q, %q", id, version, tt.id, tt.version)
			}
		})
	}
}

func TestFetchRejectsBadResponses(t *testing.T) {
	t.Run("http status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		defer server.Close()
		if _, err := Fetch(context.Background(), server.Client(), server.URL, 5); err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("Fetch() error = %v, want 503 error", err)
		}
	})

	t.Run("invalid xml", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "<feed>")
		}))
		defer server.Close()
		if _, err := Fetch(context.Background(), server.Client(), server.URL, 5); err == nil {
			t.Fatal("Fetch() error = nil, want XML error")
		}
	})

	t.Run("bad limit", func(t *testing.T) {
		if _, err := Fetch(context.Background(), nil, "", 0); err == nil {
			t.Fatal("Fetch() error = nil, want invalid limit error")
		}
	})
}

func useFetchPaging(t *testing.T, pageSize, maxPages int, delay time.Duration) {
	t.Helper()
	prevSize, prevPages, prevDelay := arxivPageSize, arxivMaxPages, arxivPageDelay
	arxivPageSize, arxivMaxPages, arxivPageDelay = pageSize, maxPages, delay
	t.Cleanup(func() {
		arxivPageSize, arxivMaxPages, arxivPageDelay = prevSize, prevPages, prevDelay
	})
}

func atomEntryXML(id, published, updated, title string) string {
	if updated == "" {
		updated = published
	}
	return `<entry><id>http://arxiv.org/abs/` + id + `</id><published>` + published + `</published><updated>` + updated + `</updated><title>` + title + `</title><summary>Abstract.</summary></entry>`
}

func TestFetchSinceStopsAtLookbackAndKeepsPagingAfterUpdates(t *testing.T) {
	useFetchPaging(t, 2, 5, 0)
	var starts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("start")
		starts = append(starts, start)
		if !strings.Contains(r.URL.Query().Get("search_query"), "cat:cs.IR") || strings.Contains(r.URL.Query().Get("search_query"), "all:") {
			t.Errorf("search_query = %q", r.URL.Query().Get("search_query"))
		}
		var body string
		switch start {
		case "0":
			body = atomEntryXML("2401.00001", "2026-10-08T00:00:00Z", "", "Newest") +
				atomEntryXML("2401.00002", "2026-10-07T00:00:00Z", "", "Still new")
		case "2":
			body = atomEntryXML("2401.00003", "2020-01-01T00:00:00Z", "2026-10-08T00:00:00Z", "Updated old paper") +
				atomEntryXML("2401.00004", "2026-10-06T00:00:00Z", "", "Still inside window")
		case "4":
			body = atomEntryXML("2401.00005", "2026-10-05T00:00:00Z", "", "Boundary") +
				atomEntryXML("2401.00006", "2026-10-01T00:00:00Z", "2026-10-01T00:00:00Z", "Before window")
		default:
			t.Errorf("unexpected start %q", start)
		}
		fmt.Fprintf(w, `<feed xmlns="http://www.w3.org/2005/Atom">%s</feed>`, body)
	}))
	defer server.Close()

	since := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	got, err := FetchSince(context.Background(), server.Client(), server.URL, since)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(starts, ",") != "0,2,4" {
		t.Fatalf("pages = %v, want start 0, 2, 4", starts)
	}
	if len(got) != 6 {
		t.Fatalf("FetchSince() returned %d papers, want 6", len(got))
	}
}

func TestFetchSinceStopsOnShortPageAndCancel(t *testing.T) {
	t.Run("short page", func(t *testing.T) {
		useFetchPaging(t, 2, 5, 0)
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			fmt.Fprintf(w, `<feed xmlns="http://www.w3.org/2005/Atom">%s</feed>`, atomEntryXML("2401.00010", "2026-10-08T00:00:00Z", "", "Only one"))
		}))
		defer server.Close()
		got, err := FetchSince(context.Background(), server.Client(), server.URL, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
		if err != nil || len(got) != 1 || calls != 1 {
			t.Fatalf("got %d papers, calls %d, err %v", len(got), calls, err)
		}
	})

	t.Run("cancel between pages", func(t *testing.T) {
		useFetchPaging(t, 1, 5, time.Hour)
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls > 1 {
				t.Error("fetched another page after cancel")
			}
			cancel()
			fmt.Fprintf(w, `<feed xmlns="http://www.w3.org/2005/Atom">%s</feed>`, atomEntryXML("2401.00011", "2026-10-08T00:00:00Z", "", "One"))
		}))
		defer server.Close()
		_, err := FetchSince(ctx, server.Client(), server.URL, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
		if err == nil || calls != 1 {
			t.Fatalf("err = %v, calls = %d, want cancellation after the first page", err, calls)
		}
	})
}

func TestSelectFiltersAndCapsResults(t *testing.T) {
	now := time.Date(2026, time.September, 27, 12, 0, 0, 0, time.UTC)
	papers := []Paper{
		{ID: "recent-a", Published: now.Add(-time.Hour), Title: "Collaborative recommendation for web search", categories: []string{"cs.IR"}},
		{ID: "recent-b", Published: now.Add(-2 * time.Hour), Title: "Sponsored search advertising auctions"},
		{ID: "recent-c", Published: now.Add(-3 * time.Hour), Title: "Personalized recommendation in information retrieval"},
		{ID: "recent-d", Published: now.Add(-4 * time.Hour), Title: "Online advertising recommendation systems"},
		{ID: "recent-e", Published: now.Add(-5 * time.Hour), Title: "Search ranking for ad allocation"},
		{ID: "recent-f", Published: now.Add(-6 * time.Hour), Title: "Personalized recommendation with ad targeting"},
		{ID: "single-topic", Published: now.Add(-7 * time.Hour), Title: "Collaborative filtering for recommendation"},
		{ID: "old", Published: now.Add(-8 * 24 * time.Hour), Title: "Recommendation for web search"},
		{ID: "future", Published: now.Add(time.Minute), Title: "Recommendation for web search"},
		{ID: "already-seen", Published: now, Title: "Recommendation for search advertising"},
		{ID: "recent-a", Published: now.Add(-30 * time.Minute), Title: "Collaborative recommendation for web search", categories: []string{"cs.IR"}},
	}
	seenCalls := make(map[string]int)
	seen := func(id string) bool {
		seenCalls[id]++
		return id == "already-seen"
	}

	got := Select(papers, now, 7, 50, seen)
	if len(got) != maxDailyPapers {
		t.Fatalf("Select() returned %d papers, want cap %d", len(got), maxDailyPapers)
	}
	if got[0].ID != "recent-a" || got[1].ID != "recent-b" {
		t.Errorf("selected order starts with %q, %q", got[0].ID, got[1].ID)
	}
	if !got[0].Published.Equal(now.Add(-30 * time.Minute)) {
		t.Errorf("duplicate ID did not keep newest version: published %s", got[0].Published)
	}
	for _, paper := range got {
		if paper.ID == "old" || paper.ID == "future" || paper.ID == "already-seen" {
			t.Errorf("Select() included excluded paper %q", paper.ID)
		}
	}
	if seenCalls["recent-a"] != 1 {
		t.Errorf("seen callback called %d times for duplicate ID, want once", seenCalls["recent-a"])
	}
}

func TestSelectUsesUnionOfRASTopics(t *testing.T) {
	now := time.Now().UTC()
	got := Select([]Paper{
		{ID: "category-search", Published: now, Title: "Information retrieval", categories: []string{"cs.IR"}},
		{ID: "keyword-cross", Published: now, Title: "Ad ranking for sponsored search recommendation"},
		{ID: "recommendation-only", Published: now, Title: "Collaborative filtering for personalized recommendation"},
		{ID: "advertising-only", Published: now, Title: "Online advertising auction design"},
		{ID: "search-only", Published: now, Title: "Search engine query processing"},
		{ID: "unrelated", Published: now, Title: "Research on molecular embeddings"},
	}, now, 7, 5, nil)
	if len(got) != 5 {
		t.Fatalf("Select() returned %d papers, want 5 papers matching the RAS union", len(got))
	}
	selected := map[string]bool{}
	for _, paper := range got {
		selected[paper.ID] = true
	}
	for _, id := range []string{"category-search", "keyword-cross", "recommendation-only", "advertising-only", "search-only"} {
		if !selected[id] {
			t.Errorf("RAS paper %q was not selected: %#v", id, got)
		}
	}
	if selected["unrelated"] {
		t.Errorf("unrelated research paper was selected: %#v", got)
	}
}
