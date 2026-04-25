package cli

import (
	"io"
	"os"
	"sync"
	"time"

	"github.com/Ogguz/gitraft/internal/redact"
	"github.com/briandowns/spinner"
)

// progressSuffix builds the spinner suffix shown next to the rotating glyph.
// It uses [redact.URL] (not [redact.String]) so credentials embedded in any
// URL shape — `https://`, `git+ssh://`, `ssh://user:pass@host/...` — are
// stripped before they reach the visible terminal line. The HTTP-only regex
// in redact.String would silently leak SSH passwords here.
//
// Extracted as a package-level function so unit tests can lock in the
// redaction contract without standing up a full progressIndicator.
func progressSuffix(srcRaw, dstRaw string) string {
	return "Migrating " + redact.URL(srcRaw) + " → " + redact.URL(dstRaw)
}

// progressIndicator wraps a CLI spinner with a no-op fallback for
// non-interactive environments (CI, piped stdout, --non-interactive, --json,
// redirected stderr).
//
// In interactive mode the spinner shows a rotating glyph + suffix on stderr
// to give users "something is happening" feedback during long operations
// (clone, lfs fetch, push). In non-interactive mode the indicator's methods
// are no-ops and progress is conveyed through slog Info logs instead — see
// [newLoggerTo] for the verbosity ladder.
//
// CONTRACT for callers: every prog.Start/prog.Update call MUST be paired
// with a logger.Info that conveys the same phase information, because in
// silent mode the spinner is the only channel that goes dark. A future
// maintainer adding a new phase like prog.Update("starting LFS fetch")
// without a paired logger.Info will leave --json users (and CI runs)
// without visibility into that phase. Today every callsite in
// migrate.go/mirror.Run follows this contract; future code must too.
//
// The spinner and slog both write to stderr; in interactive mode they can
// briefly interleave when slog emits a Warn (e.g., a submodule warning).
// That's an acceptable trade-off for v0.4: the spinner redraws on its next
// tick, and warnings are short.
//
// TODO (post-v1): with three behavioral states (active spinner, silent,
// future "emit progress as JSON events"), promote progressIndicator to an
// interface with concrete spinnerIndicator/noopIndicator/jsonIndicator
// impls. Today the nil-spinner sentinel works because there are only two
// states. See type-design review on commit 9a51ee9.
type progressIndicator struct {
	// mu serializes Start/Stop/Update across goroutines. The wrapper's
	// callers today are sequential (main goroutine drives the migration
	// loop), but mirror.Run may invoke progress callbacks from worker
	// goroutines in future versions; the mutex future-proofs that path. It
	// also prevents a torn write if Update races with Stop — briandowns/
	// spinner's Suffix field has no internal synchronization, so the
	// surrounding lock is the only thing keeping the read in the spinner
	// goroutine consistent with our writes.
	mu      sync.Mutex
	spinner *spinner.Spinner // nil when non-interactive — methods become no-ops
}

// newProgressIndicator returns an indicator that writes a spinner to w when
// (a) stdin/stdout are TTYs and the runtime isn't in non-interactive mode
// (CI, TERM=dumb, or --non-interactive), AND (b) when w is a *os.File, that
// file is also a TTY. Otherwise the indicator is silent and callers can
// rely on slog Info logs for progress feedback.
//
// The writer-TTY gate is what handles `gitraft migrate ... 2>migration.log`
// correctly: stdin/stdout still point at the user's terminal so
// [isInteractive] is true, but stderr is a regular file — drawing a spinner
// there would scatter ANSI escape sequences across the log. For non-file
// writers (e.g., bytes.Buffer in tests) the writer gate is bypassed; tests
// drive the active path via the [isInteractiveFn] seam.
//
// stdin/stdout are passed in (rather than reaching for os globals) so tests
// can substitute pipe-backed files — symmetric with [isInteractive]. The
// indirection through [isInteractiveFn] (rather than calling [isInteractive]
// directly) lets tests force the active path without spinning up a real
// pty, mirroring the wizard's seam.
func newProgressIndicator(w io.Writer, stdin, stdout *os.File, msg string) *progressIndicator {
	// Belt-and-suspenders: --json mode silences the spinner unconditionally
	// because spinner ANSI control sequences would corrupt the NDJSON
	// stream on stdout. In production this branch is redundant —
	// [isInteractive] (the implementation isInteractiveFn points at) also
	// returns false when jsonOutput is set, so the next gate would catch
	// it. The explicit check matters in tests: makeFakeTTYPair (used by
	// TestJSONMode_DisablesProgressIndicator) overrides isInteractiveFn
	// with a stub that ignores jsonOutput, so without this gate the
	// spinner would activate in that test. Removing this line would
	// silently break that test — keep both gates.
	if jsonOutput {
		return &progressIndicator{}
	}
	if !isInteractiveFn(stdin, stdout) {
		return &progressIndicator{}
	}
	if f, ok := w.(*os.File); ok && !isTTY(f) {
		return &progressIndicator{}
	}
	s := spinner.New(spinner.CharSets[14], 100*time.Millisecond, spinner.WithWriter(w))
	s.Suffix = " " + msg
	return &progressIndicator{spinner: s}
}

// Start begins the spinner animation. No-op when the indicator is silent.
func (p *progressIndicator) Start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.spinner != nil {
		p.spinner.Start()
	}
}

// Stop halts the spinner and clears its line. No-op when silent. Safe to
// call multiple times — briandowns/spinner's Stop handles the running/
// not-running state internally.
//
// Stop signals the spinner goroutine to exit and waits for it to
// acknowledge; in briandowns/spinner v1.23.2 that handshake completes
// within roughly one tick (~100ms with our config) of the call returning.
// Callers should not assume the line is cleared the instant Stop returns
// — for the purposes of "the spinner is no longer drawing" the contract
// holds, but a downstream writer to the same stream may race a final
// erase sequence the spinner's goroutine emitted on its way out. In
// practice this is invisible because we Stop on defer and exit shortly
// after.
func (p *progressIndicator) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.spinner != nil {
		p.spinner.Stop()
	}
}

// Update changes the spinner suffix to reflect a new phase. No-op when
// silent. Holds the mutex to avoid racing with Start/Stop on the same
// indicator (the underlying briandowns/spinner mutates its Suffix field
// without internal locking).
func (p *progressIndicator) Update(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.spinner != nil {
		p.spinner.Suffix = " " + msg
	}
}
