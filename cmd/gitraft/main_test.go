package main_test

// Integration test for cmd/gitraft's --json exit-error path.
//
// We invoke a compiled gitraft binary as a subprocess rather than calling
// main() in-process, because:
//
//  1. main() calls os.Exit, which is hostile to in-process testing.
//  2. We need to observe os.Stdout vs os.Stderr separately — capturing
//     them in-process via os.Pipe swap would race against process startup.
//  3. The test exercises the actual binary contract (exit code, stream
//     selection, NDJSON shape) that scripts will rely on.
//
// We compile once in TestMain (vs `go run` per case) because `go run`
// emits its own "exit status 1" line on stderr when the subprocess fails
// non-zero, which would pollute the assertion that gitraft's stderr is
// empty under --json. Compiling and exec'ing the binary directly gives
// clean stream observations.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitraftBin is the absolute path to the compiled binary, populated by
// TestMain. Each test exec's this directly so stderr only carries
// gitraft's own output (not `go run`'s noise).
var gitraftBin string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "gitraft-bin")
	if err != nil {
		panic("MkdirTemp: " + err.Error())
	}
	defer os.RemoveAll(tmp)

	gitraftBin = filepath.Join(tmp, "gitraft")
	build := exec.Command("go", "build", "-o", gitraftBin, "./cmd/gitraft")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		panic("go build failed: " + err.Error() + "\n" + string(out))
	}
	os.Exit(m.Run())
}

// runGitraft invokes the compiled binary with args and returns captured
// stdout, stderr, and exit error.
func runGitraft(t *testing.T, args ...string) (stdout, stderr []byte, exitErr error) {
	t.Helper()
	cmd := exec.Command(gitraftBin, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	exitErr = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), exitErr
}

// TestExitError_JSONMode_RoutesToStdout pins the public --json contract:
// when gitraft exits with a non-zero status under --json, the failure
// is emitted as one NDJSON event on stdout (NOT stderr), matches the
// ExitErrorEvent shape, and the binary exits non-zero.
//
// We trigger the error path via `migrate --non-interactive` (no args) —
// resolveSourceDest errors on missing args + non-interactive context,
// which propagates up to the exit handler.
func TestExitError_JSONMode_RoutesToStdout(t *testing.T) {
	stdout, stderr, exitErr := runGitraft(t, "--json", "migrate", "--non-interactive")
	if exitErr == nil {
		t.Fatal("expected non-zero exit; got success")
	}
	// Stderr must be empty under --json. (Note: `go run` itself may print
	// to stderr if compilation fails; we trust the build is healthy because
	// other tests pass. A real failure here would mean the JSON path leaked.)
	if len(stderr) != 0 {
		t.Errorf("--json must keep stderr empty; got %q", stderr)
	}
	out := strings.TrimSpace(string(stdout))
	if out == "" {
		t.Fatal("expected NDJSON event on stdout; got empty")
	}
	var ev struct {
		Level string `json:"level"`
		Msg   string `json:"msg"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &ev); err != nil {
		t.Fatalf("stdout is not valid JSON: %v (out=%q)", err, out)
	}
	if ev.Level != "ERROR" {
		t.Errorf("level: got %q want ERROR", ev.Level)
	}
	if ev.Msg != "gitraft exited with error" {
		t.Errorf("msg: got %q want %q", ev.Msg, "gitraft exited with error")
	}
	if ev.Error == "" {
		t.Error("error field must be populated")
	}
	if !strings.Contains(ev.Error, "two URL arguments required") {
		t.Errorf("error field should describe the underlying failure; got %q", ev.Error)
	}
}

// TestExitError_DefaultMode_RoutesToStderr is the symmetric control:
// without --json, the failure goes to stderr in the human-readable
// "gitraft: <msg>" form, and stdout stays empty. Guards against a
// regression that flips the routing for both modes.
func TestExitError_DefaultMode_RoutesToStderr(t *testing.T) {
	stdout, stderr, exitErr := runGitraft(t, "migrate", "--non-interactive")
	if exitErr == nil {
		t.Fatal("expected non-zero exit; got success")
	}
	if len(stdout) != 0 {
		t.Errorf("default mode must keep stdout empty; got %q", stdout)
	}
	errStr := string(stderr)
	if !strings.Contains(errStr, "gitraft:") {
		t.Errorf("expected 'gitraft:' prefix on stderr; got %q", errStr)
	}
	if !strings.Contains(errStr, "two URL arguments required") {
		t.Errorf("stderr should describe the underlying failure; got %q", errStr)
	}
}

// TestExitError_JSONMode_RedactsURL covers Critical C3 from the silent-
// failure review: the redact.String wrap on the final error message must
// not be dropped. We trigger an error whose message contains a HTTPS URL
// with embedded credentials by passing a malformed --gitlab-url (empty
// host after userinfo). parseHostFromURL formats the error with the URL
// quoted via %q; cli.WriteExitError then must scrub the userinfo before
// encoding to JSON.
func TestExitError_JSONMode_RedactsURL(t *testing.T) {
	leakingURL := "https://x-access-token:FAKE_TEST_TOKEN_DO_NOT_BLOCK@"
	stdout, _, exitErr := runGitraft(t, "--json", "migrate", "--non-interactive",
		"--gitlab-url="+leakingURL,
		"https://gitlab.com/a/b.git", "https://github.com/x/y.git")
	if exitErr == nil {
		t.Fatal("expected non-zero exit on malformed --gitlab-url; got success")
	}
	out := string(stdout)
	if strings.Contains(out, "FAKE_TEST_TOKEN_DO_NOT_BLOCK") {
		t.Errorf("URL userinfo must be redacted in JSON exit event; got %q", out)
	}
}
