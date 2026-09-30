package runtimedb

// Private mail event candidates (migration 0100, epic #326 S05, #458;
// ADR-W030 A5.6).
//
// A mailbox monitor reads one Person's new mail and may find a possible
// calendar event in it. This file is the only place that finding is
// kept, and it keeps as little as the review needs:
//
//   - Processing state per (connection, keyed fingerprint), so the
//     adapter's at-least-once delivery — a replayed batch, a UIDVALIDITY
//     reset — never produces a second candidate.
//   - Versioned candidates: minimal event fields, confidence, explicit
//     missing/ambiguous fields, the opaque provider reference and the
//     target calendar. A review is bound to (id, revision).
//   - A short excerpt, sealed under a key that is never in a backup, and
//     only while the candidate is unresolved.
//
// No column can hold a message body. Header values that correlate
// messages (Message-ID, sender, subject, invitation UID) arrive as
// connection-scoped digests computed by the caller.
//
// Every Person-facing read and write takes the acting handle and resolves
// it to an ACTIVE member whose canonical Person owns the candidate.
// Nothing substitutes: an Owner is a different Person, an Agent is not a
// member, and a miss is indistinguishable from "no such candidate".

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Candidate lifecycle states.
const (
	// MailCandidateProposed: complete enough to offer Add/Edit/Dismiss.
	MailCandidateProposed = "proposed"
	// MailCandidateNeedsDetails: a time, date, end or timezone is missing
	// or ambiguous. Offered for Edit or Dismiss only; nobody checks
	// conflicts against an invented time.
	MailCandidateNeedsDetails = "needs_details"
	// MailCandidateAccepted: the Person approved this exact revision and
	// the calendar write is in flight (S06).
	MailCandidateAccepted = "accepted"
	// MailCandidateCreated: the event exists on the target calendar.
	MailCandidateCreated = "created"
	// MailCandidateDismissed: the Person said no. Only the fingerprint
	// and correlation keys survive, so the same mail is not asked twice.
	MailCandidateDismissed = "dismissed"
	// MailCandidateSuperseded: a correction replaced it before it was
	// resolved.
	MailCandidateSuperseded = "superseded"
	// MailCandidateExpired: its start passed, the unresolved window
	// closed, or the monitor's authority went away.
	MailCandidateExpired = "expired"
	// MailCandidateCancelled: the organizer cancelled it (iTIP CANCEL)
	// before it was resolved.
	MailCandidateCancelled = "cancelled"
)

// Candidate sources.
const (
	MailSourceICS   = "ics"
	MailSourceModel = "model"
)

// Message processing states.
const (
	MailMessageCandidate        = "candidate"
	MailMessageDuplicateEvent   = "duplicate_event"
	MailMessageCancelled        = "cancelled"
	MailMessageNoEvent          = "no_event"
	MailMessageSkipped          = "skipped"
	MailMessageExtractionFailed = "extraction_failed"
	MailMessageRetrying         = "retrying"
)

// Pending calendar changes on a candidate that already created (or was
// accepted to create) its event. S06 turns these into an exact proposal.
const (
	MailUpdateNone   = ""
	MailUpdateEvent  = "update"
	MailUpdateCancel = "cancel"
)

// The closed vocabulary of missing/ambiguous fields.
const (
	MailFieldTitle      = "title"
	MailFieldDate       = "date"
	MailFieldStart      = "start"
	MailFieldEnd        = "end"
	MailFieldTimezone   = "timezone"
	MailFieldLocation   = "location"
	MailFieldRecurrence = "recurrence"
)

var mailFieldNames = map[string]bool{
	MailFieldTitle: true, MailFieldDate: true, MailFieldStart: true, MailFieldEnd: true,
	MailFieldTimezone: true, MailFieldLocation: true, MailFieldRecurrence: true,
}

// Changes a record or a sweep made, as the candidate-ready hook sees them.
const (
	// MailChangeNew: a new candidate awaits the Person's review.
	MailChangeNew = "new"
	// MailChangeSuperseded: an unresolved candidate was replaced by a
	// correction (the correction arrives as its own MailChangeNew).
	MailChangeSuperseded = "superseded"
	// MailChangeUpdateNeeded: a candidate whose event already exists was
	// corrected or cancelled; its UpdatePending says which.
	MailChangeUpdateNeeded = "update_needed"
	// MailChangeCancelled: an unresolved candidate was cancelled.
	MailChangeCancelled = "cancelled"
)

// Bounds on what a candidate holds. The schema repeats them.
const (
	MaxMailTitle    = 200
	MaxMailLocation = 200
	MaxMailNotes    = 500
	MaxMailExcerpt  = 300
	maxMailRefLen   = 512
	maxMailReason   = 64
)

// MailMonitorActor is the author of revisions the monitor extracted.
const MailMonitorActor = "monitor"

var (
	// ErrMailCandidateNotFound is every refusal of a Person-scoped read or
	// write: no such candidate, or not the caller's. The two are the same
	// answer on purpose.
	ErrMailCandidateNotFound = errors.New("runtimedb: no such mail candidate")
	// ErrMailCandidateStale: the caller named a revision or state the
	// candidate has moved past. Nothing changed.
	ErrMailCandidateStale = errors.New("runtimedb: the mail candidate has changed; review its current revision")
	// ErrMailConnectionFenced: the connection's lease epoch, owner or
	// existence moved since the run read the mail. Nothing was recorded.
	ErrMailConnectionFenced = errors.New("runtimedb: the mail connection moved since this run read it")
)

// MailCandidateFields is one revision's event.
type MailCandidateFields struct {
	Title string `json:"title"`
	// Start and End are instants; zero when unknown. An all-day event's
	// Start is midnight of its first day in TZ and End is exclusive.
	Start time.Time `json:"start,omitzero"`
	End   time.Time `json:"end,omitzero"`
	// DurationMinutes is set when a duration is known without an end.
	DurationMinutes int  `json:"durationMinutes,omitempty"`
	AllDay          bool `json:"allDay"`
	// TZ is the IANA zone the times were resolved in: the invitation's
	// own, or the household's.
	TZ       string `json:"tz,omitempty"`
	Location string `json:"location,omitempty"`
	// Notes are concise and calendar-safe: sanitized, no links.
	Notes      string   `json:"notes,omitempty"`
	Confidence float64  `json:"confidence"`
	Missing    []string `json:"missing"`
	Ambiguous  []string `json:"ambiguous"`
}

// NeedsDetails reports whether anything is missing or ambiguous that
// the Person must supply before the event can be offered for Add.
// Location alone never holds a candidate back.
func (f MailCandidateFields) NeedsDetails() bool {
	for _, list := range [][]string{f.Missing, f.Ambiguous} {
		for _, name := range list {
			if name != MailFieldLocation {
				return true
			}
		}
	}
	return false
}

