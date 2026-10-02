package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/state"
)

type libraryRunnerFuncs struct {
	collect func(context.Context) error
	process func(context.Context) error
}

func (r libraryRunnerFuncs) Collect(ctx context.Context) error { return r.collect(ctx) }
func (r libraryRunnerFuncs) Process(ctx context.Context) error { return r.process(ctx) }

func TestLibraryCLIRejectsInvalidArgsBeforeConfigOrDatabase(t *testing.T) {
	for _, command := range [][]string{
		{"collect", "extra"}, {"process", "extra"}, {"retry"}, {"retry", "0"}, {"retry", "-1"}, {"retry", "abc"},
		{"library-status", "extra"}, {"reanalyze", "2501.12345", "v0"}, {"reanalyze", "2501.12345v1", "v1"},
		{"reanalyze", "invalid", "v1"}, {"backup-library"}, {"restore-library", "backup"}, {"demo", "extra"},
	} {
		err := run(append([]string{"--config", filepath.Join(t.TempDir(), "missing.json")}, command...))
		if err == nil || !strings.Contains(err.Error(), "用法:") {
			t.Fatalf("args=%v err=%v", command, err)
		}
	}
}

func TestLibraryCollectAndProcessIgnoreDeliverySwitchAndWebhook(t *testing.T) {
	for _, command := range []string{"collect", "process"} {
		t.Run(command, func(t *testing.T) {
			cfg := serveConfig(filepath.Join(t.TempDir(), "library.db"), false)
			cfg.Topics = nil
			cfg.Library.CollectEnabled = command == "process"
			cfg.Library.ProcessEnabled = command == "collect"
			if command == "collect" {
				cfg.Anthropic.APIKey = ""
			}
			called := ""
			var shared *state.Store
			err := runLibraryCommandWithDependencies(context.Background(), cfg, []string{command}, libraryCommandDependencies{
				NewRunner: func(actual config.Config, store *state.Store, _ libraryProcessFilter) (libraryRunner, error) {
					if actual.Library.CollectEnabled != (command == "collect") || actual.Library.ProcessEnabled != (command == "process") {
						t.Errorf("one-shot command used serve switches: %+v", actual.Library)
					}
					shared = store
					return libraryRunnerFuncs{
						collect: func(ctx context.Context) error { called = "collect"; return store.Health(ctx) },
						process: func(ctx context.Context) error { called = "process"; return store.Health(ctx) },
					}, nil
				},
			})
			if err != nil || called != command {
				t.Fatalf("called=%s err=%v", called, err)
			}
			if err := shared.Health(context.Background()); err == nil {
				t.Fatal("command left store open")
			}
		})
	}
}

