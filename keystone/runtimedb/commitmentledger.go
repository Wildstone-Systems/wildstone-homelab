package runtimedb

// The commitment ledger (ADR-W034 A6, A8): what the household has done,
// and what it has arranged to do.
//
// Only completions and bookings are truth. Where a cycle stands is a
// pure function of those plus the meter readings (A17), so it is
// recomputed rather than stored — and CommitmentState below is a cache
// of what was last ANNOUNCED, not of what is the case. The distinction
// matters on restore: a stale verdict would be believed, while a stale
// announcement only causes something to be said twice.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CommitmentCompletion is one closed cycle.
type CommitmentCompletion struct {
	Seq        int64     `json:"seq"`
	Commitment string    `json:"commitment"`
	At         time.Time `json:"at"`
	RecordedAt time.Time `json:"recordedAt"`
	RecordedBy string    `json:"recordedBy"`
	// Waived records a cycle deliberately skipped, with a reason.
	Waived bool   `json:"waived,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Positions is meter name to the reading at completion. It is what
	// the next cycle is measured from, which is why A8 calls it evidence
	// and why a completion without it leaves the next cycle unplaceable.
	Positions map[string]float64 `json:"positions,omitempty"`
	// ExternalID is the household event that closed this cycle, for a
	// commitment that declares `satisfiedBy`. Empty for an answer a
	// person gave, and that emptiness is what keeps two people ticking
	// off the bins from colliding while a replayed event cannot close
	// the same cycle twice.
	ExternalID string `json:"externalId,omitempty"`
	// Approximate names the meters whose position here was taken from a
	// reading too old to rely on rather than given by whoever closed the
	// cycle. Empty means every number in Positions was current.
	//
	// It is recorded rather than inferred because nothing later can tell:
	// a mileage is a mileage, and a household reading its own maintenance
	// history deserves to know which ones were guessed.
	Approximate []string `json:"approximate,omitempty"`
}

// RecordCommitmentCompletion closes a cycle, with its evidence, in one
// statement — so a crash cannot leave a cycle closed and the readings it
// was closed at missing — and reports whether it was already closed.
//
// `duplicate` is the same shape AppendMeterReading uses, and it exists
// for the same reason: the caller has to be able to tell "recorded" from
// "the household fact this came from had already closed this cycle".
// Returning a bare zero seq for both invites the caller to treat a
// no-op as a write, which is how a harvest loop stops looking at the
// event that actually matters.
func (db *DB) RecordCommitmentCompletion(c CommitmentCompletion) (seq int64, duplicate bool, err error) {
	if err := db.gate(GateIngestion); err != nil {
		return 0, false, err
	}
	switch {
	case c.Commitment == "":
		return 0, false, fmt.Errorf("runtimedb: a completion needs the commitment it closes")
	case c.RecordedBy == "":
		return 0, false, fmt.Errorf("runtimedb: a completion needs who recorded it")
	case c.At.IsZero():
		return 0, false, fmt.Errorf("runtimedb: a completion needs when the work was done")
	}
	positions := "{}"
	if len(c.Positions) > 0 {
		encoded, err := json.Marshal(c.Positions)
		if err != nil {
			return 0, false, fmt.Errorf("runtimedb: encoding completion positions: %w", err)
		}
		positions = string(encoded)
	}
	res, err := db.sql.Exec(
		`INSERT INTO commitment_completions (commitment, at, recorded_at, recorded_by, waived, reason, positions, external_id, approximate)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (commitment, external_id) WHERE external_id <> '' DO NOTHING`,
		c.Commitment, c.At.UTC().Format(time.RFC3339Nano), now(), c.RecordedBy,
		boolToInt(c.Waived), c.Reason, positions, c.ExternalID, strings.Join(c.Approximate, ","))
	if err != nil {
		return 0, false, fmt.Errorf("runtimedb: recording completion: %w", err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		// The same household fact, seen again by a later pass. Not an
		// error and not a second completion: the cycle it closed is
		// already closed.
		return 0, true, nil
	}
	seq, err = res.LastInsertId()
	return seq, false, err
}

const completionColumns = `SELECT seq, commitment, at, recorded_at, recorded_by, waived, reason, positions, external_id, approximate
	 FROM commitment_completions`

// ListCommitmentCompletions returns a commitment's closed cycles, oldest
// first — the order the due-ness function counts them in.
func (db *DB) ListCommitmentCompletions(commitment string) ([]CommitmentCompletion, error) {
	rows, err := db.sql.Query(completionColumns+` WHERE commitment = ? ORDER BY at, seq`, commitment)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing completions: %w", err)
	}
	defer rows.Close()
	out := []CommitmentCompletion{}
	for rows.Next() {
		var c CommitmentCompletion
		var at, recorded, positions, approximate string
		var waived int
		if err := rows.Scan(&c.Seq, &c.Commitment, &at, &recorded, &c.RecordedBy, &waived, &c.Reason, &positions, &c.ExternalID, &approximate); err != nil {
			return nil, fmt.Errorf("runtimedb: scanning completion: %w", err)
		}
		c.Waived = waived == 1
		if approximate != "" {
			c.Approximate = strings.Split(approximate, ",")
		}
		if err := json.Unmarshal([]byte(positions), &c.Positions); err != nil {
			return nil, fmt.Errorf("runtimedb: bad completion positions %q: %w", positions, err)
		}
		for _, f := range []struct {
			text string
			into *time.Time
		}{{at, &c.At}, {recorded, &c.RecordedAt}} {
			parsed, perr := time.Parse(time.RFC3339Nano, f.text)
			if perr != nil {
				return nil, fmt.Errorf("runtimedb: bad completion timestamp %q: %w", f.text, perr)
			}
			*f.into = parsed
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CommitmentBooking is an appointment for the open cycle.
type CommitmentBooking struct {
	Seq         int64     `json:"seq"`
	Commitment  string    `json:"commitment"`
	At          time.Time `json:"at"`
	BookedAt    time.Time `json:"bookedAt"`
	BookedBy    string    `json:"bookedBy"`
	Note        string    `json:"note,omitempty"`
	CancelledAt time.Time `json:"cancelledAt,omitzero"`
	CancelledBy string    `json:"cancelledBy,omitempty"`
}

// Live reports whether this booking still stands.
func (b CommitmentBooking) Live() bool { return b.CancelledAt.IsZero() }

// RecordCommitmentBooking arranges an appointment.
//
// BookedAt defaults to now, which is what every caller in the product
// wants: the API lets it default, so a household cannot claim to have
// arranged something last week. It is settable because when a booking
// was MADE is load-bearing rather than decorative — the due-ness
// function refuses to see an appointment nobody had arranged yet, so a
// replay of last August must not be shown a booking made in September —
// and a store whose only clock is the wall clock cannot be tested
// against that rule at all, nor carry a household's existing
// appointments in from wherever it kept them before.
func (db *DB) RecordCommitmentBooking(b CommitmentBooking) (int64, error) {
	if err := db.gate(GateIngestion); err != nil {
		return 0, err
	}
	switch {
	case b.Commitment == "":
		return 0, fmt.Errorf("runtimedb: a booking needs the commitment it is for")
	case b.BookedBy == "":
		return 0, fmt.Errorf("runtimedb: a booking needs who made it")
	case b.At.IsZero():
		return 0, fmt.Errorf("runtimedb: a booking needs when the appointment is")
	}
	bookedAt := now()
	if !b.BookedAt.IsZero() {
		bookedAt = b.BookedAt.UTC().Format(time.RFC3339Nano)
	}
	res, err := db.sql.Exec(
		`INSERT INTO commitment_bookings (commitment, at, booked_at, booked_by, note)
		 VALUES (?, ?, ?, ?, ?)`,
		b.Commitment, b.At.UTC().Format(time.RFC3339Nano), bookedAt, b.BookedBy, b.Note)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: recording booking: %w", err)
	}
	return res.LastInsertId()
}

// CancelCommitmentBooking records that an appointment was cancelled.
// Recorded rather than deleted: "we had one and it fell through" and "we
// never made one" are different stories about the same empty calendar.
func (db *DB) CancelCommitmentBooking(seq int64, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: cancelling a booking needs who did it")
	}
	res, err := db.sql.Exec(
		`UPDATE commitment_bookings SET cancelled_at = ?, cancelled_by = ?
		 WHERE seq = ? AND cancelled_at = ''`, now(), by, seq)
	if err != nil {
		return fmt.Errorf("runtimedb: cancelling booking: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// LatestCommitmentBooking returns the most recently MADE booking for a
// commitment, cancelled or not.
//
// Most recently made rather than soonest: the question the engine asks is
// "what did the household most recently arrange", and a cancelled
// appointment is an answer to it. Filtering by Live() is the caller's
// choice because the two states mean different things to a person.
func (db *DB) LatestCommitmentBooking(commitment string) (CommitmentBooking, error) {
	row := db.sql.QueryRow(
		`SELECT seq, commitment, at, booked_at, booked_by, note, cancelled_at, cancelled_by
		 FROM commitment_bookings WHERE commitment = ? ORDER BY booked_at DESC, seq DESC LIMIT 1`, commitment)
	var b CommitmentBooking
	var at, bookedAt, cancelledAt string
	err := row.Scan(&b.Seq, &b.Commitment, &at, &bookedAt, &b.BookedBy, &b.Note, &cancelledAt, &b.CancelledBy)
	if errors.Is(err, sql.ErrNoRows) {
		return CommitmentBooking{}, ErrNotFound
	}
	if err != nil {
		return CommitmentBooking{}, fmt.Errorf("runtimedb: scanning booking: %w", err)
	}
	for _, f := range []struct {
		text string
		into *time.Time
	}{{at, &b.At}, {bookedAt, &b.BookedAt}, {cancelledAt, &b.CancelledAt}} {
		if f.text == "" {
			continue
		}
		parsed, perr := time.Parse(time.RFC3339Nano, f.text)
		if perr != nil {
			return CommitmentBooking{}, fmt.Errorf("runtimedb: bad booking timestamp %q: %w", f.text, perr)
		}
		*f.into = parsed
	}
	return b, nil
}

// CommitmentState is what the household was last told about a
// commitment. A cache, not a verdict — see the file comment.
type CommitmentState struct {
	Commitment string    `json:"commitment"`
	State      string    `json:"state"`
	Cycle      int       `json:"cycle"`
	Trigger    string    `json:"trigger,omitempty"`
	ChangedAt  time.Time `json:"changedAt"`
}

// AnnouncedCommitmentState returns what was last announced, or
// ErrNotFound when nothing has been.
func (db *DB) AnnouncedCommitmentState(commitment string) (CommitmentState, error) {
	var s CommitmentState
	var changed string
	err := db.sql.QueryRow(
		`SELECT commitment, state, cycle, trigger, changed_at FROM commitment_state WHERE commitment = ?`,
		commitment).Scan(&s.Commitment, &s.State, &s.Cycle, &s.Trigger, &changed)
	if errors.Is(err, sql.ErrNoRows) {
		return CommitmentState{}, ErrNotFound
	}
	if err != nil {
		return CommitmentState{}, fmt.Errorf("runtimedb: reading announced state: %w", err)
	}
	if changed != "" {
		parsed, perr := time.Parse(time.RFC3339Nano, changed)
		if perr != nil {
			return CommitmentState{}, fmt.Errorf("runtimedb: bad state timestamp %q: %w", changed, perr)
		}
		s.ChangedAt = parsed
	}
	return s, nil
}

// AnnounceCommitmentState records what the household has now been told.
func (db *DB) AnnounceCommitmentState(s CommitmentState) error {
	if s.Commitment == "" || s.State == "" {
		return fmt.Errorf("runtimedb: an announced state needs a commitment and a state")
	}
	_, err := db.sql.Exec(
		`INSERT INTO commitment_state (commitment, state, cycle, trigger, changed_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(commitment) DO UPDATE SET
		   state = excluded.state, cycle = excluded.cycle,
		   trigger = excluded.trigger, changed_at = excluded.changed_at`,
		s.Commitment, s.State, s.Cycle, s.Trigger, now())
	if err != nil {
		return fmt.Errorf("runtimedb: recording announced state: %w", err)
	}
	return nil
}

// ForgetCommitmentState drops the announcement cache for a commitment,
// which is what a deleted commitment leaves behind. Its completions are
// NOT dropped: the work was done, and deleting the declaration does not
// undo it.
func (db *DB) ForgetCommitmentState(commitment string) error {
	_, err := db.sql.Exec(`DELETE FROM commitment_state WHERE commitment = ?`, commitment)
	if err != nil {
		return fmt.Errorf("runtimedb: forgetting announced state: %w", err)
	}
	return nil
}
