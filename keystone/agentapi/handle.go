package agentapi

import "strings"

// Handle is the Wildstone-issued task handle a runtime carries on every
// capability, memory and work call (ADR-W039 §3.1 principle 4, §4
// item 2). It is a bearer: whoever presents it, together with a runtime
// credential bound to the right Agent, acts within the task it names.
//
// Its form is `<taskID>.<secret>`. The task id before the first dot is
// public — it is what audit rows, traces and log lines name — and
// everything after it is the secret Wildstone checks (a digest of it is
// what the task row keeps, never the secret itself). A Handle without a
// dot is treated as entirely secret, so an unexpected shape is redacted
// rather than leaked.
//
// It is a string type rather than a struct so it survives any transport
// unchanged: a header, a JSON field, a URL segment. Nothing in this
// package parses it beyond the split String needs; what a handle
// resolves to is Wildstone's business at the moment of use.
type Handle string

// IsZero reports whether the handle is absent. A call carrying a zero
// handle has no task, and Wildstone refuses it before any resolution.
func (h Handle) IsZero() bool { return h == "" }

// TaskID is the public part of the handle: the task it names, safe to
// log and to key audit by. Empty when the handle is zero or has no
// public part.
func (h Handle) TaskID() string {
	id, _, ok := strings.Cut(string(h), ".")
	if !ok {
		return ""
	}
	return id
}

// String renders the handle for logs and errors with the secret part
// elided. It is the method fmt and slog reach for, so a handle that
// finds its way into a message by accident still does not print its
// secret. Callers that need the bearer value on the wire convert with
// string(h) explicitly, which is the point: leaking has to be spelled
// out.
func (h Handle) String() string {
	if h.IsZero() {
		return "handle()"
	}
	id := h.TaskID()
	if id == "" {
		return "handle(…)"
	}
	return "handle(" + id + ".…)"
}