func TestLibraryProcessValidatesAPIKeyBeforeOpeningDatabase(t *testing.T) {
	cfg := serveConfig(filepath.Join(t.TempDir(), "missing", "library.db"), false)
	cfg.Anthropic.APIKey = ""
	err := runLibraryCommandWithDependencies(context.Background(), cfg, []string{"process"}, libraryCommandDependencies{
		NewRunner: func(config.Config, *state.Store, libraryProcessFilter) (libraryRunner, error) {
			t.Fatal("process called factory before validating API key")
			return nil, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "anthropic.api_key") {
		t.Fatalf("err=%v", err)
	}
}

func TestLibraryRunnerStartupErrorsCloseStore(t *testing.T) {
	for _, kind := range []string{"factory", "nil", "operation"} {
		t.Run(kind, func(t *testing.T) {
			cfg := serveConfig(filepath.Join(t.TempDir(), "library.db"), false)
			var shared *state.Store
			expected := errors.New("fixture failure")
			err := runLibraryCommandWithDependencies(context.Background(), cfg, []string{"process"}, libraryCommandDependencies{
				NewRunner: func(_ config.Config, store *state.Store, _ libraryProcessFilter) (libraryRunner, error) {
					shared = store
					switch kind {
					case "factory":
						return nil, expected
					case "nil":
						return nil, nil
					default:
						return libraryRunnerFuncs{process: func(context.Context) error { return expected }}, nil
					}
				},
			})
			if err == nil || (kind != "nil" && !errors.Is(err, expected)) {
				t.Fatalf("kind=%s err=%v", kind, err)
			}
			if err := shared.Health(context.Background()); err == nil {
				t.Fatal("runner failure left store open")
			}
		})
	}
}

func TestRetryPreservesCheckpointAndReanalyzeCreatesNewGeneration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "library.db")
	cfg := serveConfig(path, false)
	cfg.Anthropic.APIKey = ""
	cfg.Topics = nil
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	identity := library.Identity{Source: "arxiv", PaperID: "2501.12345", Version: "v2"}
	if err := store.UpsertVersion(ctx, library.Version{Identity: identity, Title: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueTask(ctx, identity, "analyze", 4); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	task, err := store.ClaimTask(ctx, "analyze", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := json.RawMessage(`{"requests":12,"tokens":4096,"remaining":"saved"}`)
	if err := store.SaveCheckpoint(ctx, task, checkpoint, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveChunk(ctx, task, library.Chunk{DocumentID: "doc", DocumentHash: "hash", BlockID: "block"}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.FailTask(ctx, task, "paused", "budget", time.Time{}, now); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if err := runLibraryCommand(ctx, cfg, []string{"retry", fmt.Sprint(task.ID)}); err != nil {
		t.Fatal(err)
	}
	if err := runLibraryCommand(ctx, cfg, []string{"reanalyze", "arxiv:2501.12345", "v2"}); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	retried, err := store.GetTask(ctx, task.ID)
	if err != nil || retried.Status != "queued" || retried.Generation != 4 || retried.Attempt != task.Attempt || !bytes.Equal(retried.Checkpoint, checkpoint) {
		t.Fatalf("retry=%+v err=%v", retried, err)
	}
	chunks, err := store.Chunks(ctx, task)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("chunks=%v err=%v", chunks, err)
	}
	detail, err := store.LibraryDetail(ctx, identity)
	if err != nil || len(detail.Tasks) != 2 {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	newTask := detail.Tasks[0]
	if newTask.Stage != "metadata" || newTask.Generation != 5 || newTask.Status != "queued" || len(newTask.Checkpoint) != 0 {
		t.Fatalf("new task=%+v", newTask)
	}
	if err := runLibraryCommand(ctx, cfg, []string{"reanalyze", "2501.12345", "v1"}); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("missing version=%v", err)
	}
}

func TestProcessFilterValidationAndForwarding(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want libraryProcessFilter
	}{
		{nil, libraryProcessFilter{}},
		{[]string{"--id", "arxiv:2501.12345", "--version", "v2"}, libraryProcessFilter{Identity: &library.Identity{Source: "arxiv", PaperID: "2501.12345", Version: "v2"}}},
		{[]string{"--version", "v1", "--id", "hep-th/9901001"}, libraryProcessFilter{Identity: &library.Identity{Source: "arxiv", PaperID: "hep-th/9901001", Version: "v1"}}},
		{[]string{"--batch", "cs.IR/2026-09-29"}, libraryProcessFilter{Query: library.Query{Batch: "cs.IR/2026-09-29"}}},
		{[]string{"--batch", "2026-09-29"}, libraryProcessFilter{Query: library.Query{Batch: "2026-09-29"}}},
	} {
		cfg := serveConfig(filepath.Join(t.TempDir(), "library.db"), false)
		called := false
		err := runLibraryCommandWithDependencies(context.Background(), cfg, append([]string{"process"}, tc.args...), libraryCommandDependencies{
			NewRunner: func(_ config.Config, _ *state.Store, filter libraryProcessFilter) (libraryRunner, error) {
				called = true
				if !reflect.DeepEqual(filter, tc.want) {
					t.Errorf("filter=%+v want=%+v", filter, tc.want)
				}
				return libraryRunnerFuncs{process: func(context.Context) error { return nil }}, nil
			},
		})
		if err != nil || !called {
			t.Fatalf("args=%v called=%v err=%v", tc.args, called, err)
		}
	}
	for _, args := range [][]string{
		{"--id", "2501.12345"}, {"--version", "v1"}, {"--id", "2501.12345", "--version", "v0"},
		{"--id", "2501.12345", "--version", "v1", "--batch", "2026-09-29"},
		{"--batch", "2026-02-30"}, {"--batch", "/2026-09-29"}, {"--batch", "cs.IR/2026-09-29/extra"},
		{"--batch", "bad category/2026-09-29"}, {"--batch", "2026-09-29", "--batch", "2026-09-30"},
		{"--id", "2501.12345v1", "--version", "v1"}, {"--id=2501.12345", "--version", "v1"},
	} {
		if _, err := parseProcessQuery(args); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
}

func TestLibraryBackupRestoreCLIUsesIsolatedDirectoryWithoutConfig(t *testing.T) {
	ctx := context.Background()
	path := sendingDatabase(t)
	cfg := serveConfig(path, false)
	if err := os.MkdirAll(cfg.Library.DocumentDir, 0700); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if err := runLibraryCommand(ctx, cfg, []string{"backup-library", backup}); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := run([]string{"--config", filepath.Join(t.TempDir(), "missing.json"), "restore-library", backup, restored}); err != nil {
		t.Fatal(err)
	}
	for _, database := range []string{path, filepath.Join(backup, "database.db"), filepath.Join(restored, "database.db")} {
		store, err := state.Open(database)
		if err != nil {
			t.Fatal(err)
		}
		saved, err := store.GetJob(ctx, job.Topic, "2026-09-29")
		store.Close()
		if err != nil || saved.Status != "sending" {
			t.Fatalf("database=%s saved=%+v err=%v", database, saved, err)
		}
	}
	if err := runLibraryCommand(ctx, cfg, []string{"backup-library", backup}); err == nil {
		t.Fatal("backup replaced existing target")
	}
	if err := run([]string{"restore-library", backup, restored}); err == nil {
		t.Fatal("restore replaced existing target")
	}
}

func TestLibraryStatusDoesNotRecoverDeliverySending(t *testing.T) {
	path := sendingDatabase(t)
	args := configArgs(t, path, "", "", false, "library-status")
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	saved, err := store.GetJob(context.Background(), "recommendation-advertising-search", "2026-09-29")
	if err != nil || saved.Status != "sending" {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
}
