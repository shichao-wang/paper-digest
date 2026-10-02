package state

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/library"
)

func TestCompleteBatchSurvivesFailedPollAndRetainsObservation(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, ":memory:")
	defer s.Close()
	complete := batchFixture(2)
	complete.Completeness = "complete"
	complete.Artifacts = []library.Artifact{{Path: "complete.atom", SHA256: "complete"}}
	if err := s.SaveCategoryBatch(ctx, complete); err != nil {
		t.Fatal(err)
	}
	failed := batchFixture(1)
	failed.Completeness, failed.Reason = "incomplete", "official list timed out"
	failed.CapturedAt = "2026-10-02T12:00:00Z"
	failed.Artifacts = []library.Artifact{{Path: "failed.atom", SHA256: "failed"}}
	for i := 0; i < 2; i++ {
		if err := s.SaveCategoryBatch(ctx, failed); err != nil {
			t.Fatal(err)
		}
	}
	var raw []byte
	if err := s.db.QueryRow(`SELECT data FROM library_batches WHERE category=? AND date=?`, complete.Category, complete.Date).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got library.CategoryBatch
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := complete
	want.Artifacts = mergeArtifacts(complete.Artifacts, failed.Artifacts)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("complete snapshot overwritten: got=%+v want=%+v", got, want)
	}
	mustLibraryCount(t, s, "library_source_observations", 1)
	if err := s.db.QueryRow(`SELECT data FROM library_source_observations`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, failed) {
		t.Fatalf("failed observation lost: %+v", got)
	}
	mustLibraryCount(t, s, "library_versions", 2)
	mustLibraryCount(t, s, "library_tasks", 2)
	// 两份状态分别表达已验证批次与后续失败观察，不将权威记录降级。
	status, err := s.LibraryStatus(ctx)
	if err != nil || len(status.Batches) != 2 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	completeCount := 0
	for _, b := range status.Batches {
		if b.Completeness == "complete" {
			completeCount++
		}
	}
	if completeCount != 1 {
		t.Fatalf("complete batches=%d", completeCount)
	}
}

func TestVerifiedMetadataSurvivesUnverifiedAnnouncement(t *testing.T) {
	for _, route := range []string{"upsert", "batch", "observation"} {
		t.Run(route, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t, ":memory:")
			defer s.Close()
			verified := versionFixture(1, "v1")
			verified.MetadataVerified = true
			verified.Title, verified.Abstract = "Exact API title", "Exact API abstract"
			verified.Authors = []string{"Exact Author"}
			verified.Categories = []string{"cs.IR"}
			verified.PrimaryCategory = "cs.IR"
			verified.PublishedAt, verified.UpdatedAt = "2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z"
			verified.DOI, verified.JournalRef, verified.Comment, verified.AuthorKeywords = "", "", "", nil
			verified.MetadataArtifacts = []library.Artifact{{Path: "api.xml", SHA256: "api"}}
			if err := s.UpsertVersion(ctx, verified); err != nil {
				t.Fatal(err)
			}
			incoming := verified
			incoming.MetadataVerified = false
			incoming.Title, incoming.Abstract = "RSS title", "RSS abstract"
			incoming.Authors, incoming.Categories = []string{"Wrong Author"}, []string{"cs.LG"}
			incoming.PrimaryCategory = "cs.LG"
			incoming.PublishedAt, incoming.UpdatedAt = "wrong", "wrong"
			incoming.DOI, incoming.JournalRef, incoming.Comment = "wrong", "wrong", "wrong"
			incoming.AuthorKeywords = []string{"wrong"}
			incoming.MetadataArtifacts = []library.Artifact{{Path: "rss.xml", SHA256: "rss"}}
			b := batchFixture(1)
			b.Versions = []library.Version{incoming}
			switch route {
			case "upsert":
				if err := s.UpsertVersion(ctx, incoming); err != nil {
					t.Fatal(err)
				}
			case "batch":
				if err := s.SaveCategoryBatch(ctx, b); err != nil {
					t.Fatal(err)
				}
			case "observation":
				b.Date = ""
				if err := s.SaveSourceObservation(ctx, b); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.GetVersion(ctx, verified.Identity)
			if err != nil {
				t.Fatal(err)
			}
			want := verified
			want.MetadataArtifacts = mergeArtifacts(verified.MetadataArtifacts, incoming.MetadataArtifacts)
			if route == "batch" {
				if want.Origin == "" {
					want.Origin = "announcement"
				}
				if want.AnnouncementDate == "" {
					want.AnnouncementDate = b.Date
				}
				if want.CapturedAt == "" {
					want.CapturedAt = b.CapturedAt
				}
			} else if route == "observation" {
				if want.Origin == "" {
					want.Origin = "announcement_unverified"
				}
				if want.CapturedAt == "" {
					want.CapturedAt = b.CapturedAt
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("verified metadata changed: got=%+v want=%+v", got, want)
			}
			// 后续精确 API 校验仍允许刷新权威字段。
			fresh := verified
			fresh.Title = "Refreshed exact API title"
			if err := s.UpsertVersion(ctx, fresh); err != nil {
				t.Fatal(err)
			}
			got, err = s.GetVersion(ctx, verified.Identity)
			if err != nil || got.Title != fresh.Title {
				t.Fatalf("verified refresh=%+v err=%v", got, err)
			}
		})
	}
}

func TestClaimSkipsSupersededTasksAcrossStagesAndStatuses(t *testing.T) {
	for _, stage := range []string{"metadata", "relevance", "document", "analyze", "compare"} {
		for _, status := range []string{"queued", "retry_wait", "expired-running"} {
			t.Run(stage+"/"+status, func(t *testing.T) {
				ctx := context.Background()
				s := openTestStore(t, ":memory:")
				defer s.Close()
				v := versionFixture(1, "v1")
				if err := s.UpsertVersion(ctx, v); err != nil {
					t.Fatal(err)
				}
				if err := s.EnqueueTask(ctx, v.Identity, stage, 0); err != nil {
					t.Fatal(err)
				}
				now := libraryNow
				if status != "queued" {
					old := claimFixture(t, s, stage, now)
					if status == "retry_wait" {
						if err := s.FailTask(ctx, old, "retry_wait", "retry", now, now); err != nil {
							t.Fatal(err)
						}
					} else {
						now = now.Add(2 * time.Minute)
					}
				}
				if err := s.EnqueueTask(ctx, v.Identity, "relevance", 1); err != nil {
					t.Fatal(err)
				}
				fresh := claimFixture(t, s, "relevance", now)
				if fresh.Generation != 1 {
					t.Fatalf("claimed obsolete task: %+v", fresh)
				}
				rel := library.Relevance{Level: "unrelated", Rationale: "current generation rejects"}
				if err := s.CompleteTask(ctx, fresh, library.Completion{Relevance: &rel}, now); err != nil {
					t.Fatal(err)
				}
				if got, err := s.ClaimTaskQuery(ctx, stage, now, time.Minute, library.Query{}, &v.Identity); !errors.Is(err, library.ErrNotFound) {
					t.Fatalf("obsolete claim=%+v err=%v", got, err)
				}
				// 其他论文的有效任务仍能前进。
				other := versionFixture(2, "v1")
				if err := s.UpsertVersion(ctx, other); err != nil {
					t.Fatal(err)
				}
				if err := s.EnqueueTask(ctx, other.Identity, stage, 0); err != nil {
					t.Fatal(err)
				}
				got := claimFixture(t, s, stage, now)
				if got.Identity != other.Identity {
					t.Fatalf("other version blocked: %+v", got)
				}
			})
		}
	}
}
