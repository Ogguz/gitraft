package redact

import (
	"context"
	"fmt"
	"log/slog"
)

// handler wraps an slog.Handler and redacts both the message and any string
// attributes (and string-like Any/Group attributes) flowing through it.
// URL userinfo is stripped from any HTTP(S) URLs in messages and string
// attribute values; attributes whose key looks sensitive ([Sensitive]) have
// their value replaced with "[redacted]".
//
// The wrapper preserves the inner handler's level filtering and grouping
// behavior. Group/with-attrs additions are themselves redacted before being
// forwarded — note this is one-way: the inner handler stores the redacted
// values, so future log lines reference them without ability to recover the
// originals (which is the desired property for credentials in long-lived
// loggers).
//
// The type is unexported on purpose — [New] returns the slog.Handler
// interface, so callers cannot accidentally construct a zero-value Handler
// (which would carry a nil inner and panic on first record).
type handler struct {
	inner slog.Handler
}

// New wraps inner so subsequent records pass through the redaction filter.
// Panics if inner is nil — callers are expected to compose with a real
// handler such as slog.NewTextHandler.
func New(inner slog.Handler) slog.Handler {
	if inner == nil {
		panic("redact.New: inner handler must not be nil")
	}
	return &handler{inner: inner}
}

// Enabled forwards to the inner handler — redaction doesn't change which
// levels are emitted.
func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle redacts the record's message and attributes, then forwards to inner.
func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	rec := slog.NewRecord(r.Time, r.Level, String(r.Message), r.PC)
	r.Attrs(func(attr slog.Attr) bool {
		rec.AddAttrs(redactAttr(attr))
		return true
	})
	return h.inner.Handle(ctx, rec)
}

// WithAttrs returns a new redacting handler whose downstream attrs are
// pre-redacted before storage in the inner handler.
func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = redactAttr(a)
	}
	return &handler{inner: h.inner.WithAttrs(redacted)}
}

// WithGroup forwards group naming verbatim; group names aren't typically
// sensitive and redacting them would defeat their purpose.
func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{inner: h.inner.WithGroup(name)}
}

// redactAttr applies the redaction policy to a single slog.Attr. Handles:
//
//   - sensitive keys → "[redacted]"
//   - string values → URL-redacted via [String]
//   - Any values that wrap an error or fmt.Stringer → unwrap and redact
//     the resulting string (closes the slog.Any("err", err) leak path)
//   - Group values → recurse so nested members are redacted
//   - other kinds (number, duration, etc.) → pass through unchanged
func redactAttr(attr slog.Attr) slog.Attr {
	if Sensitive(attr.Key) {
		return slog.String(attr.Key, "[redacted]")
	}
	switch attr.Value.Kind() {
	case slog.KindString:
		return slog.String(attr.Key, String(attr.Value.String()))
	case slog.KindAny:
		v := attr.Value.Any()
		if e, ok := v.(error); ok {
			return slog.String(attr.Key, String(e.Error()))
		}
		if s, ok := v.(fmt.Stringer); ok {
			return slog.String(attr.Key, String(s.String()))
		}
		return attr
	case slog.KindGroup:
		members := attr.Value.Group()
		redacted := make([]any, 0, len(members)*2)
		for _, m := range members {
			r := redactAttr(m)
			redacted = append(redacted, slog.Any(r.Key, r.Value.Any()))
		}
		return slog.Group(attr.Key, redacted...)
	}
	return attr
}
