package state

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/shichao-wang/paper-digest/internal/library"
)

func observationFixture(capturedAt string) library.CategoryBatch {
	v := versionFixture(1, "v1")
	v.CapturedAt = capturedAt
	v.MetadataArtifacts = []library.Artifact{{Path: "metadata/first.xml", SHA256: strings.Repeat("b", 64), URL: "https://export.arxiv.org/api/query?id_list=2610.00001v1", CapturedAt: capturedAt, Kind: "metadata-atom"}}
	return library.CategoryBatch{
		Category: "cs.IR", BuiltAt: "2026-10-02T08:00:00Z", CapturedAt: capturedAt,
		Completeness: "incomplete", Reason: "official list has no announcement date",
		Counts:    map[string]int{"new": 1},
		Artifacts: []library.Artifact{{Path: "snapshots/first.bin", SHA256: strings.Repeat("a", 64), URL: "https://rss.arxiv.org/atom/cs.IR", CapturedAt: capturedAt, Kind: "announcement-atom"}},
		Versions:  []library.Version{v},
		Events:    []library.Announcement{{Identity: v.Identity, Category: "cs.IR", Type: "new"}},
	}
}

func TestSourceObservationDeduplicatesAcrossCaptureTimesAndRetainsSnapshot(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, ":memory:")
	defer s.Close()
	first := observationFixture("2026-10-02T08:00:00Z")
	before, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSourceObservation(ctx, first); err != nil {
		t.Fatal(err)
	}
	// Every local capture timestamp changes, including nested metadata responses.
	// Identical response bytes can also be retained at a different local path.
	repeated := observationFixture("2026-10-02T09:00:00Z")
	repeated.Artifacts[0].Path = "snapshots/second.bin"
	repeated.Versions[0].MetadataArtifacts[0].Path = "metadata/second.xml"
	for i := 0; i < 3; i++ {
		if err := s.SaveSourceObservation(ctx, repeated); err != nil {
			t.Fatal(err)
		}
	}
	mustLibraryCount(t, s, "library_source_observations", 1)
	mustLibraryCount(t, s, "library_versions", 1)
	mustLibraryCount(t, s, "library_tasks", 1)
	mustLibraryCount(t, s, "library_batches", 0)
	mustLibraryCount(t, s, "library_events", 0)

	var capturedAt string
	var raw []byte
	if err := s.db.QueryRow(`SELECT captured_at,data FROM library_source_observations`).Scan(&capturedAt, &raw); err != nil {
		t.Fatal(err)
	}
	var saved library.CategoryBatch
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if capturedAt != first.CapturedAt || !reflect.DeepEqual(saved, first) {
		t.Fatalf("first complete snapshot changed: captured_at=%s snapshot=%+v", capturedAt, saved)
	}
	after, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("identity normalization mutated caller's batch")
	}
}

func TestSourceObservationRetainsDistinctResponsesAndContent(t *testing.T) {
	for _, change := range []string{"response-hash", "metadata-response-hash", "candidate-content", "category"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t, ":memory:")
			defer s.Close()
			first := observationFixture("2026-10-02T08:00:00Z")
			changed := observationFixture("2026-10-02T09:00:00Z")
			switch change {
			case "response-hash":
				changed.Artifacts[0].SHA256 = strings.Repeat("c", 64)
				changed.Artifacts[0].Path = "snapshots/changed.bin"
			case "metadata-response-hash":
				changed.Versions[0].MetadataArtifacts[0].SHA256 = strings.Repeat("c", 64)
				changed.Versions[0].MetadataArtifacts[0].Path = "metadata/changed.xml"
			case "candidate-content":
				// With no captured responses, candidate content still distinguishes observations.
				changed.Artifacts = nil
				changed.Versions[0].MetadataArtifacts = nil
				first.Artifacts = nil
				first.Versions[0].MetadataArtifacts = nil
				changed.Versions[0].Title = "Updated candidate title"
			case "category":
				changed.Category = "cs.LG"
			}
			if err := s.SaveSourceObservation(ctx, first); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveSourceObservation(ctx, changed); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveSourceObservation(ctx, changed); err != nil {
				t.Fatal(err)
			}
			mustLibraryCount(t, s, "library_source_observations", 2)
			var raw []byte
			if err := s.db.QueryRow(`SELECT data FROM library_source_observations WHERE captured_at=?`, changed.CapturedAt).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var saved library.CategoryBatch
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(saved, changed) {
				t.Fatalf("distinct snapshot lost its content/dependencies: %+v", saved)
			}
		})
	}
}
