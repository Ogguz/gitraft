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

// isCancellation reports whether err is a context cancellation (Ctrl+C)
// or deadline-exceeded. These cases are NOT operational failures; emitting
// remediation hints (`verify the URL is reachable`, `rerun the migration`)
// would actively mislead users who deliberately aborted the run. Each
// operational wrap below short-circuits to a hint-free wrap when this
// returns true so the user sees only `<op>: context canceled`.
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

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
		// os.MkdirTemp("", ...) defers to os.TempDir, which reads $TMPDIR
		// on Unix (Linux defaults to /tmp; macOS and many sandboxed envs
		// resolve to per-user paths under /var/folders/...) and $TMP / $TEMP
		// on Windows. The hint names the env vars without claiming a
		// universal default — those vary too much to be useful copy.
		// Not asserted in tests because forcing the failure requires
		// filesystem fault injection (read-only temp dir); manual
		// verification only.
		return fmt.Errorf("create temp dir: %w\nhint: ensure the temp directory is writable and has free space (configure via $TMPDIR on Unix or $TMP/$TEMP on Windows)", err)
	}

	logger.Info("cloning source", "src", opts.Source, "tmp", tmp)
	if err := runner.Run(ctx, "git", "clone", "--mirror", opts.Source, tmp); err != nil {
		logger.Warn("clone failed; temporary directory retained for inspection", "dir", tmp)
		// Skip the remediation hint when the user cancelled (Ctrl+C) —
		// suggesting they "verify the URL is reachable" or rerun would be
		// misleading; they aborted on purpose. Same gate is repeated at
		// every operational wrap below.
		if isCancellation(err) {
			return fmt.Errorf("clone %s: %w", opts.Source, err)
		}
		// The hint covers the two most common operational causes in one
		// line: URL reachability (DNS/proxy/firewall/typo) and token
		// access. Single-sentence form reads better than the prior split
		// "verify reachability; if reachable, verify token" — git's stderr
		// tail (attached by runner.Run) usually distinguishes the two.
		// Reproduce-command uses redact.URL so the embedded auth (set by
		// authURL upstream) is stripped before the user copy-pastes — note
		// the resulting URL has no credentials and uses the local git
		// credential helper (or SSH agent) instead.
		return fmt.Errorf("clone %s: %w\nhint: verify the URL is reachable (DNS/proxy/firewall) AND your token has read access — for SSH URLs ensure your SSH agent has the matching key (try `git ls-remote %s` to reproduce outside gitraft, using your local git credentials)", opts.Source, err, redact.URL(opts.Source))
	}

	// LFS detection runs against the cloned repo on disk; skip when the
	// runner is a stub (test/dry-run) and never wrote a real repo. Detection
	// errors (corrupt refs DB, ctx cancellation) are fatal — silently
	// proceeding without LFS migration on a real LFS-using repo would lose
	// object content at the destination.
	//
	// No `\nhint:` is appended here because detectLFS / its inner helpers
	// already attach a leaf-level hint specific to the failure mode (lfs
	// ls-files vs for-each-ref). Adding another preamble at this layer
	// produced redundant or contradictory advice — see the de-stacking note
	// at lfs.go:detectLFS.
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
			if isCancellation(err) {
				return fmt.Errorf("lfs fetch: %w", err)
			}
			// The `git lfs install` line was deliberately removed: by the
			// time we reach here, isGitLFSAvailable(ctx) returned true (run
			// `git lfs version` succeeded), which strongly implies the
			// per-user filter was already initialized. The actual common
			// causes are token LFS scope and provider-side LFS toggle.
			return fmt.Errorf("lfs fetch: %w\nhint: verify the source provider has LFS enabled and your token has read access to LFS objects (some providers gate LFS behind a separate scope, e.g. GitHub `repo` covers LFS but Bitbucket Cloud needs LFS turned on per-repo)", err)
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
		if isCancellation(err) {
			return fmt.Errorf("push to %s: %w", opts.Destination, err)
		}
		// Reproduce-command uses redact.URL on the destination so the
		// auth-embedded form (set by authURL upstream) doesn't reach the
		// user's clipboard via copy-paste. redact.String at exit DOES
		// match URLs in free-form text via its `(?i)https?://userinfo@host`
		// regex — but only HTTPS, not SSH (`ssh://user:pass@host/...` is
		// outside the regex's scheme anchor). Since opts.Destination
		// could be either form (set by authURL based on the user's input),
		// scrubbing here via redact.URL covers both schemes uniformly.
		// The redacted URL has no credentials, so it relies on the user's
		// local git credential helper or SSH agent — that's the intended
		// manual diagnostic.
		return fmt.Errorf("push to %s: %w\nhint: verify the destination token has write access and that branch protection / pre-receive hooks aren't rejecting the mirror push; the temporary clone is retained at %s — try `git -C %s push --mirror %s` to reproduce (uses your local git credentials, not the env-var token)", opts.Destination, err, tmp, tmp, redact.URL(opts.Destination))
	}

	// LFS push happens AFTER the regular mirror push so refs already exist
	// at the destination — git lfs push needs that to know what to upload.
	if lfsActive {
		logger.Info("pushing LFS objects", "dst", opts.Destination)
		if err := runner.Run(ctx, "git", "-C", tmp, "lfs", "push", "--all", opts.Destination); err != nil {
			logger.Warn("LFS push failed; temporary clone retained for inspection", "dir", tmp)
			if isCancellation(err) {
				return fmt.Errorf("lfs push to %s: %w", opts.Destination, err)
			}
			return fmt.Errorf("lfs push to %s: %w\nhint: ensure Git LFS is enabled at the destination and your token has LFS write permission — provider gotchas: Bitbucket Cloud needs LFS toggled per-repo AND workspace LFS quota remaining (Free 1 GB / Standard 5 GB / Premium 10 GB); GitLab needs lfs_enabled=true on the project; GitHub/Gitea/Bitbucket Server: usually no per-repo LFS toggle but verify Git LFS is enabled at the instance level", opts.Destination, err)
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
