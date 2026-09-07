package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ButlerSessionMode is the tool surface a session ran with (ADR-W017 A4).
type ButlerSessionMode string

const (
	ButlerModeFull   ButlerSessionMode = "full"
	ButlerModeQAOnly ButlerSessionMode = "qa-only"
)

// ButlerSurface is which Butler a session was talking to (ADR-W026 A1).
// The set is closed, and the value is decided by the endpoint the
// session arrived on — never by a request body, an adapter, or the
// model. It is stored because the two surfaces have different ceilings,
// and an audit read a year from now has to be able to tell which one
// answered.
type ButlerSurface string

const (
	// ButlerSurfaceAdmin is the operator's butler: the Statefile read
	// tools and propose_change (ADR-W006, ADR-W017).
	ButlerSurfaceAdmin ButlerSurface = "admin"
	// ButlerSurfaceFamily is the household's butler: household context
	// and domestic-tier actions, with no Statefile access of any kind
	// (ADR-W026 A1).
	ButlerSurfaceFamily ButlerSurface = "family"
)

// ValidButlerSurface reports whether s is one of the two surfaces.
func ValidButlerSurface(s ButlerSurface) bool {
	return s == ButlerSurfaceAdmin || s == ButlerSurfaceFamily
}

// ButlerSession is one persisted Butler conversation (ADR-W017 A1):
// principal-bound, transcript included. TranscriptJSON is omitted from
// list results (it can be large); Get returns it.
//
// FinalText and TranscriptJSON are the LAST turn's, kept because every
// existing reader — the CLI, the chat adapters, an audit query — asks
// "what did it say" and would otherwise have to walk Turns to find out.
// Turns is the whole conversation, oldest first (ADR-W027).
type ButlerSession struct {
	Seq            int64             `json:"seq"`
	ID             string            `json:"id"` // "bs-<seq>"
	CreatedAt      time.Time         `json:"createdAt"`
	Principal      string            `json:"principal"`
	ActingFor      string            `json:"actingFor"`
	Provider       string            `json:"provider"`
	Mode           ButlerSessionMode `json:"mode"`
	Surface        ButlerSurface     `json:"surface"`
	FinalText      string            `json:"finalText"`
	Capped         bool              `json:"capped"`
	TranscriptJSON string            `json:"transcriptJson,omitempty"`
	// Turns is set by GetButlerSession and by AppendButlerTurns; it is
	// omitted from list results for the same reason the transcript is.
	Turns []ButlerTurn `json:"turns,omitempty"`
}

// ButlerTurn is one message in a session: what was asked, or what was
// answered. Ordinal is dense from zero and is what "in order" means —
// created_at can tie when a question and its answer are written in the
// same transaction.
//
// TranscriptJSON and PlanIDs belong to an assistant turn: the reads that
// produced THIS answer, and the plans THIS answer proposed. Keeping them
// per-turn is what lets an audit ask "which reads produced that" of a
// session that has answered three times.
type ButlerTurn struct {
	Ordinal        int       `json:"ordinal"`
	Role           string    `json:"role"` // "user" | "assistant"
	Text           string    `json:"text"`
	CreatedAt      time.Time `json:"createdAt"`
	Capped         bool      `json:"capped,omitempty"`
	TranscriptJSON string    `json:"transcriptJson,omitempty"`
	PlanIDs        []string  `json:"planIds,omitempty"`
}

// ButlerRoleUser and ButlerRoleAssistant are the only two roles a turn
// can carry. The tool traffic is not a turn: it is recorded inside the
// assistant turn it produced.
const (
	ButlerRoleUser      = "user"
	ButlerRoleAssistant = "assistant"
)

func butlerSessionID(seq int64) string { return fmt.Sprintf("bs-%d", seq) }

