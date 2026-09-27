package runtimedb

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// HouseholdEvent is one row of the household event stream (ADR-W024 A2):
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
	if err := db.gate(GateIngestion); err != nil {
		return 0, false, err
	}
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

// HouseholdEventFilter narrows an event listing to what a caller is
// actually going to keep. Every field is optional.
//
// IT EXISTS BECAUSE A LIMIT AND A FILTER DO NOT COMMUTE. A caller that
// reads the newest N rows and then keeps the ones it wanted has not
// asked for its own rows at all: it has asked for everybody's and
// discarded most of them, and in a household where somebody else is
// busy it discards all of them. The catalog page did exactly that and
// showed a member no requests while their requests were still waiting
// on the owner.
type HouseholdEventFilter struct {
	// Type is the event type, e.g. catalog.requested.
	Type string
	// Subject is the person the event is about, matched the way the
	// rest of the household matches names: without regard to case.
	Subject string
	// Since bounds the window at the old end, so a caller wanting the
	// newest events WITHIN a period gets them from the newest end.
	//
	// It exists because HouseholdEventsSince answers a different
	// question — the OLDEST rows at or after a time, which is what a
	// "has this happened yet today" check wants — and a card showing
	// what the house did today reached for it and got the first two
	// hundred events of the morning. On a busy day the evening was
	// never in the window, and no amount of sorting afterwards could
	// put it there.
	Since time.Time
}

