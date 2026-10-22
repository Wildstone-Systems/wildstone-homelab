package domestic

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// Event types the forwarder delivers. Backup and drill passes share
// backup.failed; the pass kind rides in the event's attrs.
const (
	forwardMorningType      = "report.morning"
	forwardBackupFailedType = "backup.failed"
)

// forwardBatch bounds one poll so a burst cannot starve the loop.
const forwardBatch = 100

// EventSource is the slice of the runtime event stream the forwarder
// reads. *runtimedb.DB satisfies it.
type EventSource interface {
	LatestEventSeq() (int64, error)
	EventsAfter(after int64, limit int) ([]runtimedb.Event, error)
}

// EventForwarder is an outbound-only consumer of the operational event
// stream: it publishes the morning summary and backup/drill failures to
// one ntfy topic. It adds no API surface and no persistence.
//
// Delivery is at-most-once. The cursor starts at the stream head when
// Run begins, so a restart never replays history, and a failed publish
// is logged and skipped rather than blocking later events.
type EventForwarder struct {
	Source   EventSource
	Notifier *Notifier
	// Topic is the ntfy topic to publish to. Required.
	Topic string
	// Interval between polls; defaults to 15s.
	Interval time.Duration
	Log      *slog.Logger
}

// Enabled reports whether the forwarder has everything it needs; with
// no ntfy URL or topic configured it stays off and installs are
// unchanged.
func (f *EventForwarder) Enabled() bool {
	return f != nil && f.Source != nil && f.Notifier.Available() && strings.TrimSpace(f.Topic) != ""
}

func (f *EventForwarder) log() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.Default()
}

// Run polls until ctx is done.
func (f *EventForwarder) Run(ctx context.Context) {
	if !f.Enabled() {
		return
	}
	cursor, err := f.Source.LatestEventSeq()
	if err != nil {
		f.log().Error("ntfy forwarder not started", "err", err)
		return
	}
	every := f.Interval
	if every <= 0 {
		every = 15 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cursor = f.Poll(ctx, cursor)
		}
	}
}

// Poll forwards matching events after cursor and returns the new
// cursor. The cursor advances past every event read, delivered or not.
func (f *EventForwarder) Poll(ctx context.Context, cursor int64) int64 {
	events, err := f.Source.EventsAfter(cursor, forwardBatch)
	if err != nil {
		f.log().Warn("ntfy forwarder: reading events", "err", err)
		return cursor
	}
	for _, e := range events {
		cursor = e.Seq
		params, ok := f.params(e)
		if !ok {
			continue
		}
		if _, err := f.Notifier.Execute(ctx, Call{Tool: "notifications.notify", Tier: "domestic", Params: params, Rule: "event-forwarder"}); err != nil {
			f.log().Warn("ntfy forwarder: publish failed", "type", e.Type, "seq", e.Seq, "err", err)
		}
	}
	return cursor
}

// params builds the publish for one event from its existing summary
// fields only. The backup failure detail (a raw error string) is left
// out on purpose: the plan name and pass kind say what to look at.
func (f *EventForwarder) params(e runtimedb.Event) (map[string]string, bool) {
	switch e.Type {
	case forwardMorningType:
		return map[string]string{"topic": f.Topic, "title": "Morning report", "message": nonEmpty(e.Message, "Morning report is ready."), "tags": "sunrise"}, true
	case forwardBackupFailedType:
		kind := e.Attrs["kind"]
		title := "Backup failed"
		if kind != "" {
			title = fmt.Sprintf("Backup %s failed", kind)
		}
		return map[string]string{"topic": f.Topic, "title": title, "message": nonEmpty(e.Message, "A backup pass failed."), "priority": "4", "tags": "warning"}, true
	}
	return nil, false
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
