package cli

import (
	"encoding/json"
	"io"

	"github.com/Ogguz/gitraft/internal/redact"
)

// ExitErrorEvent is the NDJSON record gitraft emits on stdout when --json
// mode is active and the run exits with a non-zero status.
//
// The field shape is part of the public --json contract: scripts use
// `jq 'select(.level=="ERROR") | .error'` to extract the failure from a
// piped run. Field names match slog's JSON-handler output (`level`, `msg`)
// so consumers can use a single parser for both ordinary log lines and the
// final exit event.
//
// Renaming any field — or changing the `Level` literal away from "ERROR" —
// is a breaking change. There is no `time` field today; ordinary slog log
// lines DO carry one, and consumers that grep for it on log lines must be
// resilient to its absence on this event.
//
// The struct is exported so the schema is grep-able from outside the cli
// package; the constructor [WriteExitError] is the only sanctioned way to
// emit one (so the redaction wrap can never be skipped).
type ExitErrorEvent struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Error string `json:"error"`
}

// WriteExitError encodes a single [ExitErrorEvent] to w as one NDJSON line
// (one JSON object followed by a newline — the encoding json.Encoder
// applies by default). The error message is run through [redact.String]
// so HTTP(S) URL userinfo is scrubbed before reaching the consumer.
//
// Multi-line errors. Operational errors emit a `\nhint: <action>` preamble
// for next-step guidance. In the JSON wire form that newline encodes as
// the two-character escape `\n` inside the `.error` string — consumers
// must JSON-decode the value (e.g., `jq -r '.error'`) to recover the
// human-readable multi-line form. A naive `jq '.error'` (no -r) would
// pass the literal escape downstream; that's a consumer-side concern,
// but the contract is part of the --json schema and renaming or
// flattening the newline would be a breaking change.
//
// Caveat. [redact.String] only handles `https?://userinfo@host` patterns.
// SSH URLs of the form `ssh://user:pass@host/...` pass through unchanged
// — the upstream call sites are responsible for stripping SSH credentials
// (e.g., via [redact.URL]) before they reach this function. This gap is
// pre-existing and intentionally left as a hard requirement on callers
// rather than a silent regex-broadening here.
//
// The scp-like SSH form (`git@host:path/repo.git`) is also not handled by
// the regex — but conventionally carries no credential (`git@` is just a
// username prefix, not a token), so the pass-through is intentional. A
// future maintainer who "improves" the regex to cover this form would
// break the conventional case without buying any new redaction.
//
// Returns the encoder's error untouched. The expected failure mode is a
// write error to a closed pipe (e.g., `gitraft --json | head -n 1`); the
// caller — typically cmd/gitraft/main.go — should fall back to stderr in
// that case so the user gets *some* signal before the process exits.
//
// The map-literal form previously used in main.go is intentionally NOT
// preserved here: marshalling a typed struct with explicit JSON tags makes
// the schema grep-able and prevents typos in field names (e.g. "lvl" or
// "lev") from compiling silently.
func WriteExitError(w io.Writer, err error) error {
	return json.NewEncoder(w).Encode(ExitErrorEvent{
		Level: "ERROR",
		Msg:   "gitraft exited with error",
		Error: redact.String(err.Error()),
	})
}