// HouseholdEventsBefore returns up to limit household events with a
// sequence number BELOW before, newest first — the window a paged
// history reads backwards through. A before of zero starts at the
// newest event, which is what an unpositioned first page wants.
//
// By sequence rather than by timestamp, and newest-first rather than
// oldest-first, which is the pair of choices that makes paging correct:
// two events can share a second, so a timestamp cursor either skips one
// or repeats it forever, and a reader walking backwards needs the rows
// adjacent to where it stopped rather than the oldest in the range.
func (db *DB) HouseholdEventsBefore(before int64, limit int, f HouseholdEventFilter) ([]HouseholdEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	// A zero cursor means "from the newest", expressed as an upper bound
	// nothing can exceed rather than as a branch with a second query to
	// keep in step with this one.
	if before <= 0 {
		before = math.MaxInt64
	}
	clauses, args := []string{"seq < ?"}, []any{before}
	if f.Type != "" {
		clauses = append(clauses, "type = ?")
		args = append(args, f.Type)
	}
	if !f.Since.IsZero() {
		// occurred_at stores RFC3339Nano UTC, so the comparison is
		// lexicographic — but NOT against an RFC3339Nano bound, and the
		// difference is a real hole rather than a nicety. That format
		// drops trailing zeros, so a moment inside a second is SHORTER
		// than the whole second: "00:00:00.001Z" against "00:00:00Z"
		// differs first at '.' against 'Z', and '.' sorts lower. Every
		// sub-second event in the bound's own second would compare as
		// earlier than the bound and be dropped — a hole at exactly the
		// boundary the caller named, which for a card showing "today"
		// is the first second of the day.
		//
		// So the bound is the second-floor with no zone suffix, which
		// is a PREFIX of every stamp in that second and therefore sorts
		// at or below all of them, and below every later one. The whole
		// boundary second is included, which is what "since" means.
		clauses = append(clauses, "occurred_at >= ?")
		args = append(args, f.Since.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05"))
	}
	if f.Subject != "" {
		// NOCASE because every reader of this column compares names
		// case-insensitively — the household has one Sandy however she
		// signed in (ADR-W032 A4) — and a predicate that did not would
		// hide a member's own rows from them.
		clauses = append(clauses, "subject = ? COLLATE NOCASE")
		args = append(args, f.Subject)
	}
	args = append(args, limit)
	rows, err := db.sql.Query(
		`SELECT seq, event_id, occurred_at, recorded_at, subject, source, type, entity, payload, correlation_id, schema_version
		 FROM household_events WHERE `+strings.Join(clauses, " AND ")+` ORDER BY seq DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing household events: %w", err)
	}
	defer rows.Close()
	return scanHouseholdEvents(rows)
}

// HouseholdEventsSince returns up to limit household events that occurred
// at or after since, oldest first — the window shape a handler's "was
// breakfast logged this morning?" check needs (ADR-W024 A6). occurred_at
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
// (ADR-W024 A10): the old credential must stop working the moment the
// new one exists, not linger until someone remembers it.
func (db *DB) RevokeTokensFor(principal string) (int64, error) {
	res, err := db.sql.Exec(
		`UPDATE tokens SET revoked_at = ? WHERE principal = ? AND revoked_at = ''`, now(), principal)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: revoking tokens for %s: %w", principal, err)
	}
	return res.RowsAffected()
}

// RevokeTokensForExcept revokes every live token held by principal except
// the one with keepHash.
func (db *DB) RevokeTokensForExcept(principal, keepHash string) (int64, error) {
	res, err := db.sql.Exec(
		`UPDATE tokens SET revoked_at = ? WHERE principal = ? AND revoked_at = '' AND hash <> ?`, now(), principal, keepHash)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: revoking tokens for %s: %w", principal, err)
	}
	return res.RowsAffected()
}

// HouseholdQuery filters the household stream. Zero fields match
// everything; From/To bound occurred_at (inclusive). Newest reverses the
// order — a "last three breakfasts" lookup wants the tail, not the head.
type HouseholdQuery struct {
	Type    string
	Subject string
	// SubjectSet distinguishes "any subject" (false) from "subject is
	// exactly this, empty included" (true).
	SubjectSet bool
	Entity     string
	Source     string
	From, To   time.Time
	Limit      int
	Newest     bool
}

// QueryHouseholdEvents returns the events matching q, in seq order
// (reversed when q.Newest). The automation engine's conditions are
// built on this one query, so a condition cannot look at anything a
// replay could not also look at.
func (db *DB) QueryHouseholdEvents(q HouseholdQuery) ([]HouseholdEvent, error) {
	where := []string{"1 = 1"}
	var args []any
	if q.Type != "" {
		where = append(where, "type = ?")
		args = append(args, q.Type)
	}
	if q.SubjectSet {
		where = append(where, "subject = ?")
		args = append(args, q.Subject)
	}
	if q.Entity != "" {
		where = append(where, "entity = ?")
		args = append(args, q.Entity)
	}
	if q.Source != "" {
		where = append(where, "source = ?")
		args = append(args, q.Source)
	}
	if !q.From.IsZero() {
		where = append(where, "occurred_at >= ?")
		args = append(args, q.From.UTC().Format(time.RFC3339Nano))
	}
	if !q.To.IsZero() {
		where = append(where, "occurred_at <= ?")
		args = append(args, q.To.UTC().Format(time.RFC3339Nano))
	}
	order := "ASC"
	if q.Newest {
		order = "DESC"
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	args = append(args, limit)
	rows, err := db.sql.Query(
		`SELECT seq, event_id, occurred_at, recorded_at, subject, source, type, entity, payload, correlation_id, schema_version
		 FROM household_events WHERE `+strings.Join(where, " AND ")+` ORDER BY seq `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: querying household events: %w", err)
	}
	defer rows.Close()
	return scanHouseholdEvents(rows)
}

// AutomationCursor returns the last household seq the automation engine
// evaluated; 0 before the first tick.
func (db *DB) AutomationCursor() (int64, error) {
	var seq int64
	err := db.sql.QueryRow(`SELECT seq FROM automation_cursor WHERE id = 1`).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("runtimedb: reading automation cursor: %w", err)
	}
	return seq, nil
}

// SetAutomationCursor records the engine's position.
func (db *DB) SetAutomationCursor(seq int64) error {
	_, err := db.sql.Exec(
		`INSERT INTO automation_cursor (id, seq) VALUES (1, ?)
		 ON CONFLICT (id) DO UPDATE SET seq = excluded.seq`, seq)
	if err != nil {
		return fmt.Errorf("runtimedb: setting automation cursor: %w", err)
	}
	return nil
}
