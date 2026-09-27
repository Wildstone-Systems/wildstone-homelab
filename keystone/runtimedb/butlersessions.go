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
	Seq       int64     `json:"seq"`
	ID        string    `json:"id"` // "bs-<seq>"
	CreatedAt time.Time `json:"createdAt"`
	Principal string    `json:"principal"`
	ActingFor string    `json:"actingFor"`
	// PersonID and AgentID are the structural identity of the
	// conversation (ADR-W033 B10). ActingFor and Butler remain the audit
	// labels a person reads; continuation never joins on those labels.
	PersonID string `json:"-"`
	AgentID  int64  `json:"-"`
	// InboundConnection is written in the same transaction as this
	// turn. Ref is zero for a built-in Connection.
	InboundConnection    string            `json:"-"`
	InboundConnectionRef int64             `json:"-"`
	Provider             string            `json:"provider"`
	Mode                 ButlerSessionMode `json:"mode"`
	Surface              ButlerSurface     `json:"surface"`
	// Butler names which specialist answered (ADR-W031 A1). Empty is a
	// real value: a session recorded before Butlers existed, or one the
	// engine phrased for itself, did not run as one. It is stamped from
	// the resolved Butler, never from the request — a caller that could
	// name it could file its conversation under somebody else's name.
	Butler         string `json:"butler,omitempty"`
	FinalText      string `json:"finalText"`
	Capped         bool   `json:"capped"`
	TranscriptJSON string `json:"transcriptJson,omitempty"`
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
	// TaskID is a trusted write-time handle, committed with this turn so a
	// later bookkeeping failure cannot orphan already-executed work.
	TaskID         string    `json:"-"`
	Ordinal        int       `json:"ordinal"`
	Role           string    `json:"role"` // "user" | "assistant"
	Text           string    `json:"text"`
	CreatedAt      time.Time `json:"createdAt"`
	Capped         bool      `json:"capped,omitempty"`
	TranscriptJSON string    `json:"transcriptJson,omitempty"`
	PlanIDs        []string  `json:"planIds,omitempty"`
	// Source is how a user turn arrived: TurnSourceVoice for words that
	// came from a recording, "" for typed text. A label for the
	// conversations page; it authorises nothing.
	Source string `json:"source,omitempty"`
}