// SameEvent reports whether g describes the same event as f: title,
// times and all-day flag. A reminder of an event already on record is
// not a new candidate.
func (f MailCandidateFields) SameEvent(g MailCandidateFields) bool {
	return !f.Start.IsZero() && strings.EqualFold(strings.TrimSpace(f.Title), strings.TrimSpace(g.Title)) &&
		f.Start.Equal(g.Start) && f.End.Equal(g.End) && f.AllDay == g.AllDay &&
		f.DurationMinutes == g.DurationMinutes
}

func (f MailCandidateFields) validate() error {
	switch {
	case len(f.Title) > MaxMailTitle || !utf8.ValidString(f.Title):
		return fmt.Errorf("runtimedb: a candidate title is at most %d bytes of UTF-8", MaxMailTitle)
	case len(f.Location) > MaxMailLocation || !utf8.ValidString(f.Location):
		return fmt.Errorf("runtimedb: a candidate location is at most %d bytes of UTF-8", MaxMailLocation)
	case len(f.Notes) > MaxMailNotes || !utf8.ValidString(f.Notes):
		return fmt.Errorf("runtimedb: candidate notes are at most %d bytes of UTF-8", MaxMailNotes)
	case len(f.TZ) > 64:
		return fmt.Errorf("runtimedb: a candidate timezone name is at most 64 bytes")
	case f.TZ != "":
		if _, err := time.LoadLocation(f.TZ); err != nil {
			return fmt.Errorf("runtimedb: candidate timezone %q is not a known zone", f.TZ)
		}
	}
	if math.IsNaN(f.Confidence) || f.Confidence < 0 || f.Confidence > 1 {
		return fmt.Errorf("runtimedb: candidate confidence must be 0..1")
	}
	if f.DurationMinutes < 0 || f.DurationMinutes > 20160 {
		return fmt.Errorf("runtimedb: candidate duration must be 0..20160 minutes")
	}
	if !f.Start.IsZero() && !f.End.IsZero() && !f.End.After(f.Start) {
		return fmt.Errorf("runtimedb: a candidate must end after it starts")
	}
	for _, list := range [][]string{f.Missing, f.Ambiguous} {
		if len(list) > len(mailFieldNames) {
			return fmt.Errorf("runtimedb: too many missing/ambiguous fields")
		}
		for _, name := range list {
			if !mailFieldNames[name] {
				return fmt.Errorf("runtimedb: %q is not a candidate field", name)
			}
		}
	}
	return nil
}

// MailCandidate is a candidate at its current revision.
type MailCandidate struct {
	ID          string `json:"id"`
	Revision    int    `json:"revision"`
	PersonID    string `json:"personId"`
	Connection  string `json:"connection"`
	SourceRef   string `json:"sourceRef"`
	Fingerprint string `json:"fingerprint"`
	Source      string `json:"source"`
	// TargetCalendar is the calendar the Person authorized for this
	// monitor. It comes from the authorization, never from the mail.
	TargetCalendar string              `json:"targetCalendar"`
	State          string              `json:"state"`
	StateReason    string              `json:"stateReason,omitempty"`
	Fields         MailCandidateFields `json:"fields"`
	Supersedes     string              `json:"supersedes,omitempty"`
	SupersededBy   string              `json:"supersededBy,omitempty"`
	UpdatePending  string              `json:"updatePending,omitempty"`
	CreatedAt      time.Time           `json:"createdAt"`
	UpdatedAt      time.Time           `json:"updatedAt"`
	ExpiresAt      time.Time           `json:"expiresAt"`
	ResolvedAt     time.Time           `json:"resolvedAt,omitzero"`
	AnnouncedAt    time.Time           `json:"announcedAt,omitzero"`

	uidKey, messageKey, senderKey, subjectKey string
	icsSequence                               int
}

// Unresolved reports whether the candidate still awaits the Person.
func (c MailCandidate) Unresolved() bool { return mailUnresolved(c.State) }

func mailUnresolved(state string) bool {
	return state == MailCandidateProposed || state == MailCandidateNeedsDetails
}

// mailEventCommitted: an event exists or may exist for this candidate.
func mailEventCommitted(state string) bool {
	return state == MailCandidateAccepted || state == MailCandidateCreated
}

// MailCandidateDraft is one event (or cancellation) extracted from a
// message, before it is recorded.
type MailCandidateDraft struct {
	Fields MailCandidateFields
	Source string
	// UIDKey is the connection-scoped digest of an invitation's UID, and
	// ICSSequence its SEQUENCE; empty/zero for prose.
	UIDKey      string
	ICSSequence int
	// MessageKey, SenderKey and SubjectKey are connection-scoped digests
	// of the message's Message-ID, From address and normalized subject.
	// ThreadKeys digest its In-Reply-To and References.
	MessageKey string
	SenderKey  string
	SubjectKey string
	ThreadKeys []string
	// Correction: the message says it corrects an earlier one. It
	// supersedes a candidate only from the same sender, in the same
	// thread or under the same subject.
	Correction bool
	// Cancel: an iTIP CANCEL for UIDKey. It records no candidate.
	Cancel bool
	// Excerpt is the short evidence kept, sealed, while unresolved.
	Excerpt string
}

// MailMessageRecord is one processed message.
type MailMessageRecord struct {
	Connection string
	// Epoch is the connection lease epoch the run read the mail under.
	Epoch       int64
	PersonID    string
	Fingerprint string
	SourceRef   string
	Run         int64
	// State and Reason are the message's outcome when it produced no
	// draft; with drafts, recording decides.
	State  string
	Reason string
	Source string
	// TargetCalendar is the authorization's.
	TargetCalendar string
	Drafts         []MailCandidateDraft
	At             time.Time
	// Window bounds how long an unresolved candidate lives, and
	// ExcerptWindow its excerpt; zero means the defaults.
	Window        time.Duration
	ExcerptWindow time.Duration
}

// MailCandidateChange is one thing a record or a transition did.
type MailCandidateChange struct {
	Kind      string        `json:"kind"`
	Candidate MailCandidate `json:"candidate"`
}

// MailRecordResult says what recording a message did.
type MailRecordResult struct {
	// Duplicate: the fingerprint was already processed; nothing written.
	Duplicate bool
	State     string
	Reason    string
	Changes   []MailCandidateChange
}

// Default windows.
const (
	DefaultMailCandidateWindow = 14 * 24 * time.Hour
	DefaultMailExcerptWindow   = 7 * 24 * time.Hour
)

