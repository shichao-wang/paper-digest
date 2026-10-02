package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/state"
)

func TestReanalyzeRestartsMetadataBeforeScreening(t *testing.T) {
	for _, status := range []string{"queued", "retry_wait", "running"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "library.db")
			cfg := serveConfig(path, false)
			store, err := state.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			id := library.Identity{Source: "arxiv", PaperID: "2501.12345", Version: "v2"}
			if err := store.UpsertVersion(ctx, library.Version{Identity: id, Title: "Unverified RSS"}); err != nil {
				t.Fatal(err)
			}
			if err := store.EnqueueTask(ctx, id, "metadata", 0); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			var old library.Task
			if status != "queued" {
				old, err = store.ClaimTask(ctx, "metadata", now, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				if status == "retry_wait" {
					if err := store.FailTask(ctx, old, "retry_wait", "API unavailable", now.Add(time.Hour), now); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := runLibraryCommand(ctx, cfg, []string{"reanalyze", id.PaperID, id.Version}); err != nil {
				t.Fatal(err)
			}
			if task, err := store.ClaimTask(ctx, "relevance", now, time.Hour); !errors.Is(err, library.ErrNotFound) {
				t.Fatalf("premature screening=%+v err=%v", task, err)
			}
			if status == "running" {
				obsolete := library.Version{Identity: id, Title: "Obsolete exact metadata", MetadataVerified: true}
				if err := store.CompleteTask(ctx, old, library.Completion{Version: &obsolete, Next: []string{"relevance"}}, now); err != nil {
					t.Fatal(err)
				}
				v, err := store.GetVersion(ctx, id)
				if err != nil || v.MetadataVerified || v.Title != "Unverified RSS" {
					t.Fatalf("obsolete metadata published=%+v err=%v", v, err)
				}
				if _, err := store.ClaimTask(ctx, "relevance", now, time.Hour); !errors.Is(err, library.ErrNotFound) {
					t.Fatalf("obsolete screening queued: %v", err)
				}
			}
			current, err := store.ClaimTask(ctx, "metadata", now, time.Hour)
			if err != nil || current.Generation != 1 {
				t.Fatalf("metadata=%+v err=%v", current, err)
			}
			exact := library.Version{Identity: id, Title: "Current exact API metadata", MetadataVerified: true}
			if err := store.CompleteTask(ctx, current, library.Completion{Version: &exact, Next: []string{"relevance"}}, now); err != nil {
				t.Fatal(err)
			}
			rel, err := store.ClaimTask(ctx, "relevance", now, time.Hour)
			if err != nil || rel.Generation != 1 {
				t.Fatalf("screening=%+v err=%v", rel, err)
			}
			v, err := store.GetVersion(ctx, id)
			if err != nil || !v.MetadataVerified || v.Title != exact.Title {
				t.Fatalf("screening metadata=%+v err=%v", v, err)
			}
		})
	}
}
