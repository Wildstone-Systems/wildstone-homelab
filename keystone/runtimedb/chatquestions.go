package runtimedb

// The chat question ledger (ADR-W026 A8, ADR-W024 H4).
//
// An ntfy notification can carry answer buttons, so its reply arrives
// already knowing which question it answers. A chat message cannot: the
// household types "yes" and the transport has no idea what that is
// about. This ledger is the difference — one row per question a rule
// asked over chat, and the next reply from that conversation closes it.
//
// A question that nobody answers expires rather than waiting forever: a
// "did you get the mail?" from Tuesday must not turn Thursday's "hello"
// into an answer.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ChatQuestionWindow is how long an unanswered chat question stays open.
// Long enough for somebody to see a notification and come back to it,
// short enough that an unrelated message tomorrow is not swallowed as an
// answer.
const ChatQuestionWindow = 4 * time.Hour

// ChatQuestion is one question a household rule asked over chat.
type ChatQuestion struct {
	ID            int64
	CorrelationID string
	Surface       ButlerSurface
	Transport     string
	Target        string
	Rule          string
	Subject       string
	AskedAt       time.Time
}

// RecordChatQuestion opens a question. A rule that asks the same
// conversation twice leaves two rows, and the newest is the one an
// answer closes — which is what a person means when they answer the
// message they can see.
func (db *DB) RecordChatQuestion(q ChatQuestion) (int64, error) {
	if q.CorrelationID == "" || q.Transport == "" || q.Target == "" {
		return 0, fmt.Errorf("runtimedb: a chat question needs a correlation id, transport and target")
	}
	if !ValidButlerSurface(q.Surface) {
		return 0, fmt.Errorf("runtimedb: chat question surface %q is not admin|family", q.Surface)
	}
	res, err := db.sql.Exec(
		`INSERT INTO chat_questions (correlation_id, surface, transport, target, rule, subject, asked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		q.CorrelationID, string(q.Surface), q.Transport, q.Target, q.Rule, q.Subject, now())
	if err != nil {
		return 0, fmt.Errorf("runtimedb: recording chat question: %w", err)
	}
	return res.LastInsertId()
}

// ClaimChatAnswer closes the newest open question for a conversation and
// returns it, or ErrNotFound when there is none within the window.
//
// Claiming is a single transaction that marks the row answered, so two
// messages arriving together produce one answer rather than two — the
// same at-most-once shape the automation engine's dispatch claim has.
func (db *DB) ClaimChatAnswer(transport, target string, window time.Duration) (ChatQuestion, error) {
	if window <= 0 {
		window = ChatQuestionWindow
	}
	cutoff := time.Now().Add(-window).UTC().Format(time.RFC3339Nano)
	tx, err := db.sql.Begin()
	if err != nil {
		return ChatQuestion{}, err
	}
	defer tx.Rollback() //nolint:errcheck // rolled back unless committed

	var q ChatQuestion
	var surface, asked string
	err = tx.QueryRow(
		`SELECT id, correlation_id, surface, transport, target, rule, subject, asked_at
		 FROM chat_questions
		 WHERE transport = ? AND target = ? AND answered_at = '' AND asked_at >= ?
		 ORDER BY id DESC LIMIT 1`, transport, target, cutoff).
		Scan(&q.ID, &q.CorrelationID, &surface, &q.Transport, &q.Target, &q.Rule, &q.Subject, &asked)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatQuestion{}, ErrNotFound
	}
	if err != nil {
		return ChatQuestion{}, fmt.Errorf("runtimedb: reading chat question: %w", err)
	}
	res, err := tx.Exec(`UPDATE chat_questions SET answered_at = ? WHERE id = ? AND answered_at = ''`, now(), q.ID)
	if err != nil {
		return ChatQuestion{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ChatQuestion{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return ChatQuestion{}, err
	}
	q.Surface = ButlerSurface(surface)
	if q.AskedAt, err = time.Parse(time.RFC3339Nano, asked); err != nil {
		return ChatQuestion{}, err
	}
	return q, nil
}
