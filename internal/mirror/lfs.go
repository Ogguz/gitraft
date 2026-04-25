package mirror

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrGitLFSMissing is returned when an LFS-using repo can't be migrated
// because git-lfs isn't installed locally. Carries OS-specific install
// hints. Callers (e.g., the CLI) can detect this case via:
//
//	if errors.Is(err, mirror.ErrGitLFSMissing) { ... }
var ErrGitLFSMissing = errGitLFSMissing{}

// errGitLFSMissing is the sentinel-style error type. Empty struct so its
// equality identity comes from the type, not a value, which lets errors.Is
// match by type even when wrapped by fmt.Errorf("...: %w", err).
type errGitLFSMissing struct{}

func (errGitLFSMissing) Error() string {
	return "source repository uses Git LFS but git-lfs is not installed.\n\n" +
		"Install git-lfs:\n" + lfsInstallHint() + "\n\n" +
		"Then run `git lfs install` once to enable it for your user."
}

// Is matches any errGitLFSMissing — supports errors.Is(err, ErrGitLFSMissing).
func (errGitLFSMissing) Is(target error) bool {
	_, ok := target.(errGitLFSMissing)
	return ok
}

// isGitLFSAvailable is a package-level var (not const-bound) so tests can
// override the LFS-presence check without invoking real git-lfs. Distinguishes
// context cancellation from "not installed" so callers don't misreport
// transient cancels as a missing tool.
var isGitLFSAvailable = func(ctx context.Context) bool {
	err := exec.CommandContext(ctx, "git", "lfs", "version").Run()
	if err == nil {
		return true
	}
	// If the command was cancelled, we can't know whether git-lfs is installed.
	// Treat as "available" so the caller propagates the cancel via the next
	// command's ctx check, rather than spuriously reporting LFS missing.
	if ctx.Err() != nil {
		return true
	}
	return false
}

// runLFSLsFiles is overridable for tests that want to inject an error path
// without relying on a corrupt repo on disk.
var runLFSLsFiles = func(ctx context.Context, repoDir string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "lfs", "ls-files", "--all").Output()
	if err != nil && ctx.Err() != nil {
		// Surface cancellation distinctly so callers can errors.Is(ctx.Canceled).
		return nil, ctx.Err()
	}
	return out, err
}

// detectLFS reports whether the mirrored repo at repoDir uses Git LFS.
//
// Detection strategy:
//
//  1. If git-lfs is installed locally, run `git lfs ls-files --all` — the
//     authoritative signal that covers every ref accurately.
//  2. Otherwise, fall back to a heuristic: scan HEAD and every branch and
//     tag's .gitattributes for `filter=lfs`. The user gets the install
//     hint when migration is attempted regardless.
//
// Returns (false, nil) when repoDir is empty or not a mirror clone (e.g.,
// during dry-run where the clone never actually happened — the caller's
// guard short-circuits to avoid running detection in unsupported states).
//
// Detection helpers in this package use exec directly rather than the
// CommandRunner injected via Options.Runner — runner is for the *write-side*
// operations (clone, push, lfs fetch) that dry-run must skip; detection is
// read-only and gated by looksLikeMirrorClone.
func detectLFS(ctx context.Context, repoDir string) (bool, error) {
	if !looksLikeMirrorClone(repoDir) {
		return false, nil
	}
	if isGitLFSAvailable(ctx) {
		out, err := runLFSLsFiles(ctx, repoDir)
		if err != nil {
			return false, fmt.Errorf("git lfs ls-files: %w", err)
		}
		return len(strings.TrimSpace(string(out))) > 0, nil
	}
	used, err := detectLFSViaAttributes(ctx, repoDir)
	if err != nil {
		return false, fmt.Errorf("scan .gitattributes for LFS: %w", err)
	}
	return used, nil
}

// looksLikeMirrorClone returns true when repoDir contains a HEAD file (the
// minimum signal that git clone --mirror succeeded). Renamed from the more
// generic-sounding "looksLikeBareRepo" to make the intent — "did the mirror
// clone actually populate this dir?" — explicit.
func looksLikeMirrorClone(repoDir string) bool {
	_, err := os.Stat(filepath.Join(repoDir, "HEAD"))
	return err == nil
}

// detectLFSViaAttributes scans HEAD and every branch and tag for a
// `.gitattributes` blob containing `filter=lfs`. Returns true at the first
// match.
//
// Errors from `for-each-ref` (corrupt refs DB, permission, etc.) propagate
// so the caller can fail loudly rather than silently treating "can't list
// refs" as "no LFS detected" — a real LFS repo with damaged refs would
// otherwise migrate without object content.
//
// `git show` errors per ref are NOT propagated — they're typically just
// "this ref's tree has no .gitattributes," which is the common case.
func detectLFSViaAttributes(ctx context.Context, repoDir string) (bool, error) {
	refs, err := listScanRefs(ctx, repoDir)
	if err != nil {
		return false, fmt.Errorf("list refs: %w", err)
	}
	refs = append([]string{"HEAD"}, refs...)
	for _, ref := range refs {
		out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "show", ref+":.gitattributes").Output()
		if err != nil {
			// Surface cancellation immediately.
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			// Most other errors here mean ".gitattributes doesn't exist on
			// this ref" — the common case. Skip and keep scanning.
			continue
		}
		if looksLikeLFSAttributes(string(out)) {
			return true, nil
		}
	}
	return false, nil
}

// looksLikeLFSAttributes is the substring check broken out for direct testing.
func looksLikeLFSAttributes(content string) bool {
	return strings.Contains(content, "filter=lfs")
}

// listScanRefs returns refs/heads/* and refs/tags/* names. Returns an error
// when the listing fails — callers must distinguish "ref enumeration failed"
// from "repo has no refs."
func listScanRefs(ctx context.Context, repoDir string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "for-each-ref",
		"--format=%(refname)", "refs/heads/", "refs/tags/").Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}

// lfsInstallHint returns the OS-appropriate install command for git-lfs.
func lfsInstallHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "  brew install git-lfs"
	case "linux":
		return "  sudo apt install git-lfs    (Debian/Ubuntu)\n" +
			"  sudo dnf install git-lfs    (Fedora/RHEL)\n" +
			"  see https://git-lfs.com for other distros"
	case "windows":
		return "  choco install git-lfs       (or download from https://git-lfs.com)"
	default:
		return "  see https://git-lfs.com"
	}
}

// Compile-time assertion that errGitLFSMissing satisfies errors.Is matching.
var _ = errors.Is(errGitLFSMissing{}, ErrGitLFSMissing)
