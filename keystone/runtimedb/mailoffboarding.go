package runtimedb

// Offboarding a mailbox monitor's private state (epic #326 S08, #461;
// ADR-W030 A5.9).
//
// Revocation keeps a little: created candidates and message dedupe
// state stay, so a Person who turns their monitor back on is not asked
// about the same mail twice (PurgeMailConnection). Offboarding keeps
// nothing. The Person is gone, nobody may act on their candidates, and
// what the monitor learned from their mailbox — which messages it saw,
// what it proposed, where it had got to — is theirs and leaves with
// them. Their calendar events stay on the calendar: those are the
// calendar's, and removing them is not the mailbox's decision.
//
// It runs only AFTER the fence (OffboardMember steps 5 and 8 move the
// epoch, cancel work and revoke the authorization and credential), so
// nothing can be recording into the rows while they are deleted, and it
// is journalled, so a restore of an older snapshot deletes them again
// before anything can read them (ReconcileRestore).

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MailStateErased counts what EraseMailConnectionState removed.
type MailStateErased struct {
	Candidates  int `json:"candidates"`
	Reviews     int `json:"reviews"`
	Excerpts    int `json:"excerpts"`
	Effects     int `json:"effects"`
	Messages    int `json:"messages"`
	Checkpoints int `json:"checkpoints"`
	Answers     int `json:"answers"`
}

// Total is everything removed.
func (m MailStateErased) Total() int {
	return m.Candidates + m.Reviews + m.Excerpts + m.Effects + m.Messages + m.Checkpoints + m.Answers
}

// A connection whose mail state was erased is journalled as kind
// "connection", action "deleted" ("revoked" is the fence alone);
// ReconcileRestore erases it again.
const mailJournalKind = "connection"

// ErrMailConnectionLive refuses erasing the state of a connection that
// is still authorized: the fence comes first.
var ErrMailConnectionLive = errors.New("runtimedb: the mailbox connection is still authorized; fence it before erasing its state")

// EraseMailConnectionState deletes every candidate, revision, review
// and token, excerpt, effect record and audit row of one connection,
// its message dedupe state, the checkpoints of every subscription that
// reads it, and any mail answer still in the work queue. It refuses a
// connection whose background authorization is still live — the fence
// must already have happened — and journals what it did.
func (db *DB) EraseMailConnectionState(connection, actor, reason string, at time.Time) (MailStateErased, error) {
	var out MailStateErased
	if actor == "" || reason == "" {
		return out, fmt.Errorf("runtimedb: erasing a mailbox's state needs who and why")
	}
	if _, err := db.LiveBackgroundAuthorization(connection); err == nil {
		return out, ErrMailConnectionLive
	} else if !errors.Is(err, ErrNotFound) {
		return out, err
	}
	if err := db.eraseMailConnectionState(connection, &out); err != nil {
		return out, err
	}
	if err := db.journal(mailJournalKind, connection, "deleted", actor, BoundMailText(reason, 200)); err != nil {
		return out, err
	}
	return out, nil
}

// eraseMailConnectionState is the deletion itself, shared with restore
// replay (which needs no liveness check: replay has already re-fenced).
func (db *DB) eraseMailConnectionState(connection string, out *MailStateErased) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: erasing mailbox state: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var seq int64
	switch err := tx.QueryRow(`SELECT seq FROM integration_connections WHERE name = ?`, connection).Scan(&seq); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("runtimedb: erasing mailbox state: %w", err)
	}
	of := `(SELECT id FROM mail_candidates WHERE connection = ?)`
	steps := []struct {
		query string
		count *int
	}{
		{`DELETE FROM mail_review_tokens WHERE review IN (SELECT id FROM mail_reviews WHERE candidate IN ` + of + `)`, nil},
		{`DELETE FROM mail_reviews WHERE candidate IN ` + of, &out.Reviews},
		{`DELETE FROM mail_candidate_effects WHERE candidate IN ` + of, &out.Effects},
		{`DELETE FROM mail_candidate_excerpts WHERE candidate IN ` + of, &out.Excerpts},
		{`DELETE FROM mail_candidate_revisions WHERE candidate IN ` + of, nil},
		{`DELETE FROM mail_candidate_audit WHERE candidate IN ` + of, nil},
		{`DELETE FROM mail_candidates WHERE connection = ?`, &out.Candidates},
		{`DELETE FROM mail_monitor_messages WHERE connection = ?`, &out.Messages},
	}
	for _, s := range steps {
		res, err := tx.Exec(s.query, seq)
		if err != nil {
			return fmt.Errorf("runtimedb: erasing mailbox state: %w", err)
		}
		if s.count != nil {
			n, _ := res.RowsAffected()
			*s.count += int(n)
		}
	}
	// A checkpoint is where the monitor had got to in this mailbox. It is
	// keyed by the source it is for, so a later subscription on a
	// re-created connection of the same name starts from none.
	res, err := tx.Exec(`DELETE FROM agent_subscription_checkpoints WHERE source = ?`, connection)
	if err != nil {
		return fmt.Errorf("runtimedb: erasing mailbox checkpoints: %w", err)
	}
	n, _ := res.RowsAffected()
	out.Checkpoints += int(n)
	res, err = tx.Exec(`UPDATE integration_work SET result_json = '{}', request_json = '{}'
		WHERE connection = ? AND capability IN (?, ?, ?) AND (result_json <> '{}' OR request_json <> '{}')`,
		append([]any{seq}, mailCapabilities...)...)
	if err != nil {
		return fmt.Errorf("runtimedb: scrubbing mailbox answers: %w", err)
	}
	n, _ = res.RowsAffected()
	out.Answers += int(n)
	return tx.Commit()
}