// TurnSourceVoice marks a user turn transcribed from speech (ADR-W042).
const TurnSourceVoice = "voice"

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
	if s.Butler != "" {
		// Old audit/import callers can write labels that no longer resolve.
		// Preserve those records as history, but leave either structural
		// leg empty so they can never become continuation candidates. The
		// person-facing runtime validates both before it reaches this seam.
		if person, err := resolvePersonID(db.sql, s.ActingFor); err == nil {
			s.PersonID = person
		}
		if err := db.sql.QueryRow(`SELECT seq FROM butlers WHERE name = ?`, s.Butler).Scan(&s.AgentID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("runtimedb: resolving session agent: %w", err)
		}
	}
	if s.InboundConnection != "" && s.Butler != "" && (s.PersonID == "" || s.AgentID == 0) {
		return "", fmt.Errorf("runtimedb: an interactive session requires a canonical Person and Agent")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return "", fmt.Errorf("runtimedb: inserting butler session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var personArg, agentArg any
	if s.PersonID != "" {
		personArg = s.PersonID
	}
	if s.AgentID != 0 {
		agentArg = s.AgentID
	}
	res, err := tx.Exec(
		`INSERT INTO butler_sessions (created_at, principal, acting_for, provider, mode, surface, butler, final_text, capped, transcript_json, person_id, agent_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		now(), s.Principal, s.ActingFor, s.Provider, string(s.Mode), string(s.Surface), s.Butler, s.FinalText,
		boolInt(s.Capped), s.TranscriptJSON, personArg, agentArg)
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
	if err := insertSessionConnection(tx, seq, "inbound", s.InboundConnection, s.InboundConnectionRef); err != nil {
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
		if t.TaskID != "" {
			task, err := parseButlerTaskID(t.TaskID)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO session_task_sources(session,task) VALUES(?,?)`, session, task); err != nil {
				return err
			}
		}
		if t.Role != ButlerRoleUser && t.Role != ButlerRoleAssistant {
			return fmt.Errorf("runtimedb: butler turn role must be user or assistant, got %q", t.Role)
		}
		if t.TranscriptJSON == "" {
			t.TranscriptJSON = "{}"
		}
		if t.Source != "" && (t.Source != TurnSourceVoice || t.Role != ButlerRoleUser) {
			return fmt.Errorf("runtimedb: turn source %q is not valid on a %s turn", t.Source, t.Role)
		}
		// source is named only when there is one: a typed turn is the
		// column's default, and so is every turn written before 0090.
		cols, marks := "session, ordinal, role, text, created_at, capped, tool_json, plan_ids", "?, ?, ?, ?, ?, ?, ?, ?"
		args := []any{session, base + i, t.Role, t.Text, now(), boolInt(t.Capped), t.TranscriptJSON,
			strings.Join(t.PlanIDs, ",")}
		if t.Source != "" {
			cols, marks, args = cols+", source", marks+", ?", append(args, t.Source)
		}
		if _, err := tx.Exec(
			`INSERT INTO butler_session_turns (`+cols+`) VALUES (`+marks+`)`, args...,
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
	return db.AppendButlerTurnsFrom(id, turns, "", 0)
}

// AppendButlerTurnsFrom appends a turn and its inbound Connection atomically.
func (db *DB) AppendButlerTurnsFrom(id string, turns []ButlerTurn, connection string, connectionRef int64) error {
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
	if err := insertSessionConnection(tx, seq, "inbound", connection, connectionRef); err != nil {
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
	return listButlerTurns(db.sql, seq)
}

func listButlerTurns(q sessionReader, seq int64) ([]ButlerTurn, error) {
	rows, err := q.Query(
		`SELECT ordinal, role, text, created_at, capped, tool_json, plan_ids, source
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
		if err := rows.Scan(&t.Ordinal, &t.Role, &t.Text, &created, &capped, &t.TranscriptJSON, &planIDs, &t.Source); err != nil {
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

// ButlerSessionFilter narrows a session listing. Every field is
// optional, and an empty filter lists everything.
//
// A struct rather than three more string parameters, because the fields
// are all strings and a caller that transposes two of them gets a
// listing that is wrong rather than a compiler error.
type ButlerSessionFilter struct {
	// Surface is admin or family. Empty lists both (ADR-W026 A1).
	Surface ButlerSurface
	// Butler is the Butler whose sessions these are.
	Butler string
	// ActingFor is the person the session was held on behalf of.
	//
	// IT BELONGS HERE RATHER THAN IN THE CALLER'S LOOP, and the
	// household assistant is why. Its page lists one member's own
	// conversations, and it used to read a fixed number of the newest
	// family sessions and filter them afterwards — which is not a
	// filter at all in the household this was meant to protect. A
	// member whose partner has had the last two hundred conversations
	// reads two hundred of theirs, keeps none, and is shown an empty
	// list beneath a heading that says it is their history. Over-reading
	// only moves the number at which that happens.
	ActingFor string
}

// ListButlerSessions returns the newest limit sessions matching the
// filter, newest first, WITHOUT transcripts.
func (db *DB) ListButlerSessions(limit int, f ButlerSessionFilter) ([]ButlerSession, error) {
	if limit <= 0 {
		limit = 50
	}
	// The butler filter is an equality on the stamp, deliberately not a
	// fallback to the surface. A Butler with no sessions must come back
	// empty; widening to "every session on its surface" would answer a
	// question about one Butler with another Butler's conversations.
	clauses, args := []string{}, []any{}
	if f.Surface != "" {
		clauses = append(clauses, "surface = ?")
		args = append(args, string(f.Surface))
	}
	if f.Butler != "" {
		clauses = append(clauses, "butler = ?")
		args = append(args, f.Butler)
	}
	if f.ActingFor != "" {
		// NOCASE, because the household has one Sandy however she
		// signed in (ADR-W032 A4) and the door admits her either way:
		// the proxy asserting a different capitalisation would leave
		// her authenticated and her own conversations invisible. Every
		// other reader of a member name compares this way; a predicate
		// that did not would be the one place the household had two of
		// her.
		clauses = append(clauses, "acting_for = ? COLLATE NOCASE")
		args = append(args, f.ActingFor)
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	args = append(args, limit)
	rows, err := db.sql.Query(
		`SELECT seq, created_at, principal, acting_for, person_id, agent_id, provider, mode, surface, butler, final_text, capped
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
		var person sql.NullString
		var agent sql.NullInt64
		if err := rows.Scan(&s.Seq, &created, &s.Principal, &s.ActingFor, &person, &agent, &s.Provider, &mode, &surface, &s.Butler, &s.FinalText, &capped); err != nil {
			return nil, err
		}
		s.PersonID, s.AgentID = person.String, agent.Int64
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
	return getButlerSession(db.sql, id)
}

// sessionReader keeps all session reads on the caller's database snapshot.
type sessionReader interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func getButlerSession(q sessionReader, id string) (ButlerSession, error) {
	var seq int64
	if _, err := fmt.Sscanf(id, "bs-%d", &seq); err != nil {
		return ButlerSession{}, ErrNotFound
	}
	row := q.QueryRow(
		`SELECT seq, created_at, principal, acting_for, person_id, agent_id, provider, mode, surface, butler, final_text, capped, transcript_json
		 FROM butler_sessions WHERE seq = ?`, seq)
	var s ButlerSession
	var created, mode, surface string
	var capped int
	var person sql.NullString
	var agent sql.NullInt64
	err := row.Scan(&s.Seq, &created, &s.Principal, &s.ActingFor, &person, &agent, &s.Provider, &mode, &surface, &s.Butler, &s.FinalText, &capped, &s.TranscriptJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ButlerSession{}, ErrNotFound
	}
	if err != nil {
		return ButlerSession{}, fmt.Errorf("runtimedb: reading butler session: %w", err)
	}
	s.ID = butlerSessionID(s.Seq)
	s.PersonID, s.AgentID = person.String, agent.Int64
	s.Mode = ButlerSessionMode(mode)
	s.Surface = ButlerSurface(surface)
	s.Capped = capped != 0
	if s.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return ButlerSession{}, fmt.Errorf("runtimedb: parsing butler session created_at: %w", err)
	}
	if s.Turns, err = listButlerTurns(q, s.Seq); err != nil {
		return ButlerSession{}, err
	}
	return s, nil
}

// FirstUserTurns returns the opening thing the person said in each of
// the named sessions, keyed by session sequence number.
//
// IT EXISTS BECAUSE final_text IS THE ASSISTANT'S. AppendButlerTurns
// overwrites it with the model's reply on every assistant turn, so a
// list that titled its rows from it was naming each conversation after
// the answer rather than the question — and renaming it after every
// follow-up, so the same conversation had a different title each time a
// member came back to it.
//
// One query rather than one per session: a sidebar of two dozen rows is
// not a reason for two dozen round trips, and the transcript columns are
// left out because a title needs one line, not a conversation.
func (db *DB) FirstUserTurns(seqs []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(seqs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(seqs))
	args := make([]any, len(seqs))
	for i, seq := range seqs {
		placeholders[i] = "?"
		args[i] = seq
	}
	rows, err := db.sql.Query(
		`SELECT session, text FROM (
		   SELECT session, text, ROW_NUMBER() OVER (PARTITION BY session ORDER BY ordinal) AS rn
		     FROM butler_session_turns
		    WHERE role = ? AND session IN (`+strings.Join(placeholders, ",")+`)
		 ) WHERE rn = 1`,
		append([]any{ButlerRoleUser}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: reading opening turns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var text string
		if err := rows.Scan(&seq, &text); err != nil {
			return nil, err
		}
		out[seq] = text
	}
	return out, rows.Err()
}
