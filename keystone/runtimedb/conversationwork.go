package runtimedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var ErrConversationWorkHeld = errors.New("conversation work is held by its aggregate budget or unresolved outcome")

type ConversationWork struct {
	ID               int64  `json:"id"`
	ToolCalls        int    `json:"toolCalls"`
	ToolLimit        int    `json:"toolLimit"`
	UncertainOutcome bool   `json:"uncertainOutcome"`
	UncertainTask    string `json:"uncertainTask,omitempty"`
}

// ConversationWorkForTask keeps a fresh per-turn evidence ledger while
// exposing its parent's durable aggregate budget and uncertainty.
func (db *DB) ConversationWorkForTask(taskID string) (ConversationWork, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return ConversationWork{}, err
	}
	var work ConversationWork
	var uncertain int
	var uncertainTask sql.NullInt64
	err = db.sql.QueryRow(`SELECT w.seq,w.tool_calls,w.tool_limit,w.uncertain_outcome,w.uncertain_task
 FROM conversation_work w JOIN butler_tasks t ON t.conversation_work=w.seq WHERE t.seq=?`, seq).
		Scan(&work.ID, &work.ToolCalls, &work.ToolLimit, &uncertain, &uncertainTask)
	if err != nil {
		return work, fmt.Errorf("conversation work for %s: %w", taskID, err)
	}
	work.UncertainOutcome = uncertain != 0
	if uncertainTask.Valid {
		work.UncertainTask = butlerTaskID(uncertainTask.Int64)
	}
	return work, nil
}

// ReviewConversationWork records an explicit Owner decision. Resolving an
// ambiguous effect closes that turn as failed; it never retries the effect.
func (db *DB) ReviewConversationWork(taskID, actor, action, note string) error {
	if strings.TrimSpace(note) == "" {
		return ErrConversationWorkHeld
	}
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	person, err := activeOwnerForStop(tx, actor)
	if err != nil {
		return err
	}
	var work int64
	var uncertain int
	var uncertainTask sql.NullInt64
	if err := tx.QueryRow(`SELECT w.seq,w.uncertain_outcome,w.uncertain_task FROM conversation_work w
 JOIN butler_tasks t ON t.conversation_work=w.seq WHERE t.seq=?`, seq).Scan(&work, &uncertain, &uncertainTask); err != nil {
		return err
	}
	stamp := now()
	switch action {
	case "resolve-outcome":
		if uncertain != 1 || !uncertainTask.Valid {
			return ErrConversationWorkHeld
		}
		if err := transitionButlerTaskTx(tx, uncertainTask.Int64, ButlerTaskFailed, "Owner resolved uncertain outcome: "+note, actor); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE butler_task_waits SET resolved_at=?,resolved_by=? WHERE task=? AND resolved_at=''`, stamp, person, uncertainTask.Int64); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE conversation_work SET uncertain_outcome=0,uncertain_task=NULL,updated_at=? WHERE seq=?`, stamp, work); err != nil {
			return err
		}
	case "extend-budget":
		if _, err := tx.Exec(`UPDATE conversation_work SET tool_limit=tool_limit+64,updated_at=? WHERE seq=?`, stamp, work); err != nil {
			return err
		}
	default:
		return ErrConversationWorkHeld
	}
	if _, err := tx.Exec(`INSERT INTO conversation_work_reviews(work,actor_person_id,action,note,at) VALUES(?,?,?,?,?)`, work, person, action, note, stamp); err != nil {
		return err
	}
	return tx.Commit()
}
