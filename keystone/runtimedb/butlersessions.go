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
	if s.TranscriptJSON == "" {
		s.TranscriptJSON = "{}"
	}
	res, err := db.sql.Exec(
		`INSERT INTO butler_sessions (created_at, principal, acting_for, provider, mode, final_text, capped, transcript_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		now(), s.Principal, s.ActingFor, s.Provider, string(s.Mode), s.FinalText, boolInt(s.Capped), s.TranscriptJSON)
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
// WITHOUT transcripts.
func (db *DB) ListButlerSessions(limit int) ([]ButlerSession, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.sql.Query(
		`SELECT seq, created_at, principal, acting_for, provider, mode, final_text, capped
		 FROM butler_sessions ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler sessions: %w", err)
	}
	defer rows.Close()
	var out []ButlerSession
	for rows.Next() {
		var s ButlerSession
		var created, mode string
		var capped int
		if err := rows.Scan(&s.Seq, &created, &s.Principal, &s.ActingFor, &s.Provider, &mode, &s.FinalText, &capped); err != nil {
			return nil, err
		}
		s.ID = butlerSessionID(s.Seq)
		s.Mode = ButlerSessionMode(mode)
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
		`SELECT seq, created_at, principal, acting_for, provider, mode, final_text, capped, transcript_json
		 FROM butler_sessions WHERE seq = ?`, seq)
	var s ButlerSession
	var created, mode string
	var capped int
	err := row.Scan(&s.Seq, &created, &s.Principal, &s.ActingFor, &s.Provider, &mode, &s.FinalText, &capped, &s.TranscriptJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ButlerSession{}, ErrNotFound
	}
	if err != nil {
		return ButlerSession{}, fmt.Errorf("runtimedb: reading butler session: %w", err)
	}
	s.ID = butlerSessionID(s.Seq)
	s.Mode = ButlerSessionMode(mode)
	s.Capped = capped != 0
	if s.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return ButlerSession{}, fmt.Errorf("runtimedb: parsing butler session created_at: %w", err)
	}
	return s, nil
}
