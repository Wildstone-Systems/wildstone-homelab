package runtimedb

// Meter readings (ADR-W035 A3): observations of the numbers a household
// keeps, as distinct from the Meter resources that declare them.
//
// Two rules this file exists to hold, both from the ADR:
//
//   - A reading is evidence, not authority. It is an ordinary
//     authenticated write rather than a Plan, because requiring a Plan to
//     write down a mileage means nobody writes down a mileage (A16).
//   - What kind of caller recorded it is stored and never read back when
//     deciding anything. A4 promises that an installed capability extends
//     the tracker rather than enabling it, and the sharp form of that
//     promise is that a number typed into the Console and one published
//     by Home Assistant are indistinguishable downstream.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// What kind of caller recorded a reading. Derived by the intake from the
// authenticated principal's role, never taken from the request body — the
// same rule household events follow for `source`, and for the same
// reason: a publisher that could name itself a person could launder an
// automatic number as somebody's word.
//
// Only two, because only two are provable. The Console, the CLI and
// household chat all present a person's credential, and which of them a
// number was typed into is not something the control plane can know
// without being told. `recorded_by` already carries who; this carries
// whether a human was involved at all.
const (
	ReadingByPerson    = "person"
	ReadingByPublisher = "publisher"
)

var readingSources = map[string]bool{ReadingByPerson: true, ReadingByPublisher: true}

// MeterReading is one observation of a meter.
type MeterReading struct {
	Seq   int64   `json:"seq"`
	Meter string  `json:"meter"`
	Value float64 `json:"value"`
	// At is when the meter was read. RecordedAt is when Keystone heard
	// about it. Staleness and replay are measured against At, so a
	// number written down this evening for a reading taken this morning
	// is treated as this morning's — which is what it is.
	At         time.Time `json:"at"`
	RecordedAt time.Time `json:"recordedAt"`
	RecordedBy string    `json:"recordedBy"`
	Source     string    `json:"source"`
	// Rebase marks a coordinate reset: the meter was replaced but the
	// obligation was not. Prior is what the old one read beforehand.
	Rebase bool    `json:"rebase,omitempty"`
	Prior  float64 `json:"prior,omitempty"`
	Reason string  `json:"reason,omitempty"`
	// ExternalID lets a publisher retry a delivery without recording the
	// number twice. Empty for a person, who is not retrying anything and
	// may legitimately write the same value down again.
	ExternalID string `json:"externalId,omitempty"`
}

// AppendMeterReading records one reading and returns its sequence number.
//
// A retried publisher delivery — same meter, same principal, same
// external id — is not an error: the existing row's seq comes back with
// duplicate=true and nothing is written. A person's reading carries no
// external id and is therefore never treated as a duplicate, because two
// identical numbers an hour apart are two real observations of a truck
// that did not move.
func (db *DB) AppendMeterReading(r MeterReading) (seq int64, duplicate bool, err error) {
	if err := db.gate(GateIngestion); err != nil {
		return 0, false, err
	}
	switch {
	case r.Meter == "":
		return 0, false, fmt.Errorf("runtimedb: meter reading needs the meter it is of")
	case r.RecordedBy == "":
		return 0, false, fmt.Errorf("runtimedb: meter reading needs who recorded it")
	case r.At.IsZero():
		return 0, false, fmt.Errorf("runtimedb: meter reading needs when it was read")
	case !readingSources[r.Source]:
		return 0, false, fmt.Errorf("runtimedb: meter reading source must be person|publisher, got %q", r.Source)
	}

	if r.ExternalID != "" {
		var existing int64
		err := db.sql.QueryRow(
			`SELECT seq FROM meter_readings WHERE meter = ? AND recorded_by = ? AND external_id = ?`,
			r.Meter, r.RecordedBy, r.ExternalID).Scan(&existing)
		if err == nil {
			return existing, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, fmt.Errorf("runtimedb: checking for a retried reading: %w", err)
		}
	}

	res, err := db.sql.Exec(
		`INSERT INTO meter_readings (meter, value, at, recorded_at, recorded_by, source, rebase, prior, reason, external_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Meter, r.Value, r.At.UTC().Format(time.RFC3339Nano), now(), r.RecordedBy,
		r.Source, boolToInt(r.Rebase), r.Prior, r.Reason, r.ExternalID)
	if err != nil {
		if isUniqueViolation(err) {
			// Lost a race with an identical retry; report the winner.
			var existing int64
			if e := db.sql.QueryRow(
				`SELECT seq FROM meter_readings WHERE meter = ? AND recorded_by = ? AND external_id = ?`,
				r.Meter, r.RecordedBy, r.ExternalID).Scan(&existing); e == nil {
				return existing, true, nil
			}
		}
		return 0, false, fmt.Errorf("runtimedb: recording meter reading: %w", err)
	}
	seq, err = res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	return seq, false, nil
}

const meterReadingColumns = `SELECT seq, meter, value, at, recorded_at, recorded_by, source,
	 rebase, prior, reason, external_id FROM meter_readings`

// ListMeterReadings returns a meter's readings in the order they were
// taken, oldest first, at most limit of them (0 means all).
//
// Oldest first because that is the order every consumer wants: the
// due-ness function walks the series looking for coordinate resets and
// regressions, and both questions are about what came before what. A
// screen showing the newest first can reverse a short slice; a function
// reasoning about order cannot un-reverse a long one cheaply.
func (db *DB) ListMeterReadings(meter string, limit int) ([]MeterReading, error) {
	query := meterReadingColumns + ` WHERE meter = ? ORDER BY at, seq`
	args := []any{meter}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing meter readings: %w", err)
	}
	defer rows.Close()
	out := []MeterReading{}
	for rows.Next() {
		r, err := scanMeterReading(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestMeterReading returns the newest reading of a meter, by when it
// was taken. ErrNotFound when the meter has never been read.
func (db *DB) LatestMeterReading(meter string) (MeterReading, error) {
	return scanMeterReading(db.sql.QueryRow(
		meterReadingColumns+` WHERE meter = ? ORDER BY at DESC, seq DESC LIMIT 1`, meter))
}

// MeterNames lists every meter that has at least one reading.
//
// Readings outlive the Meter resource that declared them: a deleted
// meter's history is not destroyed with its declaration, so this can
// name a meter the Statefile no longer has. That is deliberate — the
// numbers were true when they were written — and a caller wanting only
// declared meters should intersect with desired state rather than expect
// this to have done it.
func (db *DB) MeterNames() ([]string, error) {
	rows, err := db.sql.Query(`SELECT DISTINCT meter FROM meter_readings ORDER BY meter`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing meters: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("runtimedb: scanning meter name: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func scanMeterReading(row rowScanner) (MeterReading, error) {
	var r MeterReading
	var at, recorded string
	var rebase int
	err := row.Scan(&r.Seq, &r.Meter, &r.Value, &at, &recorded, &r.RecordedBy,
		&r.Source, &rebase, &r.Prior, &r.Reason, &r.ExternalID)
	if errors.Is(err, sql.ErrNoRows) {
		return MeterReading{}, ErrNotFound
	}
	if err != nil {
		return MeterReading{}, fmt.Errorf("runtimedb: scanning meter reading: %w", err)
	}
	r.Rebase = rebase == 1
	for _, f := range []struct {
		text string
		into *time.Time
	}{{at, &r.At}, {recorded, &r.RecordedAt}} {
		if f.text == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, f.text)
		if err != nil {
			return MeterReading{}, fmt.Errorf("runtimedb: bad meter reading timestamp %q: %w", f.text, err)
		}
		*f.into = parsed
	}
	return r, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
