package redact

import (
	"context"
	"fmt"
	"log/slog"
)

// LogHandler wraps a slog.Handler so every record's message and string
// (and error) attributes pass through String before they are written.
// It is the log-plane counterpart of the API encoder and the tool
// gateway (epic #326 S08): the mailbox monitor, its review engine and
// the mail adapter log through it, so a value that ever reached a log
// line by mistake — an app password inside a provider error, a token in
// a URL — is replaced rather than recorded.
//
// It redacts secret-SHAPED values. It does not make message content
// safe to log: the mail paths log identifiers, counts and fixed
// categories only, never a subject, sender or body, and their capture
// tests hold them to that.
type LogHandler struct{ next slog.Handler }

// NewLogHandler wraps next.
func NewLogHandler(next slog.Handler) *LogHandler { return &LogHandler{next: next} }

// Logger wraps an existing logger's handler; nil wraps slog.Default().
func Logger(l *slog.Logger) *slog.Logger {
	if l == nil {
		l = slog.Default()
	}
	if _, ok := l.Handler().(*LogHandler); ok {
		return l
	}
	return slog.New(NewLogHandler(l.Handler()))
}

// Enabled reports whether next handles level.
func (h *LogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle redacts the record and passes it on.
func (h *LogHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, String(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

// WithAttrs redacts attributes bound to a derived logger.
func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = redactAttr(a)
	}
	return &LogHandler{next: h.next.WithAttrs(clean)}
}

// WithGroup passes the group on.
func (h *LogHandler) WithGroup(name string) slog.Handler {
	return &LogHandler{next: h.next.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, String(v.String()))
	case slog.KindGroup:
		group := v.Group()
		clean := make([]any, len(group))
		for i, g := range group {
			clean[i] = redactAttr(g)
		}
		return slog.Group(a.Key, clean...)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, String(x.Error()))
		case fmt.Stringer:
			return slog.String(a.Key, String(x.String()))
		case []byte:
			return slog.String(a.Key, String(string(x)))
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}
