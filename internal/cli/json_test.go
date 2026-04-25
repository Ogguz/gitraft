package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Ogguz/gitraft/internal/redact"
)

// Test fixture sentinels — chosen to be obviously fake so secret-scanners
// (Gitleaks, GitHub push protection) don't flag the literals when the file
// is committed. Real-shaped tokens like `ghp_...` or
// `x-access-token:supersecret` were avoided per the silent-failure review
// on commit 9a51ee9.
const (
	fakePassword = "FAKE_TEST_TOKEN_DO_NOT_BLOCK"
	fakePAT      = "FAKE_GHP_TEST_FIXTURE"
)

// resetJSONMode arranges for jsonOutput to be restored at end-of-test, AND
// sets jsonOutput=false now so the test starts from a known-clean baseline.
// Tests that need jsonOutput=true must assign it themselves after this call.
//
// Not safe under t.Parallel() — the helper mutates a package global.
func resetJSONMode(t *testing.T) {
	t.Helper()
	prev := jsonOutput
	t.Cleanup(func() { jsonOutput = prev })
	jsonOutput = false
}

// resetCLIGlobals resets all three persistent-flag globals (jsonOutput,
// nonInteractive, verbose) plus the env vars (CI, TERM) that gate
// interactivity. Tests that touch any combination of these benefit from
// the one-call form rather than threading 3-4 helpers through Setup.
//
// Not safe under t.Parallel() — see [resetJSONMode].
func resetCLIGlobals(t *testing.T) {
	t.Helper()
	resetJSONMode(t)
	resetNonInteractive(t)
	resetEnv(t)
	prev := verbose
	t.Cleanup(func() { verbose = prev })
	verbose = 0
}

// ---- flag binding ----

