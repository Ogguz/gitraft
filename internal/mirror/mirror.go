// Package mirror performs a full git repository mirror migration:
// clone the source with --mirror, then push --mirror to the destination.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
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

	logger.Info("pushing to destination", "dst", opts.Destination)
	if err := runner.Run(ctx, "git", "-C", tmp, "push", "--mirror", opts.Destination); err != nil {
		logger.Warn("push failed; temporary clone retained for inspection", "dir", tmp)
		return fmt.Errorf("push to %s: %w", opts.Destination, err)
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
