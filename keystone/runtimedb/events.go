package runtimedb

import (
	"encoding/json"
	"fmt"
	"time"
)

// Event is one row of the append-only operational event stream (ADR-W007
// A4). Consumers: Console live view, ntfy, Butler (E3 wires producers).
type Event struct {
	Seq     int64             `json:"seq"`
	Time    time.Time         `json:"time"`
	Type    string            `json:"type"`
	Node    string            `json:"node,omitempty"`
	Kind    string            `json:"kind,omitempty"`
	Name    string            `json:"name,omitempty"`
	Message string            `json:"message,omitempty"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// AppendEvent appends one event and returns its sequence number. Time is
// stamped here; callers never supply it.
func (db *DB) AppendEvent(e Event) (int64, error) {
	if e.Type == "" {
		return 0, fmt.Errorf("runtimedb: event type required")
	}
	attrs := "{}"
	if len(e.Attrs) > 0 {
		b, err := json.Marshal(e.Attrs)
		if err != nil {
			return 0, fmt.Errorf("runtimedb: marshaling event attrs: %w", err)
		}
		attrs = string(b)
	}
	res, err := db.sql.Exec(
		`INSERT INTO events (time, type, node, kind, name, message, attrs)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		now(), e.Type, e.Node, e.Kind, e.Name, e.Message, attrs)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: appending event: %w", err)
	}
	return res.LastInsertId()
}

// EventsAfter returns up to limit events with seq > after, oldest first —
// the long-poll cursor shape the API serves (ADR-W007 K3).
func (db *DB) EventsAfter(after int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.sql.Query(
		`SELECT seq, time, type, node, kind, name, message, attrs
		 FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts, attrs string
		if err := rows.Scan(&e.Seq, &ts, &e.Type, &e.Node, &e.Kind, &e.Name, &e.Message, &attrs); err != nil {
			return nil, err
		}
		if e.Time, err = time.Parse(time.RFC3339Nano, ts); err != nil {
			return nil, fmt.Errorf("runtimedb: bad event timestamp %q: %w", ts, err)
		}
		if attrs != "{}" && attrs != "" {
			if err := json.Unmarshal([]byte(attrs), &e.Attrs); err != nil {
				return nil, fmt.Errorf("runtimedb: bad event attrs: %w", err)
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventsSince returns up to limit events with time >= since, oldest
// first — the morning report's 24h window (ADR-W017 A5). The time column
// stores RFC3339Nano UTC, so the comparison is a plain lexicographic one.
func (db *DB) EventsSince(since time.Time, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := db.sql.Query(
		`SELECT seq, time, type, node, kind, name, message, attrs
		 FROM events WHERE time >= ? ORDER BY seq ASC LIMIT ?`,
		since.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing events since %s: %w", since, err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts, attrs string
		if err := rows.Scan(&e.Seq, &ts, &e.Type, &e.Node, &e.Kind, &e.Name, &e.Message, &attrs); err != nil {
			return nil, err
		}
		if e.Time, err = time.Parse(time.RFC3339Nano, ts); err != nil {
			return nil, fmt.Errorf("runtimedb: bad event timestamp %q: %w", ts, err)
		}
		if attrs != "{}" && attrs != "" {
			if err := json.Unmarshal([]byte(attrs), &e.Attrs); err != nil {
				return nil, fmt.Errorf("runtimedb: bad event attrs: %w", err)
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
