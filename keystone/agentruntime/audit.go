package agentruntime

import (
	"context"
	"strconv"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// Audit event types. Every tool call produces tool.requested and then
// exactly one of tool.denied, tool.result, tool.failed or tool.timeout;
// every job ends with a job.state event carrying a settled state.
const (
	AuditJobState      = "job.state"
	AuditToolRequested = "tool.requested"
	AuditToolDenied    = "tool.denied"
	AuditToolResult    = "tool.result"
	AuditToolFailed    = "tool.failed"
	AuditToolTimeout   = "tool.timeout"
	AuditInvalidAction = "action.invalid"
)

// AuditEvent is one append-only audit record.
type AuditEvent struct {
	JobID   string
	Runtime string
	Type    string
	State   State
	Tool    string
	CallID  string
	Actor   string
	// Detail is a short, already-bounded explanation. Tool output is
	// never copied here: it can be large and it is the gateway's to
	// redact, not the audit log's to store.
	Detail string
	At     time.Time
}

// Recorder is where the audit trail goes. Adapters fail closed: when
// Record returns an error before a tool call, the call does not run and
// the job fails.
type Recorder interface {
	Record(ctx context.Context, e AuditEvent) error
}

// RecorderFunc adapts a function to Recorder.
type RecorderFunc func(ctx context.Context, e AuditEvent) error

func (f RecorderFunc) Record(ctx context.Context, e AuditEvent) error { return f(ctx, e) }

// EventAppender is the slice of *runtimedb.DB the event recorder needs.
type EventAppender interface {
	AppendEvent(e runtimedb.Event) (int64, error)
}

// EventLogRecorder writes the audit trail to the runtime database's
// append-only event stream, where the Console and notifications already
// read from (ADR-W007 A4).
type EventLogRecorder struct {
	Events EventAppender
}

func (r EventLogRecorder) Record(_ context.Context, e AuditEvent) error {
	attrs := map[string]string{
		"job":     e.JobID,
		"runtime": e.Runtime,
	}
	if e.State != "" {
		attrs["state"] = string(e.State)
	}
	if e.Tool != "" {
		attrs["tool"] = e.Tool
	}
	if e.CallID != "" {
		attrs["call"] = e.CallID
	}
	if e.Actor != "" {
		attrs["actor"] = e.Actor
	}
	if !e.At.IsZero() {
		attrs["at"] = strconv.FormatInt(e.At.UnixMilli(), 10)
	}
	_, err := r.Events.AppendEvent(runtimedb.Event{
		Type:    "agent." + e.Type,
		Kind:    "agent-job",
		Name:    e.JobID,
		Message: e.Detail,
		Attrs:   attrs,
	})
	return err
}