// mailTS is a fixed-width UTC timestamp, so the TEXT columns compare in
// time order.
func mailTS(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func parseMailTS(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// BoundMailText truncates s to max bytes on a rune boundary.
func BoundMailText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// ---------------------------------------------------------------------
// The excerpt key.

// MailExcerptVault seals candidate excerpts.
//
// Key custody: a dedicated 32-byte AES-256-GCM key at a path beside the
// runtime database (by convention <DataDir>/mail-excerpts.key, 0600),
// created on first use like household-secrets.key — but, unlike that
// key, deliberately NOT part of the recovery set or any backup. An
// excerpt is expendable evidence, not household data: a database copy
// in a backup carries only ciphertext nobody can open, and a restored
// or re-keyed database discards every excerpt at open rather than
// refusing to start.
type MailExcerptVault struct {
	aead cipher.AEAD
}

// OpenMailExcerptVault loads (or creates) the excerpt key and drops
// every excerpt it cannot open.
func (db *DB) OpenMailExcerptVault(keyPath string) (*MailExcerptVault, error) {
	key, err := os.ReadFile(keyPath)
	created := false
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
			return nil, err
		}
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		created = true
	case err != nil:
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("runtimedb: invalid mail excerpt key")
	}
	block, err := aes.NewCipher(key)
	clear(key)
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	v := &MailExcerptVault{aead: a}
	if created {
		// Whatever was sealed before this key existed is unreadable.
		if _, err := db.sql.Exec(`DELETE FROM mail_candidate_excerpts`); err != nil {
			return nil, fmt.Errorf("runtimedb: discarding excerpts sealed under another key: %w", err)
		}
		return v, nil
	}
	rows, err := db.sql.Query(`SELECT e.candidate, c.person_id, e.sealed FROM mail_candidate_excerpts e
		JOIN mail_candidates c ON c.id = e.candidate`)
	if err != nil {
		return nil, err
	}
	var bad []string
	for rows.Next() {
		var id, person string
		var sealed []byte
		if err := rows.Scan(&id, &person, &sealed); err != nil {
			rows.Close()
			return nil, err
		}
		plain, err := v.open(id, person, sealed)
		clear(plain)
		if err != nil {
			bad = append(bad, id)
		}
	}
	rows.Close()
	for _, id := range bad {
		if _, err := db.sql.Exec(`DELETE FROM mail_candidate_excerpts WHERE candidate = ?`, id); err != nil {
			return nil, err
		}
	}
	return v, nil
}

func mailExcerptAAD(id, person string) []byte {
	return []byte("mail-excerpt/v1/" + id + "/" + person)
}

func (v *MailExcerptVault) seal(id, person, text string) ([]byte, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return v.aead.Seal(nonce, nonce, []byte(text), mailExcerptAAD(id, person)), nil
}

func (v *MailExcerptVault) open(id, person string, sealed []byte) ([]byte, error) {
	n := v.aead.NonceSize()
	if len(sealed) <= n {
		return nil, errors.New("runtimedb: excerpt ciphertext is too short")
	}
	return v.aead.Open(nil, sealed[:n], sealed[n:], mailExcerptAAD(id, person))
}

// ---------------------------------------------------------------------
// Recording processed mail.

// MailMessageState returns what is on record for a fingerprint: its
// state and attempts, and ok=false when it was never seen.
func (db *DB) MailMessageState(connection, fingerprint string) (state string, attempts int, ok bool, err error) {
	err = db.sql.QueryRow(`SELECT m.state, m.attempts FROM mail_monitor_messages m
		JOIN integration_connections c ON c.seq = m.connection
		WHERE c.name = ? AND m.fingerprint = ?`, connection, fingerprint).Scan(&state, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("runtimedb: reading mail message state: %w", err)
	}
	return state, attempts, true, nil
}

