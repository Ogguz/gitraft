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
// Returns the encoder's error untouched. The expected failure mode is a
// write error to a closed pipe (e.g., `gitraft --json | head -n 1`); the
// caller — typically cmd/gitraft/main.go — should fall back to stderr in
// that case so the user gets *some* signal before the process exits.
//
// The map-literal form previously used in main.go is intentionally NOT
// preserved here: marshalling a typed struct with explicit JSON tags makes
// the schema grep-able and prevents typos in field names ("levle" instead
// of "level") from compiling silently.
func WriteExitError(w io.Writer, err error) error {
	return json.NewEncoder(w).Encode(ExitErrorEvent{
		Level: "ERROR",
		Msg:   "gitraft exited with error",
		Error: redact.String(err.Error()),
	})
}
