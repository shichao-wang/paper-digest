package state

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/digest"
	"github.com/shichao-wang/paper-digest/internal/papers"
)

func saveBrowseDay(t *testing.T, store *Store, topic, date string, items []papers.Paper, summaries map[string]string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.ClaimDay(ctx, topic, date); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCandidates(ctx, topic, date, items); err != nil {
		t.Fatal(err)
	}
	for id, text := range summaries {
		if err := store.SaveSummary(ctx, topic, date, id, digest.Summary{Text: text, Model: "fixture-model", PromptVersion: "fixture-v1"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBrowseLatestAssociationBeforeSearchAndSummaryFilter(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "browse.db"))
	defer store.Close()
	ctx := context.Background()
	saveBrowseDay(t, store, "topic", "2026-09-28", []papers.Paper{{ID: "same", Version: "v1", Title: "old title", Authors: []string{"old author"}, Abstract: "old abstract"}}, map[string]string{"same": "old chinese summary"})
	saveBrowseDay(t, store, "topic", "2026-09-29", []papers.Paper{{ID: "same", Version: "v2", Title: "current title"}, {ID: "second", Version: "v1", Title: "another", Authors: []string{"Lin"}, Abstract: "Search systems"}}, map[string]string{"second": "推荐"})
	saveBrowseDay(t, store, "other-topic", "2026-09-30", []papers.Paper{{ID: "hidden", Title: "hidden"}}, nil)
	page, err := store.BrowsePapers(ctx, "topic", PaperQuery{})
	if err != nil || page.Total != 2 || len(page.Items) != 2 || page.Items[0].Version != "v2" || page.Items[0].Summary != nil || page.Items[0].DigestDate != "2026-09-29" {
		t.Fatalf("latest page=%+v err=%v", page, err)
	}
	for _, query := range []string{"old title", "old author", "old abstract", "old chinese summary"} {
		page, err := store.BrowsePapers(ctx, "topic", PaperQuery{Q: query})
		if err != nil || page.Total != 0 {
			t.Fatalf("old association matched q=%q page=%+v err=%v", query, page, err)
		}
	}
	for _, query := range []string{"Lin", "Search systems", "推荐"} {
		page, err := store.BrowsePapers(ctx, "topic", PaperQuery{Q: query})
		if err != nil || page.Total != 1 || page.Items[0].ID != "second" {
			t.Fatalf("search q=%q page=%+v err=%v", query, page, err)
		}
	}
	for _, filter := range []string{"available", "missing"} {
		page, err := store.BrowsePapers(ctx, "topic", PaperQuery{Summary: filter})
		if err != nil || page.Total != 1 || (page.Items[0].Summary != nil) != (filter == "available") {
			t.Fatalf("summary filter=%s page=%+v err=%v", filter, page, err)
		}
	}
	page, err = store.BrowsePapers(ctx, "topic", PaperQuery{Date: "2026-09-28", Q: "old chinese"})
	if err != nil || page.Total != 1 || page.Items[0].Version != "v1" || page.Items[0].Summary.Text != "old chinese summary" {
		t.Fatalf("dated query=%+v err=%v", page, err)
	}
	old, err := store.PaperDetail(ctx, "topic", "same", "2026-09-28")
	if err != nil || old.Version != "v1" || old.Summary.Text != "old chinese summary" {
		t.Fatalf("old detail=%+v err=%v", old, err)
	}
	if _, err := store.PaperDetail(ctx, "other-topic", "same", "2026-09-28"); !errors.Is(err, ErrPaperNotFound) {
		t.Fatalf("topic isolation: %v", err)
	}
}

func TestBrowseLiteralSearchPaginationAndUnversionedData(t *testing.T) {
	store := openTestStore(t, ":memory:")
	defer store.Close()
	ctx := context.Background()
	published := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	saveBrowseDay(t, store, "topic", "2026-09-29", []papers.Paper{
		{ID: "percent", Title: "100% improvement", Published: published},
		{ID: "underscore", Title: "a_b"}, {ID: "slash", Title: `a\b`}, {ID: "normal", Title: "normal title"},
	}, nil)
	saveBrowseDay(t, store, "topic", "2026-09-30", []papers.Paper{{ID: "newest", Title: "newest"}}, nil)
	for _, query := range []struct{ q, id string }{{"%", "percent"}, {"_", "underscore"}, {`\`, "slash"}, {"' OR 1=1 --", ""}} {
		page, err := store.BrowsePapers(ctx, "topic", PaperQuery{Q: query.q})
		if err != nil {
			t.Fatal(err)
		}
		if query.id == "" {
			if page.Total != 0 {
				t.Fatalf("SQL injection matched: %+v", page)
			}
			continue
		}
		if page.Total != 1 || page.Items[0].ID != query.id {
			t.Fatalf("literal q=%q: %+v", query.q, page)
		}
	}
	for _, query := range []struct {
		page int
		ids  []string
	}{{1, []string{"newest", "percent"}}, {2, []string{"underscore", "slash"}}, {3, []string{"normal"}}, {4, []string{}}} {
		page, err := store.BrowsePapers(ctx, "topic", PaperQuery{PageQuery: PageQuery{Page: query.page, PageSize: 2}})
		if err != nil || page.Total != 5 || len(page.Items) != len(query.ids) || page.Page != query.page || page.PageSize != 2 {
			t.Fatalf("pagination=%+v err=%v", page, err)
		}
		for i, id := range query.ids {
			if page.Items[i].ID != id {
				t.Fatalf("order=%+v", page.Items)
			}
		}
	}
	record, err := store.PaperDetail(ctx, "topic", "percent", "2026-09-29")
	if err != nil || record.Version != "" || !record.PublishedAt.Equal(published) || record.Authors == nil {
		t.Fatalf("unversioned record=%+v err=%v", record, err)
	}
}

func TestSameVersionSummaryIsPerDateAndDigestHistoryIncludesEmpty(t *testing.T) {
	store := openTestStore(t, ":memory:")
	defer store.Close()
	ctx := context.Background()
	saveBrowseDay(t, store, "topic", "2026-09-28", []papers.Paper{{ID: "same", Version: "v1"}}, map[string]string{"same": "earlier"})
	saveBrowseDay(t, store, "topic", "2026-09-29", []papers.Paper{{ID: "first", Version: "v1"}, {ID: "same", Version: "v1"}}, map[string]string{"first": "first summary"})
	if err := store.MarkMissed(ctx, "topic", "2026-09-29"); err != nil {
		t.Fatal(err)
	}
	saveBrowseDay(t, store, "topic", "2026-09-30", nil, nil)
	if err := store.Ready(ctx, "topic", "2026-09-30", "empty digest message"); err != nil {
		t.Fatal(err)
	}
	history, err := store.BrowseDigests(ctx, "topic", PageQuery{PageSize: 2})
	if err != nil || history.Total != 3 || len(history.Items) != 2 || history.Items[0].PaperCount != 0 || history.Items[1].PaperCount != 2 || history.Items[1].SummaryCount != 1 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	detail, err := store.DigestDetail(ctx, "topic", "2026-09-29")
	if err != nil || detail.Status != "missed" || len(detail.Items) != 2 || detail.Items[0].ID != "first" || detail.Items[1].ID != "same" || detail.Items[1].Summary != nil {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	empty, err := store.DigestDetail(ctx, "topic", "2026-09-30")
	if err != nil || empty.Message != "empty digest message" || empty.Items == nil || len(empty.Items) != 0 {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
	if _, err := store.DigestDetail(ctx, "topic", "2026-09-01"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("missing=%v", err)
	}
}

func TestBrowsePaginationUsesConsistentSnapshotDuringConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	reader := openTestStore(t, path)
	defer reader.Close()
	writer := openTestStore(t, path)
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		for i := 1; i <= 30; i++ {
			date := fmt.Sprintf("2026-09-%02d", i)
			if _, err := writer.ClaimDay(ctx, "topic", date); err != nil {
				done <- err
				return
			}
			if err := writer.SaveCandidates(ctx, "topic", date, []papers.Paper{{ID: fmt.Sprintf("paper-%d", i)}}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	<-started
	for i := 0; i < 40; i++ {
		papers, err := reader.BrowsePapers(ctx, "topic", PaperQuery{PageQuery: PageQuery{PageSize: 100}})
		if err != nil {
			t.Fatal(err)
		}
		if papers.Total != len(papers.Items) {
			t.Fatalf("paper count differs from snapshot: total=%d items=%d", papers.Total, len(papers.Items))
		}
		digests, err := reader.BrowseDigests(ctx, "topic", PageQuery{PageSize: 100})
		if err != nil {
			t.Fatal(err)
		}
		if digests.Total != len(digests.Items) {
			t.Fatalf("digest count differs from snapshot: total=%d items=%d", digests.Total, len(digests.Items))
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBrowseValidationAndReleasedTransactions(t *testing.T) {
	store := openTestStore(t, ":memory:")
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, q := range []PaperQuery{{Date: "2026-02-30"}, {Summary: "bad"}, {PageQuery: PageQuery{Page: -1}}, {PageQuery: PageQuery{PageSize: 101}}} {
		if _, err := store.BrowsePapers(ctx, "topic", q); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("query=%+v err=%v", q, err)
		}
	}
	saveBrowseDay(t, store, "topic", "2026-09-30", []papers.Paper{{ID: "corrupt"}}, nil)
	if _, err := store.db.ExecContext(ctx, `UPDATE papers SET data = 'broken'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BrowsePapers(ctx, "topic", PaperQuery{}); err == nil {
		t.Fatal("corrupt data should fail")
	}
	if err := store.Health(ctx); err != nil {
		t.Fatalf("browse left transaction or rows open: %v", err)
	}
}
