package runtimedb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// HouseholdEvent is one row of the household event stream (ADR-W022 A2):
// a fact about the household — an arrival, a meal, a calendar change —
// as distinct from the operational stream in Event. Source is stamped
// from the publishing principal, RecordedAt by AppendHouseholdEvent;
// callers never supply either.
type HouseholdEvent struct {
	Seq           int64           `json:"seq"`
	EventID       string          `json:"eventId"`
	OccurredAt    time.Time       `json:"occurredAt"`
	RecordedAt    time.Time       `json:"recordedAt"`
	Subject       string          `json:"subject,omitempty"`
	Source        string          `json:"source"`
	Type          string          `json:"type"`
	Entity        string          `json:"entity,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	CorrelationID string          `json:"correlationId,omitempty"`
	SchemaVersion int             `json:"schemaVersion"`
}

// AppendHouseholdEvent appends one household event and returns its
// sequence number. A retried delivery — same source, same event_id — is
// not an error: the existing row's seq comes back with duplicate=true and
// nothing is written, which is what lets a publisher re-send after a lost
// response without double-counting. Semantic validation of the envelope
// is keystone/household's job; this only insists on the columns the
// schema cannot default.
func (db *DB) AppendHouseholdEvent(e HouseholdEvent) (seq int64, duplicate bool, err error) {
	switch {
	case e.EventID == "":
		return 0, false, fmt.Errorf("runtimedb: household event id required")
	case e.Source == "":
		return 0, false, fmt.Errorf("runtimedb: household event source required")
	case e.Type == "":
		return 0, false, fmt.Errorf("runtimedb: household event type required")
	case e.OccurredAt.IsZero():
		return 0, false, fmt.Errorf("runtimedb: household event occurred_at required")
	}
	payload := "{}"
	if len(e.Payload) > 0 {
		if !json.Valid(e.Payload) {
			return 0, false, fmt.Errorf("runtimedb: household event payload is not valid JSON")
		}
		payload = string(e.Payload)
	}
	version := e.SchemaVersion
	if version <= 0 {
		version = 1
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return 0, false, fmt.Errorf("runtimedb: begin household append: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	res, err := tx.Exec(
		`INSERT INTO household_events
		   (event_id, occurred_at, recorded_at, subject, source, type, entity, payload, correlation_id, schema_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (source, event_id) DO NOTHING`,
		e.EventID, e.OccurredAt.UTC().Format(time.RFC3339Nano), now(),
		e.Subject, e.Source, e.Type, e.Entity, payload, e.CorrelationID, version)
	if err != nil {
		return 0, false, fmt.Errorf("runtimedb: appending household event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if n == 0 {
		duplicate = true
		err = tx.QueryRow(
			`SELECT seq FROM household_events WHERE source = ? AND event_id = ?`,
			e.Source, e.EventID).Scan(&seq)
		if err != nil {
			return 0, false, fmt.Errorf("runtimedb: resolving duplicate household event: %w", err)
		}
	} else if seq, err = res.LastInsertId(); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("runtimedb: commit household append: %w", err)
	}
	return seq, duplicate, nil
}

// HouseholdEventsAfter returns up to limit household events with
// seq > after, oldest first — the same cursor shape EventsAfter serves,
// so a consumer that already follows the operational stream follows this
// one the same way.
func (db *DB) HouseholdEventsAfter(after int64, limit int) ([]HouseholdEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.sql.Query(
		`SELECT seq, event_id, occurred_at, recorded_at, subject, source, type, entity, payload, correlation_id, schema_version
		 FROM household_events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing household events: %w", err)
	}
	defer rows.Close()
	return scanHouseholdEvents(rows)
}

// HouseholdEventsSince returns up to limit household events that occurred
// at or after since, oldest first — the window shape a handler's "was
// breakfast logged this morning?" check needs (ADR-W022 A6). occurred_at
// stores RFC3339Nano UTC, so the comparison is lexicographic.
func (db *DB) HouseholdEventsSince(since time.Time, limit int) ([]HouseholdEvent, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := db.sql.Query(
		`SELECT seq, event_id, occurred_at, recorded_at, subject, source, type, entity, payload, correlation_id, schema_version
		 FROM household_events WHERE occurred_at >= ? ORDER BY seq LIMIT ?`,
		since.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing household events since %s: %w", since, err)
	}
	defer rows.Close()
	return scanHouseholdEvents(rows)
}

func scanHouseholdEvents(rows *sql.Rows) ([]HouseholdEvent, error) {
	var out []HouseholdEvent
	for rows.Next() {
		var e HouseholdEvent
		var occurred, recorded, payload string
		if err := rows.Scan(&e.Seq, &e.EventID, &occurred, &recorded, &e.Subject, &e.Source, &e.Type,
			&e.Entity, &payload, &e.CorrelationID, &e.SchemaVersion); err != nil {
			return nil, fmt.Errorf("runtimedb: scanning household event: %w", err)
		}
		var err error
		if e.OccurredAt, err = time.Parse(time.RFC3339Nano, occurred); err != nil {
			return nil, fmt.Errorf("runtimedb: bad household occurred_at %q: %w", occurred, err)
		}
		if e.RecordedAt, err = time.Parse(time.RFC3339Nano, recorded); err != nil {
			return nil, fmt.Errorf("runtimedb: bad household recorded_at %q: %w", recorded, err)
		}
		if payload != "" && payload != "{}" {
			e.Payload = json.RawMessage(payload)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// RevokeTokensFor revokes every live token held by principal and returns
// how many it revoked. Re-minting a publisher's token is a rotation
// (ADR-W022 A10): the old credential must stop working the moment the
// new one exists, not linger until someone remembers it.
func (db *DB) RevokeTokensFor(principal string) (int64, error) {
	res, err := db.sql.Exec(
		`UPDATE tokens SET revoked_at = ? WHERE principal = ? AND revoked_at = ''`, now(), principal)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: revoking tokens for %s: %w", principal, err)
	}
	return res.RowsAffected()
}