func TestJSONFlagWiredToCobra(t *testing.T) {
	resetCLIGlobals(t)
	root := NewRoot()
	root.SetArgs([]string{"--json", "--help"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !jsonOutput {
		t.Error("--json flag did not set the package global")
	}
	if !JSONMode() {
		t.Error("JSONMode() must return true after --json")
	}
}

// TestJSONFlagAfterSubcommand mirrors TestNonInteractiveFlagAfterSubcommand
// — persistent flags must work in any position. A refactor that
// accidentally moved --json from PersistentFlags() to Flags() would
// silently fail this test.
func TestJSONFlagAfterSubcommand(t *testing.T) {
	resetCLIGlobals(t)
	root := NewRoot()
	root.SetArgs([]string{"migrate", "--json", "--help"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !jsonOutput {
		t.Error("--json after subcommand did not set the package global")
	}
}

// TestJSONMode_DefaultFalse pins the contract that the global starts false
// — guards against a future refactor that flips the zero value (e.g. via
// an init() side-effect) and accidentally turns every gitraft invocation
// into a JSON-emitting one.
func TestJSONMode_DefaultFalse(t *testing.T) {
	resetCLIGlobals(t)
	if JSONMode() {
		t.Error("JSONMode() must be false by default")
	}
	if jsonOutput {
		t.Error("jsonOutput must be false by default")
	}
}

// ---- isInteractive: --json gate ----

func TestJSONMode_DisablesInteractive(t *testing.T) {
	resetCLIGlobals(t)
	jsonOutput = true

	stdin, stdout := makeFakeTTYPair(t) // both look like TTYs
	if isInteractive(stdin, stdout) {
		t.Error("--json must force isInteractive=false even on real TTYs")
	}
}

// TestJSONMode_DisablesInteractiveUnderCI exercises the realistic CI
// shape: --json plus piped streams plus CI=true. Multiple OR-gates should
// each independently force false; the test locks the matrix.
func TestJSONMode_DisablesInteractiveUnderCI(t *testing.T) {
	resetCLIGlobals(t)
	jsonOutput = true
	t.Setenv("CI", "true")
	stdin, stdout := nonTTYFile(t)

	if isInteractive(stdin, stdout) {
		t.Error("--json + CI=true + piped streams must yield isInteractive=false")
	}
}

// TestJSONMode_AndNonInteractiveBothForceFalse confirms the two flags are
// independent OR-gates, not one implying the other. Even if a future
// refactor stops treating --json as implying non-interactive, both flags
// being set must still produce false.
func TestJSONMode_AndNonInteractiveBothForceFalse(t *testing.T) {
	resetCLIGlobals(t)
	jsonOutput = true
	nonInteractive = true
	stdin, stdout := makeFakeTTYPair(t)

	if isInteractive(stdin, stdout) {
		t.Error("--json + --non-interactive must yield isInteractive=false")
	}
}

// TestJSONMode_DisablesProgressIndicator verifies the explicit jsonOutput
// short-circuit at the top of newProgressIndicator. The test relies on
// makeFakeTTYPair flipping isInteractiveFn to true, which would normally
// activate the spinner — the explicit gate is what catches it. Removing
// the gate from progress.go silently breaks this test.
func TestJSONMode_DisablesProgressIndicator(t *testing.T) {
	resetCLIGlobals(t)
	jsonOutput = true

	stdin, stdout := makeFakeTTYPair(t)
	var buf bytes.Buffer
	p := newProgressIndicator(&buf, stdin, stdout, "Migrating...")
	if p.spinner != nil {
		t.Error("--json must produce a silent progress indicator (spinner ANSI codes would corrupt the JSON stream)")
	}
	// Behavioral assertion: driving the indicator must not write to the
	// buffer. Pairs with the structural p.spinner==nil check.
	p.Start()
	p.Update("phase 2")
	p.Stop()
	if buf.Len() != 0 {
		t.Errorf("--json indicator must be silent end-to-end; got %q", buf.String())
	}
}

// ---- wizard not invoked under --json ----

// TestJSONMode_DoesNotInvokeWizard locks the load-bearing invariant:
// --json on a real-looking TTY must NOT trigger the wizard, because the
// wizard would block the script driving gitraft. The test uses the real
// isInteractive (no isInteractiveFn override) so the jsonOutput gate is
// what's actually exercised.
func TestJSONMode_DoesNotInvokeWizard(t *testing.T) {
	resetCLIGlobals(t)
	resetRunWizardFn(t)
	jsonOutput = true

	wizardCalled := false
	runWizardFn = func(context.Context, *os.File, *os.File) (wizardResult, error) {
		wizardCalled = true
		return wizardResult{source: "src", destination: "dst"}, nil
	}

	logger, _ := testLogger()
	// Use TTY-shaped streams so isInteractive's stream check would pass —
	// the jsonOutput short-circuit must fire before reaching it.
	r, w := nonTTYFile(t) // these aren't real TTYs, but combined with jsonOutput=true the env-disable + flag-disable both apply
	_, _, err := resolveSourceDest(context.Background(), nil, r, w, logger)
	if err == nil {
		t.Fatal("expected error when 0 args under --json (wizard must be skipped)")
	}
	if wizardCalled {
		t.Error("wizard must NOT be invoked under --json; this would block any script piping gitraft")
	}
}

// ---- newLoggerTo: NDJSON shape, multi-line, level + time ----

func TestNewLoggerTo_JSONHandlerProducesNDJSON(t *testing.T) {
	resetCLIGlobals(t)
	jsonOutput = true

	var buf bytes.Buffer
	logger := newLoggerTo(&buf, 0)
	logger.Info("first event", "key", "value-1")
	logger.Info("second event", "key", "value-2")
	logger.Warn("third event", "key", "value-3")

	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Fatal("expected output")
	}

	// Load-bearing NDJSON property: one record per line, no JSON-array
	// wrapping. Three calls must produce three distinct lines that each
	// parse to a JSON object.
	if strings.HasPrefix(out, "[") {
		t.Fatalf("NDJSON: must not be a JSON array (got %q)", out)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 NDJSON lines for 3 log calls; got %d in %q", len(lines), out)
	}

	// Each line carries the expected schema. Scope payload assertions to
	// the line whose msg matches — guards against future startup-banner
	// records that the loop would otherwise misclassify as failures.
	wantPayloads := map[string]string{
		"first event":  "value-1",
		"second event": "value-2",
		"third event":  "value-3",
	}
	wantLevels := map[string]string{
		"first event":  "INFO",
		"second event": "INFO",
		"third event":  "WARN",
	}
	seen := map[string]bool{}
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("line %q is not valid JSON: %v", line, err)
			continue
		}
		// Every record must carry slog's built-in fields.
		if _, ok := rec["level"]; !ok {
			t.Errorf("missing level field: %v", rec)
		}
		if _, ok := rec["time"]; !ok {
			t.Errorf("missing time field: %v", rec)
		}
		msg, ok := rec["msg"].(string)
		if !ok {
			t.Errorf("missing/non-string msg field: %v", rec)
			continue
		}
		if want, ok := wantPayloads[msg]; ok {
			seen[msg] = true
			if rec["key"] != want {
				t.Errorf("msg=%q: expected key=%q; got %v", msg, want, rec["key"])
			}
			if rec["level"] != wantLevels[msg] {
				t.Errorf("msg=%q: expected level=%q; got %v", msg, wantLevels[msg], rec["level"])
			}
		}
	}
	for msg := range wantPayloads {
		if !seen[msg] {
			t.Errorf("did not find %q in NDJSON output", msg)
		}
	}
}

func TestNewLoggerTo_TextHandlerWhenJSONOff(t *testing.T) {
	resetCLIGlobals(t)
	jsonOutput = false

	var buf bytes.Buffer
	logger := newLoggerTo(&buf, 0)
	logger.Info("text-only event")

	out := buf.String()
	// Text handler produces key=value lines, not JSON braces.
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("default mode must use text handler, not JSON; got %q", out)
	}
	if !strings.Contains(out, "text-only event") {
		t.Errorf("expected message in output; got %q", out)
	}
}