// RecordMailMessage records one processed message and whatever
// candidates it produced, corrected or cancelled, in one transaction
// that first re-checks the connection is still at the epoch the run read
// it under and still owned by rec.PersonID. A fingerprint already on
// record (other than as retrying) is a duplicate and writes nothing.
func (db *DB) RecordMailMessage(v *MailExcerptVault, rec MailMessageRecord) (MailRecordResult, error) {
	if err := db.gate(GateIngestion); err != nil {
		return MailRecordResult{}, err
	}
	if rec.At.IsZero() {
		return MailRecordResult{}, fmt.Errorf("runtimedb: a mail record needs its time")
	}
	if !strings.HasPrefix(rec.Fingerprint, "mfp1_") || len(rec.Fingerprint) > 128 {
		return MailRecordResult{}, fmt.Errorf("runtimedb: %q is not a mail fingerprint", rec.Fingerprint)
	}
	if len(rec.SourceRef) > maxMailRefLen || len(rec.Reason) > maxMailReason {
		return MailRecordResult{}, fmt.Errorf("runtimedb: mail reference or reason too long")
	}
	for _, d := range rec.Drafts {
		if err := d.Fields.validate(); err != nil {
			return MailRecordResult{}, err
		}
		if d.Source != MailSourceICS && d.Source != MailSourceModel {
			return MailRecordResult{}, fmt.Errorf("runtimedb: candidate source must be ics|model")
		}
	}
	if rec.Window <= 0 {
		rec.Window = DefaultMailCandidateWindow
	}
	if rec.ExcerptWindow <= 0 {
		rec.ExcerptWindow = DefaultMailExcerptWindow
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return MailRecordResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var connSeq, epoch int64
	var ownership, owner string
	err = tx.QueryRow(`SELECT seq, lease_epoch, ownership, owner FROM integration_connections WHERE name = ?`,
		rec.Connection).Scan(&connSeq, &epoch, &ownership, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return MailRecordResult{}, fmt.Errorf("%w: %q no longer exists", ErrMailConnectionFenced, rec.Connection)
	}
	if err != nil {
		return MailRecordResult{}, err
	}
	if epoch != rec.Epoch {
		return MailRecordResult{}, fmt.Errorf("%w: epoch %d, read under %d", ErrMailConnectionFenced, epoch, rec.Epoch)
	}
	var ownerPerson string
	if err := tx.QueryRow(`SELECT person_id FROM butler_members WHERE member = ?`, owner).Scan(&ownerPerson); err != nil ||
		ownership != OwnershipPersonal || ownerPerson != rec.PersonID {
		return MailRecordResult{}, fmt.Errorf("%w: %q is not a Personal connection of this Person", ErrMailConnectionFenced, rec.Connection)
	}

	var prevState string
	var attempts int
	err = tx.QueryRow(`SELECT state, attempts FROM mail_monitor_messages WHERE connection = ? AND fingerprint = ?`,
		connSeq, rec.Fingerprint).Scan(&prevState, &attempts)
	seen := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return MailRecordResult{}, err
	}
	if seen && prevState != MailMessageRetrying {
		return MailRecordResult{Duplicate: true, State: prevState}, nil
	}
	at := mailTS(rec.At)
	res := MailRecordResult{State: rec.State, Reason: rec.Reason}
	if rec.State == MailMessageRetrying {
		if seen {
			_, err = tx.Exec(`UPDATE mail_monitor_messages SET attempts = attempts + 1, reason = ?, run = ?, processed_at = ?
				WHERE connection = ? AND fingerprint = ?`, rec.Reason, rec.Run, at, connSeq, rec.Fingerprint)
		} else {
			_, err = tx.Exec(`INSERT INTO mail_monitor_messages (connection, fingerprint, person_id, state, reason, source,
				attempts, run, first_seen_at, processed_at) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
				connSeq, rec.Fingerprint, rec.PersonID, rec.State, rec.Reason, rec.Source, rec.Run, at, at)
		}
		if err != nil {
			return MailRecordResult{}, fmt.Errorf("runtimedb: recording a mail retry: %w", err)
		}
		return res, tx.Commit()
	}

	if len(rec.Drafts) > 0 {
		res.State, res.Reason = MailMessageNoEvent, ""
		for _, d := range rec.Drafts {
			state, reason, changes, err := db.applyMailDraft(tx, v, connSeq, rec, d)
			if err != nil {
				return MailRecordResult{}, err
			}
			res.Changes = append(res.Changes, changes...)
			// The message's state is its most consequential draft.
			if mailMessageRank(state) > mailMessageRank(res.State) {
				res.State, res.Reason = state, reason
			}
		}
	}
	switch res.State {
	case MailMessageCandidate, MailMessageDuplicateEvent, MailMessageCancelled, MailMessageNoEvent,
		MailMessageSkipped, MailMessageExtractionFailed:
	default:
		return MailRecordResult{}, fmt.Errorf("runtimedb: %q is not a mail message state", res.State)
	}
	if seen {
		_, err = tx.Exec(`UPDATE mail_monitor_messages SET state = ?, reason = ?, source = ?, attempts = attempts + 1,
			run = ?, processed_at = ? WHERE connection = ? AND fingerprint = ?`,
			res.State, res.Reason, rec.Source, rec.Run, at, connSeq, rec.Fingerprint)
	} else {
		_, err = tx.Exec(`INSERT INTO mail_monitor_messages (connection, fingerprint, person_id, state, reason, source,
			attempts, run, first_seen_at, processed_at) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
			connSeq, rec.Fingerprint, rec.PersonID, res.State, res.Reason, rec.Source, rec.Run, at, at)
	}
	if err != nil {
		return MailRecordResult{}, fmt.Errorf("runtimedb: recording a mail message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return MailRecordResult{}, err
	}
	return res, nil
}

func mailMessageRank(state string) int {
	switch state {
	case MailMessageCandidate:
		return 5
	case MailMessageCancelled:
		return 4
	case MailMessageDuplicateEvent:
		return 3
	case MailMessageSkipped:
		return 2
	case MailMessageNoEvent:
		return 1
	}
	return 0
}

const mailCandidateColumns = `SELECT c.id, c.revision, c.person_id, ic.name, c.source_ref, c.fingerprint, c.source,
	c.uid_key, c.ics_sequence, c.message_key, c.sender_key, c.subject_key, c.target_calendar, c.state,
	c.state_reason, c.supersedes, c.superseded_by, c.update_pending, c.announced_at, c.created_at,
	c.updated_at, c.expires_at, c.resolved_at,
	r.title, r.start_at, r.end_at, r.duration_minutes, r.all_day, r.tz, r.location, r.notes, r.confidence,
	r.missing_json, r.ambiguous_json
	FROM mail_candidates c
	JOIN integration_connections ic ON ic.seq = c.connection
	JOIN mail_candidate_revisions r ON r.candidate = c.id AND r.revision = c.revision`

func scanMailCandidate(row rowScanner) (MailCandidate, error) {
	var c MailCandidate
	var announced, created, updated, expires, resolved, start, end, missing, ambiguous string
	var allDay int
	err := row.Scan(&c.ID, &c.Revision, &c.PersonID, &c.Connection, &c.SourceRef, &c.Fingerprint, &c.Source,
		&c.uidKey, &c.icsSequence, &c.messageKey, &c.senderKey, &c.subjectKey, &c.TargetCalendar, &c.State,
		&c.StateReason, &c.Supersedes, &c.SupersededBy, &c.UpdatePending, &announced, &created,
		&updated, &expires, &resolved,
		&c.Fields.Title, &start, &end, &c.Fields.DurationMinutes, &allDay, &c.Fields.TZ, &c.Fields.Location,
		&c.Fields.Notes, &c.Fields.Confidence, &missing, &ambiguous)
	if errors.Is(err, sql.ErrNoRows) {
		return MailCandidate{}, ErrMailCandidateNotFound
	}
	if err != nil {
		return MailCandidate{}, fmt.Errorf("runtimedb: scanning mail candidate: %w", err)
	}
	c.Fields.AllDay = allDay != 0
	for _, f := range []struct {
		s    string
		into *time.Time
	}{{announced, &c.AnnouncedAt}, {created, &c.CreatedAt}, {updated, &c.UpdatedAt}, {expires, &c.ExpiresAt},
		{resolved, &c.ResolvedAt}, {start, &c.Fields.Start}, {end, &c.Fields.End}} {
		if *f.into, err = parseMailTS(f.s); err != nil {
			return MailCandidate{}, err
		}
	}
	if err := json.Unmarshal([]byte(missing), &c.Fields.Missing); err != nil {
		return MailCandidate{}, err
	}
	if err := json.Unmarshal([]byte(ambiguous), &c.Fields.Ambiguous); err != nil {
		return MailCandidate{}, err
	}
	if c.Fields.Missing == nil {
		c.Fields.Missing = []string{}
	}
	if c.Fields.Ambiguous == nil {
		c.Fields.Ambiguous = []string{}
	}
	return c, nil
}

func mailCandidateTx(tx *sql.Tx, id string) (MailCandidate, error) {
	return scanMailCandidate(tx.QueryRow(mailCandidateColumns+` WHERE c.id = ?`, id))
}

func mailJSONList(list []string) string {
	if len(list) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(list)
	return string(b)
}

func mailAudit(tx *sql.Tx, id string, revision int, action, from, to, actor, reason string, at time.Time) error {
	_, err := tx.Exec(`INSERT INTO mail_candidate_audit (candidate, revision, action, from_state, to_state, actor, reason, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, revision, action, from, to, actor, BoundMailText(reason, 200), mailTS(at))
	if err != nil {
		return fmt.Errorf("runtimedb: auditing mail candidate: %w", err)
	}
	return nil
}

func insertMailRevision(tx *sql.Tx, id string, revision int, f MailCandidateFields, author string, at time.Time) error {
	_, err := tx.Exec(`INSERT INTO mail_candidate_revisions (candidate, revision, title, start_at, end_at,
		duration_minutes, all_day, tz, location, notes, confidence, missing_json, ambiguous_json, author, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, revision, f.Title, mailTS(f.Start), mailTS(f.End), f.DurationMinutes, boolInt(f.AllDay), f.TZ,
		f.Location, f.Notes, f.Confidence, mailJSONList(f.Missing), mailJSONList(f.Ambiguous), author, mailTS(at))
	if err != nil {
		return fmt.Errorf("runtimedb: recording candidate revision: %w", err)
	}
	return nil
}

// mailEventKey digests an event's identity; "" when it has no time.
func mailEventKey(f MailCandidateFields) string {
	if f.Start.IsZero() {
		return ""
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("mail-event/v1\x00%s\x00%s\x00%s\x00%t\x00%d",
		strings.ToLower(strings.TrimSpace(f.Title)), mailTS(f.Start), mailTS(f.End), f.AllDay, f.DurationMinutes)))
	return hex.EncodeToString(sum[:])
}

func newMailCandidateID() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "mc_" + hex.EncodeToString(b), nil
}

func mailStateFor(f MailCandidateFields) string {
	if f.NeedsDetails() {
		return MailCandidateNeedsDetails
	}
	return MailCandidateProposed
}

// resolveMailCandidateTx moves a candidate to a resolved state and drops
// its excerpt.
func resolveMailCandidateTx(tx *sql.Tx, c MailCandidate, to, reason, actor string, at time.Time) error {
	if _, err := tx.Exec(`UPDATE mail_candidates SET state = ?, state_reason = ?, resolved_at = ?, updated_at = ?
		WHERE id = ?`, to, BoundMailText(reason, 200), mailTS(at), mailTS(at), c.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM mail_candidate_excerpts WHERE candidate = ?`, c.ID); err != nil {
		return err
	}
	if to == MailCandidateDismissed {
		// Dismiss keeps only what stops the same mail being asked about
		// again: the fingerprint and the correlation digests.
		if _, err := tx.Exec(`UPDATE mail_candidate_revisions SET title = '', location = '', notes = ''
			WHERE candidate = ?`, c.ID); err != nil {
			return err
		}
	}
	return mailAudit(tx, c.ID, c.Revision, "resolve", c.State, to, actor, reason, at)
}

func (db *DB) applyMailDraft(tx *sql.Tx, v *MailExcerptVault, connSeq int64, rec MailMessageRecord, d MailCandidateDraft) (string, string, []MailCandidateChange, error) {
	at := rec.At
	// The candidate this draft corrects or cancels, if any.
	var prior MailCandidate
	var found bool
	var err error
	switch {
	case d.UIDKey != "":
		prior, err = scanMailCandidate(tx.QueryRow(mailCandidateColumns+`
			WHERE c.connection = ? AND c.uid_key = ? AND c.superseded_by = ''
			ORDER BY c.created_at DESC LIMIT 1`, connSeq, d.UIDKey))
	case d.Correction && d.SenderKey != "":
		// An empty key matches nothing: a message with no subject is not
		// "the same subject" as every other message with none.
		args := []any{connSeq, d.SenderKey, d.SubjectKey}
		cond := `(c.subject_key = ? AND c.subject_key <> '')`
		if len(d.ThreadKeys) > 0 {
			cond += ` OR c.message_key IN (` + strings.TrimSuffix(strings.Repeat("?,", len(d.ThreadKeys)), ",") + `)`
			for _, k := range d.ThreadKeys {
				args = append(args, k)
			}
		}
		prior, err = scanMailCandidate(tx.QueryRow(mailCandidateColumns+`
			WHERE c.connection = ? AND c.sender_key = ? AND c.superseded_by = '' AND c.source = 'model'
			  AND (`+cond+`) ORDER BY c.created_at DESC LIMIT 1`, args...))
	default:
		err = ErrMailCandidateNotFound
	}
	switch {
	case err == nil:
		found = true
	case errors.Is(err, ErrMailCandidateNotFound):
	default:
		return "", "", nil, err
	}
	// A cancellation or a correction is honoured only from whoever sent
	// what it corrects: a stranger who learns an invitation's UID cannot
	// cancel or rewrite it.
	if found && prior.senderKey != d.SenderKey {
		if d.Cancel {
			return MailMessageSkipped, "cancel_sender_mismatch", nil, nil
		}
		found = false
	}

	if d.Cancel {
		if !found {
			return MailMessageNoEvent, "cancel_unknown_event", nil, nil
		}
		switch {
		case prior.Unresolved():
			if err := resolveMailCandidateTx(tx, prior, MailCandidateCancelled, "the organizer cancelled it", MailMonitorActor, at); err != nil {
				return "", "", nil, err
			}
			prior.State = MailCandidateCancelled
			return MailMessageCancelled, "", []MailCandidateChange{{Kind: MailChangeCancelled, Candidate: prior}}, nil
		case mailEventCommitted(prior.State):
			if _, err := tx.Exec(`UPDATE mail_candidates SET update_pending = 'cancel', announced_at = '', updated_at = ? WHERE id = ?`,
				mailTS(at), prior.ID); err != nil {
				return "", "", nil, err
			}
			if err := mailAudit(tx, prior.ID, prior.Revision, "update_pending", prior.State, prior.State, MailMonitorActor, "cancel", at); err != nil {
				return "", "", nil, err
			}
			prior.UpdatePending = MailUpdateCancel
			return MailMessageCancelled, "", []MailCandidateChange{{Kind: MailChangeUpdateNeeded, Candidate: prior}}, nil
		}
		return MailMessageCancelled, "already_resolved", nil, nil
	}

	if found && d.UIDKey != "" && d.ICSSequence < prior.icsSequence {
		return MailMessageDuplicateEvent, "stale_sequence", nil, nil
	}
	if found && prior.Fields.SameEvent(d.Fields) {
		return MailMessageDuplicateEvent, "", nil, nil
	}
	if !found {
		// A reminder or resend of an event already on record from this
		// mailbox — even one the Person dismissed — is not a new question.
		var dup int
		if ek := mailEventKey(d.Fields); ek != "" {
			if err := tx.QueryRow(`SELECT COUNT(*) FROM mail_candidates
				WHERE connection = ? AND superseded_by = '' AND event_key = ?`, connSeq, ek).Scan(&dup); err != nil {
				return "", "", nil, err
			}
		}
		if dup > 0 {
			return MailMessageDuplicateEvent, "", nil, nil
		}
	}

	id, err := newMailCandidateID()
	if err != nil {
		return "", "", nil, err
	}
	state := mailStateFor(d.Fields)
	expires := at.Add(rec.Window)
	if s := d.Fields.Start; !s.IsZero() && s.After(at) && s.Before(expires) {
		expires = s
	}
	supersedes := ""
	if found {
		supersedes = prior.ID
	}
	if _, err := tx.Exec(`INSERT INTO mail_candidates (id, person_id, connection, source_ref, fingerprint, source,
		uid_key, ics_sequence, message_key, sender_key, subject_key, event_key, target_calendar, revision, state,
		supersedes, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?)`,
		id, rec.PersonID, connSeq, rec.SourceRef, rec.Fingerprint, d.Source, d.UIDKey, d.ICSSequence,
		d.MessageKey, d.SenderKey, d.SubjectKey, mailEventKey(d.Fields), rec.TargetCalendar, state, supersedes,
		mailTS(at), mailTS(at), mailTS(expires)); err != nil {
		return "", "", nil, fmt.Errorf("runtimedb: recording mail candidate: %w", err)
	}
	if err := insertMailRevision(tx, id, 1, d.Fields, MailMonitorActor, at); err != nil {
		return "", "", nil, err
	}
	if err := mailAudit(tx, id, 1, "create", "", state, MailMonitorActor, d.Source, at); err != nil {
		return "", "", nil, err
	}
	if v != nil && strings.TrimSpace(d.Excerpt) != "" {
		sealed, err := v.seal(id, rec.PersonID, BoundMailText(d.Excerpt, MaxMailExcerpt))
		if err != nil {
			return "", "", nil, err
		}
		exp := at.Add(rec.ExcerptWindow)
		if expires.Before(exp) {
			exp = expires
		}
		if _, err := tx.Exec(`INSERT INTO mail_candidate_excerpts (candidate, sealed, expires_at) VALUES (?, ?, ?)`,
			id, sealed, mailTS(exp)); err != nil {
			return "", "", nil, fmt.Errorf("runtimedb: sealing mail excerpt: %w", err)
		}
	}
	created, err := mailCandidateTx(tx, id)
	if err != nil {
		return "", "", nil, err
	}
	changes := []MailCandidateChange{{Kind: MailChangeNew, Candidate: created}}
	if found {
		if _, err := tx.Exec(`UPDATE mail_candidates SET superseded_by = ?, updated_at = ? WHERE id = ?`,
			id, mailTS(at), prior.ID); err != nil {
			return "", "", nil, err
		}
		prior.SupersededBy = id
		switch {
		case prior.Unresolved():
			if err := resolveMailCandidateTx(tx, prior, MailCandidateSuperseded, "corrected by a later message", MailMonitorActor, at); err != nil {
				return "", "", nil, err
			}
			prior.State = MailCandidateSuperseded
			changes = append(changes, MailCandidateChange{Kind: MailChangeSuperseded, Candidate: prior})
		case mailEventCommitted(prior.State):
			if _, err := tx.Exec(`UPDATE mail_candidates SET update_pending = 'update', announced_at = '' WHERE id = ?`, prior.ID); err != nil {
				return "", "", nil, err
			}
			if err := mailAudit(tx, prior.ID, prior.Revision, "update_pending", prior.State, prior.State, MailMonitorActor, "update", at); err != nil {
				return "", "", nil, err
			}
			prior.UpdatePending = MailUpdateEvent
			changes = append(changes, MailCandidateChange{Kind: MailChangeUpdateNeeded, Candidate: prior})
		}
	}
	return MailMessageCandidate, "", changes, nil
}

// ---------------------------------------------------------------------
// Person-scoped reads and writes.

// mailViewer resolves an acting handle to the canonical Person it may
// read candidates for. Only an active member resolves; an Agent, a
// machine principal, a disabled or offboarded Person resolves to
// nothing, and nothing is ErrMailCandidateNotFound.
func (db *DB) mailViewer(actor string) (string, error) {
	if strings.TrimSpace(actor) == "" {
		return "", ErrMailCandidateNotFound
	}
	m, err := db.GetButlerMember(actor)
	if err != nil || !m.Active() || m.PersonID == "" {
		return "", ErrMailCandidateNotFound
	}
	return m.PersonID, nil
}

// MailCandidateFilter narrows ListMailCandidates.
type MailCandidateFilter struct {
	States     []string
	Connection string
	Limit      int
}

// ListMailCandidates lists the acting Person's own candidates, newest
// first. There is no household-wide or Owner listing.
func (db *DB) ListMailCandidates(actor string, f MailCandidateFilter) ([]MailCandidate, error) {
	person, err := db.mailViewer(actor)
	if err != nil {
		return nil, nil
	}
	query := mailCandidateColumns + ` WHERE c.person_id = ?`
	args := []any{person}
	if len(f.States) > 0 {
		query += ` AND c.state IN (` + strings.TrimSuffix(strings.Repeat("?,", len(f.States)), ",") + `)`
		for _, s := range f.States {
			args = append(args, s)
		}
	}
	if f.Connection != "" {
		query += ` AND ic.name = ?`
		args = append(args, f.Connection)
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	query += ` ORDER BY c.created_at DESC, c.id LIMIT ?`
	args = append(args, limit)
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing mail candidates: %w", err)
	}
	defer rows.Close()
	var out []MailCandidate
	for rows.Next() {
		c, err := scanMailCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetMailCandidate returns one of the acting Person's candidates.
func (db *DB) GetMailCandidate(actor, id string) (MailCandidate, error) {
	person, err := db.mailViewer(actor)
	if err != nil {
		return MailCandidate{}, err
	}
	c, err := scanMailCandidate(db.sql.QueryRow(mailCandidateColumns+` WHERE c.id = ? AND c.person_id = ?`, id, person))
	if err != nil {
		return MailCandidate{}, err
	}
	return c, nil
}

// GetMailCandidateRevision returns one revision's fields of the acting
// Person's candidate — what an approval token bound to (id, revision)
// must be checked against.
func (db *DB) GetMailCandidateRevision(actor, id string, revision int) (MailCandidateFields, error) {
	c, err := db.GetMailCandidate(actor, id)
	if err != nil {
		return MailCandidateFields{}, err
	}
	row := db.sql.QueryRow(strings.Replace(mailCandidateColumns, "r.revision = c.revision", "r.revision = ?", 1)+
		` WHERE c.id = ?`, revision, c.ID)
	rc, err := scanMailCandidate(row)
	if err != nil {
		return MailCandidateFields{}, err
	}
	return rc.Fields, nil
}

// ReadMailCandidateExcerpt opens the acting Person's candidate excerpt,
// or "" when there is none (resolved, expired, never kept, or a restore
// discarded it).
func (db *DB) ReadMailCandidateExcerpt(v *MailExcerptVault, actor, id string, at time.Time) (string, error) {
	c, err := db.GetMailCandidate(actor, id)
	if err != nil {
		return "", err
	}
	if v == nil || !c.Unresolved() && c.State != MailCandidateAccepted {
		return "", nil
	}
	var sealed []byte
	var expires string
	err = db.sql.QueryRow(`SELECT sealed, expires_at FROM mail_candidate_excerpts WHERE candidate = ?`, c.ID).Scan(&sealed, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if exp, err := parseMailTS(expires); err != nil || !at.Before(exp) {
		return "", nil
	}
	plain, err := v.open(c.ID, c.PersonID, sealed)
	if err != nil {
		return "", nil
	}
	return string(plain), nil
}

// ReviseMailCandidate is Edit: a new revision with the Person's fields,
// from exactly the revision they were shown. The state is recomputed
// (needs_details until nothing is missing), and an accepted candidate
// returns to review. A resolved candidate cannot be edited.
func (db *DB) ReviseMailCandidate(actor, id string, baseRevision int, f MailCandidateFields, at time.Time) (MailCandidate, error) {
	person, err := db.mailViewer(actor)
	if err != nil {
		return MailCandidate{}, err
	}
	f.Title = strings.TrimSpace(f.Title)
	if err := f.validate(); err != nil {
		return MailCandidate{}, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return MailCandidate{}, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := mailCandidateTx(tx, id)
	if err != nil || c.PersonID != person {
		return MailCandidate{}, ErrMailCandidateNotFound
	}
	if c.Revision != baseRevision || !(c.Unresolved() || c.State == MailCandidateAccepted) {
		return MailCandidate{}, ErrMailCandidateStale
	}
	next := c.Revision + 1
	if err := insertMailRevision(tx, id, next, f, actor, at); err != nil {
		return MailCandidate{}, err
	}
	state := mailStateFor(f)
	expires := c.ExpiresAt
	if s := f.Start; !s.IsZero() && s.After(at) && s.Before(expires) {
		expires = s
	}
	if _, err := tx.Exec(`UPDATE mail_candidates SET revision = ?, state = ?, state_reason = '', updated_at = ?, expires_at = ?,
		event_key = ? WHERE id = ? AND revision = ?`, next, state, mailTS(at), mailTS(expires), mailEventKey(f), id, baseRevision); err != nil {
		return MailCandidate{}, err
	}
	if err := mailAudit(tx, id, next, "revise", c.State, state, actor, "", at); err != nil {
		return MailCandidate{}, err
	}
	out, err := mailCandidateTx(tx, id)
	if err != nil {
		return MailCandidate{}, err
	}
	return out, tx.Commit()
}

// mailTransitions is the closed lifecycle a Person (through S06's
// review) may drive. Expiry, supersession and cancellation are the
// monitor's and the sweep's, never a caller's.
var mailTransitions = map[string]map[string]bool{
	MailCandidateProposed:     {MailCandidateAccepted: true, MailCandidateDismissed: true},
	MailCandidateNeedsDetails: {MailCandidateDismissed: true},
	// Accepted returns to review when a new conflict appears, or becomes
	// created when the write lands.
	MailCandidateAccepted: {MailCandidateCreated: true, MailCandidateProposed: true},
}

// TransitionMailCandidate moves the acting Person's candidate at exactly
// revision to state to. Accepting requires the dispatch gate (no restore
// quarantine, no household stop). Created, dismissed and every other
// resolution drop the excerpt; dismissed also blanks the fields.
func (db *DB) TransitionMailCandidate(actor, id string, revision int, to, reason string, at time.Time) (MailCandidate, error) {
	person, err := db.mailViewer(actor)
	if err != nil {
		return MailCandidate{}, err
	}
	if to == MailCandidateAccepted || to == MailCandidateCreated {
		if err := db.gate(GateDispatch); err != nil {
			return MailCandidate{}, err
		}
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return MailCandidate{}, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := mailCandidateTx(tx, id)
	if err != nil || c.PersonID != person {
		return MailCandidate{}, ErrMailCandidateNotFound
	}
	if c.Revision != revision || !mailTransitions[c.State][to] {
		return MailCandidate{}, fmt.Errorf("%w: %s at revision %d cannot become %s", ErrMailCandidateStale, c.State, c.Revision, to)
	}
	switch to {
	case MailCandidateCreated, MailCandidateDismissed:
		if err := resolveMailCandidateTx(tx, c, to, reason, actor, at); err != nil {
			return MailCandidate{}, err
		}
	default:
		if _, err := tx.Exec(`UPDATE mail_candidates SET state = ?, state_reason = ?, updated_at = ? WHERE id = ? AND revision = ?`,
			to, BoundMailText(reason, 200), mailTS(at), id, revision); err != nil {
			return MailCandidate{}, err
		}
		if err := mailAudit(tx, id, revision, "transition", c.State, to, actor, reason, at); err != nil {
			return MailCandidate{}, err
		}
	}
	out, err := mailCandidateTx(tx, id)
	if err != nil {
		return MailCandidate{}, err
	}
	return out, tx.Commit()
}

// ClearMailCandidateUpdate records that the acting Person's pending
// update or cancel was proposed and settled (S06).
func (db *DB) ClearMailCandidateUpdate(actor, id string, at time.Time) error {
	person, err := db.mailViewer(actor)
	if err != nil {
		return err
	}
	res, err := db.sql.Exec(`UPDATE mail_candidates SET update_pending = '', updated_at = ?
		WHERE id = ? AND person_id = ? AND update_pending <> ''`, mailTS(at), id, person)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMailCandidateNotFound
	}
	return nil
}

// ---------------------------------------------------------------------
// Monitor-internal: the candidate-ready hand-off. These return
// candidates to trusted in-process code only (the hook that sends the
// Person's private review); nothing here is, or may be put, behind an
// API route.

// UnannouncedMailCandidates returns candidates on a connection whose
// hand-off to the review hook has not been acknowledged: unresolved ones
// never announced, and committed ones with an update pending.
func (db *DB) UnannouncedMailCandidates(connection string, limit int) ([]MailCandidate, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := db.sql.Query(mailCandidateColumns+` WHERE ic.name = ? AND c.announced_at = ''
		AND (c.state IN ('proposed', 'needs_details') OR c.update_pending <> '')
		ORDER BY c.created_at, c.id LIMIT ?`, connection, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailCandidate
	for rows.Next() {
		c, err := scanMailCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkMailCandidateAnnounced records the hook's acknowledgement. A
// later update_pending re-arms it (the correction clears announced_at).
func (db *DB) MarkMailCandidateAnnounced(id string, at time.Time) error {
	_, err := db.sql.Exec(`UPDATE mail_candidates SET announced_at = ? WHERE id = ?`, mailTS(at), id)
	return err
}

// ---------------------------------------------------------------------
// Retention and purge.

// MailRetention is how long each kind of row lives.
type MailRetention struct {
	// Resolved: dismissed, superseded, expired and cancelled candidates
	// are deleted this long after resolution (their message row still
	// dedupes).
	Resolved time.Duration
	// Created: a created candidate is kept this long after its event
	// ends, so a correction can still become an update proposal.
	Created time.Duration
	// Messages: processing state, which dedupes replays; longer than any
	// plausible UIDVALIDITY-reset replay.
	Messages time.Duration
	// WorkResults: a finished mail read's result (which carries the
	// normalized messages) is scrubbed from the integration-work queue
	// this long after it finished.
	WorkResults time.Duration
}

// DefaultMailRetention is the shipped policy.
func DefaultMailRetention() MailRetention {
	return MailRetention{
		Resolved:    30 * 24 * time.Hour,
		Created:     180 * 24 * time.Hour,
		Messages:    400 * 24 * time.Hour,
		WorkResults: 15 * time.Minute,
	}
}

// MailSweepResult counts what a sweep did.
type MailSweepResult struct {
	Expired           int
	Revoked           int
	ExcerptsPurged    int
	CandidatesDeleted int
	MessagesDeleted   int
	WorkScrubbed      int
}

// mailCapabilities are the mail reads whose results carry message
// content.
var mailCapabilities = []any{"mail.changes", "mail.message", "mail.search"}

// SweepMailMonitor expires, purges and scrubs. It is safe to run at any
// cadence; nothing it removes is needed by anyone who still has
// authority to see it.
func (db *DB) SweepMailMonitor(at time.Time, r MailRetention) (MailSweepResult, error) {
	var out MailSweepResult
	d := DefaultMailRetention()
	if r.Resolved <= 0 {
		r.Resolved = d.Resolved
	}
	if r.Created <= 0 {
		r.Created = d.Created
	}
	if r.Messages <= 0 {
		r.Messages = d.Messages
	}
	if r.WorkResults <= 0 {
		r.WorkResults = d.WorkResults
	}
	// Authority first: a connection whose Person revoked the monitor, was
	// offboarded or disabled, or that is gone, keeps no unresolved
	// candidate and no excerpt.
	conns, err := db.sql.Query(`SELECT DISTINCT ic.name FROM mail_candidates c
		JOIN integration_connections ic ON ic.seq = c.connection
		WHERE c.state IN ('proposed', 'needs_details', 'accepted')
		   OR c.id IN (SELECT candidate FROM mail_candidate_excerpts)`)
	if err != nil {
		return out, err
	}
	var names []string
	for conns.Next() {
		var n string
		if err := conns.Scan(&n); err != nil {
			conns.Close()
			return out, err
		}
		names = append(names, n)
	}
	conns.Close()
	for _, name := range names {
		if reason := db.mailAuthorityGone(name); reason != "" {
			n, err := db.PurgeMailConnection(name, MailMonitorActor, reason, at)
			if err != nil {
				return out, err
			}
			out.Revoked += n
		}
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	stamp := mailTS(at)
	expiring, err := mailCandidatesWhere(tx, `c.state IN ('proposed', 'needs_details') AND c.expires_at <= ?`, stamp)
	if err != nil {
		return out, err
	}
	for _, c := range expiring {
		if err := resolveMailCandidateTx(tx, c, MailCandidateExpired, "no answer before it expired", MailMonitorActor, at); err != nil {
			return out, err
		}
		out.Expired++
	}
	res, err := tx.Exec(`DELETE FROM mail_candidate_excerpts WHERE expires_at <= ?
		OR candidate IN (SELECT id FROM mail_candidates WHERE state NOT IN ('proposed', 'needs_details', 'accepted'))`, stamp)
	if err != nil {
		return out, err
	}
	n, _ := res.RowsAffected()
	out.ExcerptsPurged = int(n)

	resolvedBefore := mailTS(at.Add(-r.Resolved))
	createdBefore := mailTS(at.Add(-r.Created))
	res, err = tx.Exec(`DELETE FROM mail_candidates WHERE
		(state IN ('dismissed', 'superseded', 'expired', 'cancelled') AND resolved_at <> '' AND resolved_at <= ?
		  AND id NOT IN (SELECT supersedes FROM mail_candidates WHERE supersedes <> '' AND state IN ('proposed', 'needs_details', 'accepted', 'created')))
		OR (state = 'created' AND update_pending = '' AND resolved_at <> '' AND resolved_at <= ?
		  AND id NOT IN (SELECT c.id FROM mail_candidates c JOIN mail_candidate_revisions r
		      ON r.candidate = c.id AND r.revision = c.revision WHERE r.end_at > ?))`,
		resolvedBefore, createdBefore, createdBefore)
	if err != nil {
		return out, err
	}
	n, _ = res.RowsAffected()
	out.CandidatesDeleted = int(n)
	if _, err := tx.Exec(`DELETE FROM mail_candidate_audit WHERE candidate NOT IN (SELECT id FROM mail_candidates)`); err != nil {
		return out, err
	}
	res, err = tx.Exec(`DELETE FROM mail_monitor_messages WHERE processed_at <= ?
		AND fingerprint NOT IN (SELECT fingerprint FROM mail_candidates WHERE connection = mail_monitor_messages.connection)`,
		mailTS(at.Add(-r.Messages)))
	if err != nil {
		return out, err
	}
	n, _ = res.RowsAffected()
	out.MessagesDeleted = int(n)
	res, err = tx.Exec(`UPDATE integration_work SET result_json = '{}'
		WHERE capability IN (?, ?, ?) AND state IN ('done', 'expired', 'cancelled')
		  AND result_json <> '{}' AND finished_at <> '' AND finished_at <= ?`,
		append(mailCapabilities, at.Add(-r.WorkResults).UTC().Format(time.RFC3339Nano))...)
	if err != nil {
		return out, err
	}
	n, _ = res.RowsAffected()
	out.WorkScrubbed = int(n)
	return out, tx.Commit()
}

// mailAuthorityGone says why a connection may no longer hold unresolved
// candidates, or "" when it may.
func (db *DB) mailAuthorityGone(connection string) string {
	conn, err := db.GetConnection(connection)
	if err != nil {
		return "the mailbox connection was removed"
	}
	m, err := db.GetButlerMember(conn.Owner)
	if err != nil || !m.Active() {
		return "the mailbox owner is no longer an active member"
	}
	if _, err := db.LiveBackgroundAuthorization(connection); err != nil {
		return "the mailbox owner revoked the monitor"
	}
	return ""
}

func mailCandidatesWhere(tx *sql.Tx, where string, args ...any) ([]MailCandidate, error) {
	rows, err := tx.Query(mailCandidateColumns+` WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailCandidate
	for rows.Next() {
		c, err := scanMailCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PurgeMailConnection is the revocation and offboarding purge: every
// unresolved (or accepted, not yet created) candidate on the connection
// expires with reason, and every excerpt is deleted. Created candidates
// and message dedupe state stay, so a later re-enable does not ask about
// the same mail again. It returns how many candidates it expired.
func (db *DB) PurgeMailConnection(connection, actor, reason string, at time.Time) (int, error) {
	if actor == "" || reason == "" {
		return 0, fmt.Errorf("runtimedb: purging a mailbox's candidates needs who and why")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var seq int64
	if err := tx.QueryRow(`SELECT seq FROM integration_connections WHERE name = ?`, connection).Scan(&seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	open, err := mailCandidatesWhere(tx, `c.connection = ? AND c.state IN ('proposed', 'needs_details', 'accepted')`, seq)
	if err != nil {
		return 0, err
	}
	for _, c := range open {
		if err := resolveMailCandidateTx(tx, c, MailCandidateExpired, reason, actor, at); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(`DELETE FROM mail_candidate_excerpts WHERE candidate IN
		(SELECT id FROM mail_candidates WHERE connection = ?)`, seq); err != nil {
		return 0, err
	}
	return len(open), tx.Commit()
}

// ScrubIntegrationWorkResult blanks one finished work item's result — a
// mail read's answer, once its messages are processed.
func (db *DB) ScrubIntegrationWorkResult(seq int64) error {
	_, err := db.sql.Exec(`UPDATE integration_work SET result_json = '{}'
		WHERE seq = ? AND state IN ('done', 'expired', 'cancelled')`, seq)
	return err
}

// purgeMailOnRestore runs on a restored copy: no excerpt survives a
// restore (its key is not in the recovery set anyway), and no mail read
// result is revived from a snapshot.
func purgeMailOnRestore(db *DB) error {
	if _, err := db.sql.Exec(`DELETE FROM mail_candidate_excerpts`); err != nil {
		return err
	}
	_, err := db.sql.Exec(`UPDATE integration_work SET result_json = '{}' WHERE capability IN (?, ?, ?)`, mailCapabilities...)
	return err
}
