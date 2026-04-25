package mirror_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Ogguz/gitraft/internal/mirror"
)

func TestRun_MirrorsRefsBetweenBareRepos(t *testing.T) {
	requireGit(t)

	src := makeBareWithHistory(t)
	dst := makeEmptyBare(t)

	err := mirror.Run(context.Background(), mirror.Options{
		Source:      src,
		Destination: dst,
		Cleanup:     true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := listRefs(t, dst)
	for _, want := range []string{"refs/heads/main", "refs/tags/v1.0"} {
		if !slices.Contains(got, want) {
			t.Errorf("destination missing %s; got %v", want, got)
		}
	}
}

func TestRun_DryRunDoesNotTouchDestination(t *testing.T) {
	requireGit(t)

	src := makeBareWithHistory(t)
	dst := makeEmptyBare(t)

	err := mirror.Run(context.Background(), mirror.Options{
		Source:      src,
		Destination: dst,
		DryRun:      true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := listRefs(t, dst); len(got) != 0 {
		t.Errorf("dry-run pushed refs; got %v", got)
	}
}

func TestRun_RequiresSource(t *testing.T) {
	err := mirror.Run(context.Background(), mirror.Options{Destination: "/tmp/dst"})
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestRun_RequiresDestination(t *testing.T) {
	err := mirror.Run(context.Background(), mirror.Options{Source: "/tmp/src"})
	if err == nil {
		t.Fatal("expected error for missing destination")
	}
}

func TestRun_CloneFailureIsWrappedWithSourceURL(t *testing.T) {
	stub := &stubRunner{errs: map[int]error{0: errors.New("boom")}}
	err := mirror.Run(context.Background(), mirror.Options{
		Source:      "src-url",
		Destination: "dst-url",
		Runner:      stub,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "src-url") {
		t.Errorf("error missing source URL: %v", err)
	}
	if len(stub.calls) != 1 {
		t.Errorf("push should not run after clone failure; got %d calls", len(stub.calls))
	}
	_ = os.RemoveAll(stub.cloneTarget())
}

func TestRun_PushFailureRetainsTmpEvenWithCleanup(t *testing.T) {
	stub := &stubRunner{errs: map[int]error{1: errors.New("boom")}}
	err := mirror.Run(context.Background(), mirror.Options{
		Source:      "src",
		Destination: "dst-url",
		Runner:      stub,
		Cleanup:     true,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "dst-url") {
		t.Errorf("error missing destination URL: %v", err)
	}
	if _, statErr := os.Stat(stub.cloneTarget()); statErr != nil {
		t.Errorf("tmp should be retained on push failure for investigation; stat err: %v", statErr)
	}
	_ = os.RemoveAll(stub.cloneTarget())
}

func TestRun_CleanupFalseRetainsTmp(t *testing.T) {
	stub := &stubRunner{}
	err := mirror.Run(context.Background(), mirror.Options{
		Source:      "src",
		Destination: "dst",
		Runner:      stub,
		Cleanup:     false,
	})
	if err != nil {
		t.Fatal(err)
	}
	tmp := stub.cloneTarget()
	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("tmp should be retained with Cleanup=false; stat err: %v", err)
	}
	_ = os.RemoveAll(tmp)
}

func TestRun_CleanupTrueRemovesTmp(t *testing.T) {
	stub := &stubRunner{}
	err := mirror.Run(context.Background(), mirror.Options{
		Source:      "src",
		Destination: "dst",
		Runner:      stub,
		Cleanup:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	tmp := stub.cloneTarget()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("tmp should be removed with Cleanup=true; stat err: %v", err)
	}
}

func TestRun_ContextCancellationPropagates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stub := &stubRunner{errs: map[int]error{0: context.Canceled}}
	err := mirror.Run(ctx, mirror.Options{
		Source:      "src",
		Destination: "dst",
		Runner:      stub,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled in chain; got %v", err)
	}
	_ = os.RemoveAll(stub.cloneTarget())
}

// ---- helpers ----

// stubRunner records each call and returns a mapped error for that call index.
type stubRunner struct {
	calls [][]string
	errs  map[int]error
}

func (r *stubRunner) Run(_ context.Context, name string, args ...string) error {
	i := len(r.calls)
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.errs[i]
}

// cloneTarget returns the tmp path from the first recorded call, which is
// always ["git", "clone", "--mirror", src, tmp].
func (r *stubRunner) cloneTarget() string {
	if len(r.calls) == 0 || len(r.calls[0]) < 5 {
		return ""
	}
	return r.calls[0][4]
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// makeBareWithHistory builds a bare repo containing:
// - main branch with two commits
// - v1.0 annotated tag on the second commit
func makeBareWithHistory(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "src.git")

	runGit(t, "", "-c", "init.defaultBranch=main", "init", work)
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "test")
	writeFile(t, filepath.Join(work, "README.md"), "hello\n")
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "first")
	writeFile(t, filepath.Join(work, "README.md"), "hello world\n")
	runGit(t, work, "commit", "-am", "second")
	runGit(t, work, "tag", "-a", "v1.0", "-m", "release 1")

	runGit(t, "", "clone", "--bare", work, bare)
	return bare
}

func makeEmptyBare(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "dst.git")
	runGit(t, "", "init", "--bare", bare)
	return bare
}

func listRefs(t *testing.T, bareRepo string) []string {
	t.Helper()
	out := runGitOut(t, bareRepo, "for-each-ref", "--format=%(refname)")
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// runGit invokes git with an isolated environment so host global/system
// config (signing, hooks, templates) can't leak into tests.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = isolatedGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = isolatedGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func isolatedGitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// makeBareWithSubmoduleConfig creates a bare repo whose HEAD commit contains
// a .gitmodules file with the given content. We don't actually
// `git submodule add` — that requires network. Just commit the file so
// listSubmodules can parse it.
func makeBareWithSubmoduleConfig(t *testing.T, gitmodules string) string {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "src.git")

	runGit(t, "", "-c", "init.defaultBranch=main", "init", work)
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "test")
	writeFile(t, filepath.Join(work, "README.md"), "hi\n")
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-m", "initial")
	writeFile(t, filepath.Join(work, ".gitmodules"), gitmodules)
	runGit(t, work, "add", ".gitmodules")
	runGit(t, work, "commit", "-m", "add submodule config")

	runGit(t, "", "clone", "--bare", work, bare)
	return bare
}

func TestRun_WarnsAboutSubmodules(t *testing.T) {
	requireGit(t)
	src := makeBareWithSubmoduleConfig(t,
		"[submodule \"vendor/dep\"]\n\tpath = vendor/dep\n\turl = https://example.com/dep.git\n")
	dst := makeEmptyBare(t)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	err := mirror.Run(context.Background(), mirror.Options{
		Source:      src,
		Destination: dst,
		Cleanup:     true,
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "submodule not recursively migrated") {
		t.Errorf("expected submodule warning in log; got %s", logged)
	}
	if !strings.Contains(logged, "vendor/dep") {
		t.Errorf("expected submodule path in log; got %s", logged)
	}
}

func TestRun_WarnsForEverySubmodule(t *testing.T) {
	requireGit(t)
	src := makeBareWithSubmoduleConfig(t,
		"[submodule \"a\"]\n\tpath = lib/a\n\turl = https://example.com/a.git\n"+
			"[submodule \"b\"]\n\tpath = lib/b\n\turl = https://example.com/b.git\n"+
			"[submodule \"c\"]\n\tpath = lib/c\n\turl = https://example.com/c.git\n")
	dst := makeEmptyBare(t)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	err := mirror.Run(context.Background(), mirror.Options{
		Source: src, Destination: dst, Cleanup: true, Logger: logger,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	logged := buf.String()
	for _, want := range []string{"lib/a", "lib/b", "lib/c"} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected warning for %q; got %s", want, logged)
		}
	}
}

func TestRun_RedactsSubmoduleCredentials(t *testing.T) {
	requireGit(t)
	src := makeBareWithSubmoduleConfig(t,
		"[submodule \"x\"]\n\tpath = lib/x\n\turl = https://alice:hunter2@example.com/x.git\n")
	dst := makeEmptyBare(t)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	err := mirror.Run(context.Background(), mirror.Options{
		Source: src, Destination: dst, Cleanup: true, Logger: logger,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	logged := buf.String()
	if strings.Contains(logged, "hunter2") {
		t.Errorf("password leaked into submodule warning log: %s", logged)
	}
	if strings.Contains(logged, "alice") {
		t.Errorf("username leaked into submodule warning log: %s", logged)
	}
	if !strings.Contains(logged, "redacted") {
		t.Errorf("expected redaction marker; got %s", logged)
	}
}

func TestRun_DryRunSilentOnLFSAndSubmodules(t *testing.T) {
	requireGit(t)
	src := makeBareWithSubmoduleConfig(t,
		"[submodule \"a\"]\n\tpath = lib/a\n\turl = https://example.com/a.git\n")
	dst := makeEmptyBare(t)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	err := mirror.Run(context.Background(), mirror.Options{
		Source:      src,
		Destination: dst,
		DryRun:      true, // dry-run: no real clone happens, so detection short-circuits
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	logged := buf.String()
	if strings.Contains(logged, "submodule not recursively migrated") {
		t.Errorf("dry-run must not log submodule warnings (no real clone); got %s", logged)
	}
	if strings.Contains(logged, "Git LFS detected") {
		t.Errorf("dry-run must not log LFS detection (no real clone); got %s", logged)
	}
}

func TestRun_NoSubmoduleWarningWhenAbsent(t *testing.T) {
	requireGit(t)
	src := makeBareWithHistory(t) // no .gitmodules
	dst := makeEmptyBare(t)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	err := mirror.Run(context.Background(), mirror.Options{
		Source:      src,
		Destination: dst,
		Cleanup:     true,
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if strings.Contains(buf.String(), "submodule not recursively migrated") {
		t.Errorf("submodule warning should not fire for repo without .gitmodules; got %s", buf.String())
	}
}