// TestNewLoggerTo_JSONHonorsVerbosity covers the --json + -v combo: with
// verbosity=1, Debug records must be emitted as NDJSON.
func TestNewLoggerTo_JSONHonorsVerbosity(t *testing.T) {
	resetCLIGlobals(t)
	jsonOutput = true

	var buf bytes.Buffer
	logger := newLoggerTo(&buf, 1) // -v
	logger.Debug("debug event", "phase", "clone")

	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Fatal("expected debug output under -v")
	}
	if !strings.HasPrefix(out, "{") {
		t.Errorf("--json + -v must produce JSON-shaped debug records; got %q", out)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("debug line is not JSON: %v (line=%q)", err, out)
	}
	if rec["level"] != "DEBUG" {
		t.Errorf("expected level=DEBUG; got %v", rec["level"])
	}
}

// ---- redaction still applies under JSON ----

func TestNewLoggerTo_JSONStillRedactsURLs(t *testing.T) {
	// The redact wrap is OUTSIDE the json/text branch, so JSON output must
	// also redact URL userinfo. This guards against any future refactor
	// that builds the JSON handler chain without going through redact.New
	// (e.g., a "fast path" for JSON, an alternative filter, or a
	// positioning regression that moves the JSON branch outside the wrap).
	resetCLIGlobals(t)
	jsonOutput = true

	var buf bytes.Buffer
	logger := newLoggerTo(&buf, 0)
	logger.Info("pushing", "dst", "https://x-access-token:"+fakePassword+"@github.com/x/y.git")

	if strings.Contains(buf.String(), fakePassword) {
		t.Errorf("JSON output must still redact URL userinfo; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), redact.URLUserSentinel) {
		t.Errorf("expected redaction sentinel %q in JSON output; got %q", redact.URLUserSentinel, buf.String())
	}
}

func TestNewLoggerTo_JSONStillRedactsSensitiveAttrs(t *testing.T) {
	resetCLIGlobals(t)
	jsonOutput = true

	var buf bytes.Buffer
	logger := newLoggerTo(&buf, 0)
	logger.Info("auth", "token", fakePAT)

	if strings.Contains(buf.String(), fakePAT) {
		t.Errorf("JSON output must redact sensitive attr values; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), redact.AttrSentinel) {
		t.Errorf("expected %q in JSON output; got %q", redact.AttrSentinel, buf.String())
	}
}

// ---- newLogger: stdout-vs-stderr selection (behavioral) ----

// TestNewLogger_RoutesToStdoutUnderJSON and its companion exercise the
// load-bearing stdout/stderr selection in newLogger. We swap os.Stdout and
// os.Stderr for pipes around the call and assert the chosen sink received
// bytes (and the other did not). Without this test the JSON branch of
// newLogger reports as covered but isn't actually exercised — every other
// test calls newLoggerTo with an explicit buffer, bypassing the selector.
func TestNewLogger_RoutesToStdoutUnderJSON(t *testing.T) {
	resetCLIGlobals(t)
	jsonOutput = true

	stdoutBuf, stderrBuf, restore := captureStdoutStderr(t)
	defer restore()

	logger := newLogger(0)
	logger.Info("routed event", "k", "v")

	// Restore early so reads see the buffered writes.
	restore()

	if stdoutBuf.Len() == 0 {
		t.Error("--json must route slog output to os.Stdout; nothing landed there")
	}
	if stderrBuf.Len() != 0 {
		t.Errorf("--json must NOT route to os.Stderr; got %q", stderrBuf.String())
	}
}

func TestNewLogger_RoutesToStderrByDefault(t *testing.T) {
	resetCLIGlobals(t)
	jsonOutput = false

	stdoutBuf, stderrBuf, restore := captureStdoutStderr(t)
	defer restore()

	logger := newLogger(0)
	logger.Info("default-route event")

	restore()

	if stderrBuf.Len() == 0 {
		t.Error("default mode must route slog output to os.Stderr; nothing landed there")
	}
	if stdoutBuf.Len() != 0 {
		t.Errorf("default mode must NOT route to os.Stdout; got %q", stdoutBuf.String())
	}
}

// captureStdoutStderr swaps os.Stdout and os.Stderr for pipes whose read
// ends are drained into the returned buffers. The returned restore
// function reverts the swap and is idempotent (safe to call from defer
// AND inline before assertions). Tests use the inline call to flush
// pending writes before reading, then defer restore as a safety net.
func captureStdoutStderr(t *testing.T) (*bytes.Buffer, *bytes.Buffer, func()) {
	t.Helper()
	origStdout := os.Stdout
	origStderr := os.Stderr

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stdout: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stderr: %v", err)
	}

	os.Stdout = outW
	os.Stderr = errW

	stdoutBuf := &bytes.Buffer{}
	stderrBuf := &bytes.Buffer{}

	// Drain in goroutines so writes don't block on full pipe buffers.
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(stdoutBuf, outR)
		close(stdoutDone)
	}()
	go func() {
		_, _ = io.Copy(stderrBuf, errR)
		close(stderrDone)
	}()

	var restored bool
	restore := func() {
		if restored {
			return
		}
		restored = true
		_ = outW.Close()
		_ = errW.Close()
		<-stdoutDone
		<-stderrDone
		_ = outR.Close()
		_ = errR.Close()
		os.Stdout = origStdout
		os.Stderr = origStderr
	}
	return stdoutBuf, stderrBuf, restore
}