// InsertButlerSession records one completed session and returns its id.
// ActingFor is mandatory: a session not bound to a caller is an ADR-W001
// violation, refused here rather than lint-hoped away.
func (db *DB) InsertButlerSession(s ButlerSession) (string, error) {
	if s.Principal == "" || s.ActingFor == "" {
		return "", fmt.Errorf("runtimedb: butler session requires principal and acting_for (ADR-W001)")
	}
	// A session with no surface is refused rather than defaulted. Both
	// defaults are wrong in a way that matters: defaulting to admin files
	// a household conversation under the operator's ceiling, and
	// defaulting to family hides an operator session from an audit of
	// what touched the Statefile (ADR-W026 A1).
	if !ValidButlerSurface(s.Surface) {
		return "", fmt.Errorf("runtimedb: butler session requires a valid surface (admin|family), got %q (ADR-W026 A1)", s.Surface)
	}
	if s.TranscriptJSON == "" {
		s.TranscriptJSON = "{}"
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return "", fmt.Errorf("runtimedb: inserting butler session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(
		`INSERT INTO butler_sessions (created_at, principal, acting_for, provider, mode, surface, final_text, capped, transcript_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		now(), s.Principal, s.ActingFor, s.Provider, string(s.Mode), string(s.Surface), s.FinalText,
		boolInt(s.Capped), s.TranscriptJSON)
	if err != nil {
		return "", fmt.Errorf("runtimedb: inserting butler session: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return "", err
	}
	// The turns go in with the session, not after it: a session row
	// whose conversation failed to write is a record of an answer with
	// no question, which is the state migration 0013 had to backfill
	// around and should not be created again.
	if err := insertTurns(tx, seq, 0, s.Turns); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("runtimedb: inserting butler session: %w", err)
	}
	return butlerSessionID(seq), nil
}

// insertTurns writes turns at ordinals starting from base, inside tx.
func insertTurns(tx *sql.Tx, session int64, base int, turns []ButlerTurn) error {
	for i, t := range turns {
		if t.Role != ButlerRoleUser && t.Role != ButlerRoleAssistant {
			return fmt.Errorf("runtimedb: butler turn role must be user or assistant, got %q", t.Role)
		}
		if t.TranscriptJSON == "" {
			t.TranscriptJSON = "{}"
		}
		if _, err := tx.Exec(
			`INSERT INTO butler_session_turns (session, ordinal, role, text, created_at, capped, tool_json, plan_ids)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			session, base+i, t.Role, t.Text, now(), boolInt(t.Capped), t.TranscriptJSON,
			strings.Join(t.PlanIDs, ","),
		); err != nil {
			return fmt.Errorf("runtimedb: inserting butler turn: %w", err)
		}
	}
	return nil
}

