package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
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
}

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
	res, err := db.sql.Exec(
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
	return butlerSessionID(seq), nil
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
	return s, nil
}
