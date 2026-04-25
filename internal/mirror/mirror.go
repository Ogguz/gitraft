// Package mirror performs a full git repository mirror migration:
// clone the source with --mirror, then push --mirror to the destination.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/Ogguz/gitraft/internal/redact"
)

// Options configures a single mirror migration.
type Options struct {
	Source      string
	Destination string
	DryRun      bool
	Cleanup     bool
	Logger      *slog.Logger
	// Runner overrides the default command runner. Mainly useful in tests.
	// When nil, an exec-based runner is used (or a dry-run runner when DryRun is true).
	Runner CommandRunner
}

// Run executes a mirror migration from opts.Source to opts.Destination.
func Run(ctx context.Context, opts Options) error {
	if opts.Source == "" || opts.Destination == "" {
		return errors.New("source and destination are required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	runner := opts.Runner
	if runner == nil {
		runner = defaultRunner(opts.DryRun, logger)
	}

	tmp, err := os.MkdirTemp("", "gitraft-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}

	logger.Info("cloning source", "src", opts.Source, "tmp", tmp)
	if err := runner.Run(ctx, "git", "clone", "--mirror", opts.Source, tmp); err != nil {
		logger.Warn("clone failed; temporary directory retained for inspection", "dir", tmp)
		return fmt.Errorf("clone %s: %w", opts.Source, err)
	}

	// LFS detection runs against the cloned repo on disk; skip when the
	// runner is a stub (test/dry-run) and never wrote a real repo. Detection
	// errors (corrupt refs DB, ctx cancellation) are fatal — silently
	// proceeding without LFS migration on a real LFS-using repo would lose
	// object content at the destination.
	lfsActive, err := detectLFS(ctx, tmp)
	if err != nil {
		logger.Warn("LFS detection failed; temporary clone retained for inspection", "dir", tmp)
		return fmt.Errorf("detect LFS: %w", err)
	}
	if lfsActive {
		if !isGitLFSAvailable(ctx) {
			logger.Warn("Git LFS required but not installed; temporary clone retained for inspection", "dir", tmp)
			return ErrGitLFSMissing
		}
		logger.Info("Git LFS detected; fetching object content", "tmp", tmp)
		if err := runner.Run(ctx, "git", "-C", tmp, "lfs", "fetch", "--all"); err != nil {
			logger.Warn("LFS fetch failed; temporary clone retained for inspection", "dir", tmp)
			return fmt.Errorf("lfs fetch: %w", err)
		}
	}

	// Submodules: warn that they're not recursively migrated. Errors here are
	// logged but non-fatal — submodule warnings are advisory, not migration-
	// critical. URLs are redacted so embedded credentials don't leak to logs.
	mods, _, smErr := listSubmodules(ctx, tmp)
	if smErr != nil {
		logger.Warn("submodule listing failed; submodule warnings may be incomplete", "err", smErr)
	}
	for _, m := range mods {
		logger.Warn("submodule not recursively migrated; only the parent's reference is preserved",
			"path", m.Path, "url", redact.URL(m.URL))
	}

	logger.Info("pushing to destination", "dst", opts.Destination)
	if err := runner.Run(ctx, "git", "-C", tmp, "push", "--mirror", opts.Destination); err != nil {
		logger.Warn("push failed; temporary clone retained for inspection", "dir", tmp)
		return fmt.Errorf("push to %s: %w", opts.Destination, err)
	}

	// LFS push happens AFTER the regular mirror push so refs already exist
	// at the destination — git lfs push needs that to know what to upload.
	if lfsActive {
		logger.Info("pushing LFS objects", "dst", opts.Destination)
		if err := runner.Run(ctx, "git", "-C", tmp, "lfs", "push", "--all", opts.Destination); err != nil {
			logger.Warn("LFS push failed; temporary clone retained for inspection", "dir", tmp)
			return fmt.Errorf("lfs push to %s: %w", opts.Destination, err)
		}
	}

	// Dry-run creates an empty tmp dir; always clean it up.
	// Real runs retain tmp by default so the user can investigate; --cleanup removes it.
	if opts.DryRun || opts.Cleanup {
		logger.Info("cleaning up", "dir", tmp)
		if err := os.RemoveAll(tmp); err != nil {
			// Cleanup failure must not mask a successful migration.
			logger.Warn("cleanup failed (migration succeeded)", "dir", tmp, "err", err)
		}
	} else {
		logger.Info("temporary clone retained", "dir", tmp)
	}
	return nil
}
