package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/papers"
)

func TestSelectionLabelsMigrateWithoutTouchingJobs(t *testing.T) {
	path := t.TempDir() + "/digest.db"
	ctx := context.Background()
	store := openTestStore(t, path)
	const topic = "recommendation-advertising-search"
	const date = "2026-10-09"
	if _, err := store.ClaimDay(ctx, topic, date); err != nil {
		t.Fatal(err)
	}
	paper := papers.Paper{ID: "arxiv:strong", Title: "Sequential Recommendation", Abstract: "session logs", Categories: []string{"cs.IR"}, Published: time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)}
	if err := store.SaveCandidates(ctx, topic, date, []papers.Paper{paper}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE selection_labels`); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store = openTestStore(t, path)
	defer store.Close()
	job, err := store.GetJob(ctx, topic, date)
	if err != nil || job.Status != statusProcessing {
		t.Fatalf("schema upgrade changed the digest: %+v %v", job, err)
	}
	var jobsBefore int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobsBefore); err != nil {
		t.Fatal(err)
	}
	picks, err := store.DigestPicks(ctx, topic, "2026-10-08")
	if err != nil || picks != nil {
		t.Fatalf("missing digest = %#v, %v", picks, err)
	}
	if _, err := store.GetJob(ctx, topic, "2026-10-08"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("missing digest created a job: %v", err)
	}
	saved, err := store.DigestPicks(ctx, topic, date)
	if err != nil || len(saved) != 1 || saved[0].ID != paper.ID || saved[0].Title != paper.Title {
		t.Fatalf("digest picks = %+v, %v", saved, err)
	}
	if _, err := store.DigestPicks(ctx, topic, "bad"); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("invalid date err = %v", err)
	}

	snapshot, err := SelectionLabelSnapshot(paper)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSelectionLabel(ctx, topic, paper.ID, LabelRelevant, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSelectionLabel(ctx, topic, paper.ID, LabelNotRelevant, ""); err != nil {
		t.Fatal(err)
	}
	labels, err := store.SelectionLabels(ctx, topic)
	if err != nil || len(labels) != 1 || labels[0].Label != LabelNotRelevant {
		t.Fatalf("labels = %+v, %v", labels, err)
	}
	parsed, err := ParseSelectionSnapshot(labels[0].Snapshot)
	if err != nil || parsed.Title != paper.Title || len(parsed.Categories) != 1 || parsed.Categories[0] != "cs.IR" {
		t.Fatalf("snapshot = %+v, %v", parsed, err)
	}
	if err := store.SetSelectionLabel(ctx, topic, paper.ID, "maybe", snapshot); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("invalid label err = %v", err)
	}
	if err := store.ClearSelectionLabel(ctx, topic, paper.ID); err != nil {
		t.Fatal(err)
	}
	labels, err = store.SelectionLabels(ctx, topic)
	if err != nil || len(labels) != 0 {
		t.Fatalf("cleared labels = %+v, %v", labels, err)
	}
	var jobsAfter int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobsAfter); err != nil {
		t.Fatal(err)
	}
	if jobsAfter != jobsBefore {
		t.Fatalf("jobs %d → %d", jobsBefore, jobsAfter)
	}
	seen, err := store.Seen(ctx, topic, paper.ID)
	if err != nil || seen {
		t.Fatalf("label marked the paper seen: %v %v", seen, err)
	}
	if _, err := ParseSelectionSnapshot("  "); err == nil {
		t.Fatal("empty snapshot parsed")
	}
}
