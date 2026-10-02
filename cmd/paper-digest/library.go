package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/shichao-wang/paper-digest/internal/archive"
	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/pipeline"
	"github.com/shichao-wang/paper-digest/internal/state"
)

type libraryRunner interface {
	Collect(context.Context) error
	Process(context.Context) error
}

type libraryProcessFilter struct {
	Query    library.Query
	Identity *library.Identity
}

type libraryCommandDependencies struct {
	NewRunner func(config.Config, *state.Store, libraryProcessFilter) (libraryRunner, error)
}

func isLibraryCommand(command string) bool {
	switch command {
	case "collect", "process", "retry", "library-status", "reanalyze", "backup-library", "restore-library", "demo":
		return true
	}
	return false
}

func validateLibraryArgs(args []string) error {
	if len(args) == 0 {
		return errors.New("缺少 Library 命令")
	}
	usage := "用法: paper-digest "
	switch args[0] {
	case "process":
		_, err := parseProcessQuery(args[1:])
		return err
	case "collect", "library-status", "demo":
		if len(args) == 1 {
			return nil
		}
		usage += args[0]
	case "retry":
		if len(args) == 2 {
			id, err := strconv.ParseInt(args[1], 10, 64)
			if err == nil && id > 0 {
				return nil
			}
		}
		usage += "retry <正整数 taskID>"
	case "reanalyze":
		if len(args) == 3 {
			if err := libraryIdentity(args[1], args[2]).Validate(); err == nil {
				return nil
			}
		}
		usage += "reanalyze <arXiv id> <vN>"
	case "backup-library":
		if len(args) == 2 && strings.TrimSpace(args[1]) != "" {
			return nil
		}
		usage += "backup-library <未存在的备份目录>"
	case "restore-library":
		if len(args) == 3 && strings.TrimSpace(args[1]) != "" && strings.TrimSpace(args[2]) != "" {
			return nil
		}
		usage += "restore-library <备份目录> <未存在的新目录>"
	default:
		return fmt.Errorf("未知 Library 命令 %q", args[0])
	}
	return errors.New(usage)
}

func parseProcessQuery(args []string) (libraryProcessFilter, error) {
	invalid := errors.New("用法: paper-digest process [--id <arXiv id> --version <vN> | --batch <YYYY-MM-DD 或 category/date>]")
	filter := libraryProcessFilter{}
	if len(args) == 0 {
		return filter, nil
	}
	values := make(map[string]string)
	for i := 0; i < len(args); i += 2 {
		if i+1 >= len(args) || (args[i] != "--id" && args[i] != "--version" && args[i] != "--batch") || values[args[i]] != "" || strings.TrimSpace(args[i+1]) == "" {
			return filter, invalid
		}
		values[args[i]] = args[i+1]
	}
	if batch := values["--batch"]; batch != "" {
		if len(values) != 1 {
			return filter, invalid
		}
		category, date, hasCategory := strings.Cut(batch, "/")
		if !hasCategory {
			date = category
		}
		if !state.ValidDate(date) || (hasCategory && !regexp.MustCompile(`^[a-z]+(?:\.[A-Z]{2})?$`).MatchString(category)) {
			return filter, invalid
		}
		filter.Query.Batch = batch
		return filter, nil
	}
	if len(values) != 2 || values["--id"] == "" || values["--version"] == "" {
		return filter, invalid
	}
	identity := libraryIdentity(values["--id"], values["--version"])
	if err := identity.Validate(); err != nil {
		return filter, invalid
	}
	filter.Identity = &identity
	return filter, nil
}

func libraryIdentity(id, version string) library.Identity {
	return library.Identity{Source: "arxiv", PaperID: strings.TrimPrefix(id, "arxiv:"), Version: version}
}

func validateLibraryProcessing(cfg config.Config) error {
	if strings.TrimSpace(cfg.Anthropic.APIKey) == "" {
		return errors.New("处理论文库需要 anthropic.api_key")
	}
	return nil
}

func defaultLibraryWorker(cfg config.Config, store *state.Store) (worker, error) {
	return pipeline.New(cfg, store)
}

// runLibraryCommand is shared by the CLI dispatcher and the offline demo entry point.
func runLibraryCommand(ctx context.Context, cfg config.Config, args []string) error {
	return runLibraryCommandWithDependencies(ctx, cfg, args, libraryCommandDependencies{})
}

func runLibraryCommandWithDependencies(ctx context.Context, cfg config.Config, args []string, deps libraryCommandDependencies) error {
	if err := validateLibraryArgs(args); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if args[0] == "restore-library" {
		return archive.Restore(ctx, args[1], args[2])
	}
	if args[0] == "demo" {
		return seedDemo(ctx, cfg)
	}
	if args[0] == "collect" || args[0] == "process" {
		if err := cfg.ValidateLibrary(); err != nil {
			return err
		}
	}
	if args[0] == "process" {
		if err := validateLibraryProcessing(cfg); err != nil {
			return err
		}
	}
	path, err := cfg.DatabasePath()
	if err != nil {
		return err
	}
	store, err := state.Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	switch args[0] {
	case "collect", "process":
		// 单次命令明确选择动作，不受 serve 的自动运行开关影响。
		cfg.Library.CollectEnabled = args[0] == "collect"
		cfg.Library.ProcessEnabled = args[0] == "process"
		filter := libraryProcessFilter{}
		if args[0] == "process" {
			filter, _ = parseProcessQuery(args[1:])
		}
		factory := deps.NewRunner
		if factory == nil {
			factory = func(cfg config.Config, store *state.Store, filter libraryProcessFilter) (libraryRunner, error) {
				runner, err := pipeline.New(cfg, store)
				if err == nil {
					runner.Query = filter.Query
					runner.IdentityFilter = filter.Identity
				}
				return runner, err
			}
		}
		runner, err := factory(cfg, store, filter)
		if err != nil {
			return err
		}
		if runner == nil {
			return errors.New("library runner initialization returned nil")
		}
		if args[0] == "collect" {
			return runner.Collect(ctx)
		}
		return runner.Process(ctx)
	case "retry":
		id, _ := strconv.ParseInt(args[1], 10, 64)
		return store.RetryTask(ctx, id)
	case "library-status":
		status, err := store.LibraryStatus(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(status)
	case "reanalyze":
		identity := libraryIdentity(args[1], args[2])
		detail, err := store.LibraryDetail(ctx, identity)
		if err != nil {
			return err
		}
		generation := 0
		for _, task := range detail.Tasks {
			if task.Generation > generation {
				generation = task.Generation
			}
		}
		if generation == int(^uint(0)>>1) {
			return errors.New("Library generation 已达到上限")
		}
		return store.EnqueueTask(ctx, identity, "metadata", generation+1)
	case "backup-library":
		return archive.Backup(ctx, store, cfg.Library.DocumentDir, args[1])
	}
	return nil
}