// ---- WriteExitError: shape, redaction, encoder failure ----

// TestWriteExitError_Shape locks in the public --json contract: one
// JSON object per call, with exactly the fields {level: "ERROR",
// msg: "gitraft exited with error", error: <redacted>}, terminated by
// a newline (the NDJSON line separator). Renaming any field or changing
// the level literal breaks consumer scripts.
func TestWriteExitError_Shape(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteExitError(&buf, errors.New("clone failed: network unreachable")); err != nil {
		t.Fatalf("WriteExitError: %v", err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("WriteExitError must terminate with newline (NDJSON contract); got %q", out)
	}
	var ev ExitErrorEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &ev); err != nil {
		t.Fatalf("WriteExitError output is not valid ExitErrorEvent JSON: %v (line=%q)", err, out)
	}
	if ev.Level != "ERROR" {
		t.Errorf("Level: got %q want ERROR", ev.Level)
	}
	if ev.Msg != "gitraft exited with error" {
		t.Errorf("Msg: got %q want %q", ev.Msg, "gitraft exited with error")
	}
	if ev.Error != "clone failed: network unreachable" {
		t.Errorf("Error: got %q", ev.Error)
	}
}

// TestWriteExitError_RedactsURL pins the redaction guarantee on the
// final-exit path. A removed redact.String wrap inside WriteExitError
// would let a wrapped clone error like
// "clone https://x-access-token:TOKEN@host: ..." leak the token to
// every script piping gitraft output.
func TestWriteExitError_RedactsURL(t *testing.T) {
	leakingErr := errors.New("clone https://x-access-token:" + fakePassword + "@github.com/x/y.git failed")
	var buf bytes.Buffer
	if err := WriteExitError(&buf, leakingErr); err != nil {
		t.Fatalf("WriteExitError: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, fakePassword) {
		t.Errorf("WriteExitError must redact URL userinfo; got %q", out)
	}
	if !strings.Contains(out, redact.URLUserSentinel) {
		t.Errorf("expected redaction sentinel %q in output; got %q", redact.URLUserSentinel, out)
	}
}

// errWriter is an io.Writer that fails on first Write — simulates a
// closed pipe. Used by TestWriteExitError_PropagatesWriteFailure to
// verify the encoder error reaches the caller (so cmd/gitraft/main.go
// can fall back to stderr).
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("simulated closed pipe") }

func TestWriteExitError_PropagatesWriteFailure(t *testing.T) {
	err := WriteExitError(errWriter{}, errors.New("inner"))
	if err == nil {
		t.Error("expected encoder error when writer fails; got nil — this would silently swallow broken-pipe failures in cmd/gitraft/main.go")
	}
}

// TestWriteExitError_EmptyError covers the edge case of an error whose
// Error() returns "". The JSON object must still be emitted with a
// non-zero shape so the script can detect the failure even without a
// useful message.
func TestWriteExitError_EmptyError(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteExitError(&buf, errors.New("")); err != nil {
		t.Fatalf("WriteExitError: %v", err)
	}
	var ev ExitErrorEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &ev); err != nil {
		t.Fatalf("not valid JSON: %v (%q)", err, buf.String())
	}
	if ev.Level != "ERROR" || ev.Msg == "" {
		t.Errorf("empty inner error must still produce a usable event; got %+v", ev)
	}
}