// AppendButlerTurns adds turns to an existing session and moves the
// session's own final_text, capped and transcript to the newest
// assistant turn, so a reader that only knows about final_text keeps
// seeing the latest answer (ADR-W027).
//
// It refuses a session that does not exist rather than creating one:
// a follow-up to a conversation that is not there is a bug in the
// caller, not a new conversation.
func (db *DB) AppendButlerTurns(id string, turns []ButlerTurn) error {
	var seq int64
	if _, err := fmt.Sscanf(id, "bs-%d", &seq); err != nil {
		return ErrNotFound
	}
	if len(turns) == 0 {
		return nil
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: appending butler turns: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var next int
	err = tx.QueryRow(
		`SELECT COALESCE(MAX(ordinal) + 1, 0) FROM butler_session_turns WHERE session = ?`, seq,
	).Scan(&next)
	if err != nil {
		return fmt.Errorf("runtimedb: appending butler turns: %w", err)
	}
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM butler_sessions WHERE seq = ?`, seq).Scan(&exists); err != nil {
		return fmt.Errorf("runtimedb: appending butler turns: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	if err := insertTurns(tx, seq, next, turns); err != nil {
		return err
	}
	for _, t := range turns {
		if t.Role != ButlerRoleAssistant {
			continue
		}
		transcript := t.TranscriptJSON
		if transcript == "" {
			transcript = "{}"
		}
		if _, err := tx.Exec(
			`UPDATE butler_sessions SET final_text = ?, capped = ?, transcript_json = ? WHERE seq = ?`,
			t.Text, boolInt(t.Capped), transcript, seq,
		); err != nil {
			return fmt.Errorf("runtimedb: appending butler turns: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: appending butler turns: %w", err)
	}
	return nil
}

// ListButlerTurns returns one session's conversation, oldest first.
func (db *DB) ListButlerTurns(seq int64) ([]ButlerTurn, error) {
	rows, err := db.sql.Query(
		`SELECT ordinal, role, text, created_at, capped, tool_json, plan_ids
		 FROM butler_session_turns WHERE session = ? ORDER BY ordinal`, seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler turns: %w", err)
	}
	defer rows.Close()
	var out []ButlerTurn
	for rows.Next() {
		var t ButlerTurn
		var created, planIDs string
		var capped int
		if err := rows.Scan(&t.Ordinal, &t.Role, &t.Text, &created, &capped, &t.TranscriptJSON, &planIDs); err != nil {
			return nil, err
		}
		t.Capped = capped != 0
		if planIDs != "" {
			t.PlanIDs = strings.Split(planIDs, ",")
		}
		if t.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("runtimedb: parsing butler turn created_at: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListButlerSessions returns the newest limit sessions, newest first,
// WITHOUT transcripts. An empty surface lists both; naming one lists
// only that surface's sessions (ADR-W026 A1).
func (db *DB) ListButlerSessions(limit int, surface ButlerSurface) ([]ButlerSession, error) {
	if limit <= 0 {
		limit = 50
	}
	where, args := "", []any{}
	if surface != "" {
		where = " WHERE surface = ?"
		args = append(args, string(surface))
	}
	args = append(args, limit)
	rows, err := db.sql.Query(
		`SELECT seq, created_at, principal, acting_for, provider, mode, surface, final_text, capped
		 FROM butler_sessions`+where+` ORDER BY seq DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler sessions: %w", err)
	}
	defer rows.Close()
	var out []ButlerSession
	for rows.Next() {
		var s ButlerSession
		var created, mode, surface string
		var capped int
		if err := rows.Scan(&s.Seq, &created, &s.Principal, &s.ActingFor, &s.Provider, &mode, &surface, &s.FinalText, &capped); err != nil {
			return nil, err
		}
		s.ID = butlerSessionID(s.Seq)
		s.Mode = ButlerSessionMode(mode)
		s.Surface = ButlerSurface(surface)
		s.Capped = capped != 0
		if s.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("runtimedb: parsing butler session created_at: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetButlerSession returns one session including its transcript.
func (db *DB) GetButlerSession(id string) (ButlerSession, error) {
	var seq int64
	if _, err := fmt.Sscanf(id, "bs-%d", &seq); err != nil {
		return ButlerSession{}, ErrNotFound
	}
	row := db.sql.QueryRow(
		`SELECT seq, created_at, principal, acting_for, provider, mode, surface, final_text, capped, transcript_json
		 FROM butler_sessions WHERE seq = ?`, seq)
	var s ButlerSession
	var created, mode, surface string
	var capped int
	err := row.Scan(&s.Seq, &created, &s.Principal, &s.ActingFor, &s.Provider, &mode, &surface, &s.FinalText, &capped, &s.TranscriptJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ButlerSession{}, ErrNotFound
	}
	if err != nil {
		return ButlerSession{}, fmt.Errorf("runtimedb: reading butler session: %w", err)
	}
	s.ID = butlerSessionID(s.Seq)
	s.Mode = ButlerSessionMode(mode)
	s.Surface = ButlerSurface(surface)
	s.Capped = capped != 0
	if s.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return ButlerSession{}, fmt.Errorf("runtimedb: parsing butler session created_at: %w", err)
	}
	if s.Turns, err = db.ListButlerTurns(s.Seq); err != nil {
		return ButlerSession{}, err
	}
	return s, nil
}
