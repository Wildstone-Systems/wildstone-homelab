package runtimedb

// Durable upstream accounting for ADR-W047. These are trusted platform store
// methods, never runtime/model inputs. The coordinator supplies current policy.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrResearchBudgetExhausted = errors.New("budget_exhausted: research progress saved; ask for explicit continuation with higher search/fetch totals")
	ErrResearchConflict        = errors.New("research budget or work changed; reload before continuing")
)

type ResearchLimits struct{ Searches, Fetches int }

func (l ResearchLimits) valid() bool {
	return l.Searches > 0 && l.Searches <= 100 && l.Fetches > 0 && l.Fetches <= 200
}
func DefaultResearchLimits() ResearchLimits { return ResearchLimits{10, 20} }

type ResearchBudget struct {
	SearchesUsed int
	FetchesUsed  int
	Allowed      ResearchLimits
	Revision     int64
}
type ResearchCheckpoint struct {
	Seq int64
	// Only verified steps in this task may be referenced. Their existing results,
	// receipts and source provenance remain authoritative and access controlled.
	Steps      []int64
	NextAction string
}
type ResearchAttempt struct {
	Seq           int64
	LedgerAttempt int64
	Kind          string
	State         string
}

func researchBudgetTx(tx *sql.Tx, task int64) (b ResearchBudget, err error) {
	err = tx.QueryRow(`SELECT searches_used,fetches_used,searches_allowed,fetches_allowed,revision FROM research_budgets WHERE task=?`, task).Scan(&b.SearchesUsed, &b.FetchesUsed, &b.Allowed.Searches, &b.Allowed.Fetches, &b.Revision)
	return
}

// Check again after taking SQLite's writer lock: a promotion, stop or restore
// can commit while admission is waiting for that lock. All three facts must
// describe the same snapshot as the reservation/checkpoint/continuation.
func (db *DB) researchFenceTx(tx *sql.Tx) error {
	lease := db.executionLease.Load()
	if lease == nil {
		return ErrCoordinatorFenced
	}
	var holder string
	var epoch int64
	var quarantine, stopped bool
	err := tx.QueryRow(`SELECT l.holder,l.epoch,COALESCE((SELECT quarantined FROM restore_state WHERE id=1),0),h.active
 FROM coordinator_lease l CROSS JOIN household_execution_stop h
 WHERE l.id=1 AND h.singleton=1`).Scan(&holder, &epoch, &quarantine, &stopped)
	if err != nil {
		return err
	}
	if holder != lease.Holder || epoch != lease.Epoch {
		return ErrCoordinatorFenced
	}
	if quarantine {
		return ErrQuarantined
	}
	if stopped {
		return ErrHouseholdStopped
	}
	return nil
}

// ReserveResearchAttempt commits one spend before I/O. Every retry calls this
// again. A reservation is never refunded or reused to dispatch: a crash between
// commit and I/O is conservatively spent. Config can narrow existing totals but
// cannot raise them here. Continuation is the only path that raises totals.
func (db *DB) ReserveResearchAttempt(ctx context.Context, taskID string, ledgerAttempt int64, kind string, limits ResearchLimits) (int64, ResearchBudget, error) {
	var empty ResearchBudget
	if !limits.valid() || (kind != "search" && kind != "fetch") {
		return 0, empty, ErrResearchConflict
	}
	if err := ctx.Err(); err != nil {
		return 0, empty, err
	}
	if err := db.CheckHouseholdExecution(); err != nil {
		return 0, empty, err
	}
	task, err := parseButlerTaskID(taskID)
	if err != nil {
		return 0, empty, err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, empty, err
	}
	defer tx.Rollback()
	if err = db.researchFenceTx(tx); err != nil {
		return 0, empty, err
	}
	var state string
	var epoch, attemptEpoch int64
	var attemptState, stepKind string
	err = tx.QueryRow(`SELECT t.state,t.lease_epoch,a.epoch,a.state,s.kind FROM butler_tasks t
 JOIN butler_task_attempts a ON a.task=t.seq JOIN butler_task_steps s ON s.seq=a.step
 WHERE t.seq=? AND a.seq=?`, task, ledgerAttempt).Scan(&state, &epoch, &attemptEpoch, &attemptState, &stepKind)
	if err != nil {
		return 0, empty, err
	}
	if epoch != attemptEpoch || attemptState != "dispatched" || stepKind != "tool" {
		return 0, empty, ErrAttemptFenced
	}
	if state != "running" && state != "waiting" {
		return 0, empty, ErrResearchConflict
	}
	if state == "waiting" {
		var ownWait int
		if err = tx.QueryRow(`SELECT count(*) FROM research_budgets b JOIN butler_task_waits w ON w.seq=b.wait_seq
  WHERE b.task=? AND w.task=b.task AND w.reason='budget-exhausted' AND w.resolved_at=''`, task).Scan(&ownWait); err != nil {
			return 0, empty, err
		}
		if ownWait != 1 {
			return 0, empty, ErrResearchConflict
		}
	}
	if _, err = tx.Exec(`INSERT INTO research_budgets(task,searches_allowed,fetches_allowed,config_searches,config_fetches) VALUES(?,?,?,?,?) ON CONFLICT(task) DO NOTHING`, task, limits.Searches, limits.Fetches, limits.Searches, limits.Fetches); err != nil {
		return 0, empty, err
	}
	// Lowered configuration persists; switching providers or raising config cannot
	// silently restore a larger limit, even after restart.
	if _, err = tx.Exec(`UPDATE research_budgets SET revision=revision+CASE WHEN config_searches<>? OR config_fetches<>? THEN 1 ELSE 0 END,
 searches_allowed=CASE WHEN ?<config_searches THEN MIN(searches_allowed,?) ELSE searches_allowed END,
 fetches_allowed=CASE WHEN ?<config_fetches THEN MIN(fetches_allowed,?) ELSE fetches_allowed END,
 config_searches=?,config_fetches=? WHERE task=?`, limits.Searches, limits.Fetches, limits.Searches, limits.Searches, limits.Fetches, limits.Fetches, limits.Searches, limits.Fetches, task); err != nil {
		return 0, empty, err
	}
	b, err := researchBudgetTx(tx, task)
	if err != nil {
		return 0, empty, err
	}
	exhausted := b.SearchesUsed > b.Allowed.Searches || b.FetchesUsed > b.Allowed.Fetches ||
		kind == "search" && b.SearchesUsed >= b.Allowed.Searches || kind == "fetch" && b.FetchesUsed >= b.Allowed.Fetches
	if exhausted {
		if state == "running" {
			if err = transitionButlerTaskTx(tx, task, ButlerTaskWaiting, "research budget exhausted", ""); err != nil {
				return 0, b, err
			}
			detail := fmt.Sprintf("research budget: %d/%d searches; %d/%d fetches. Verified steps and checkpoints are saved. Ask for explicit continuation with higher totals; tool/turn limits still apply.", b.SearchesUsed, b.Allowed.Searches, b.FetchesUsed, b.Allowed.Fetches)
			res, e := tx.Exec(`INSERT INTO butler_task_waits(task,created_at,reason,detail,resume_at) VALUES(?,?,?,?, '')`, task, now(), ButlerWaitBudgetExhausted, detail)
			if e != nil {
				return 0, b, e
			}
			wait, e := res.LastInsertId()
			if e != nil {
				return 0, b, e
			}
			if _, err = tx.Exec(`UPDATE research_budgets SET wait_seq=? WHERE task=?`, wait, task); err != nil {
				return 0, b, err
			}
			if _, err = tx.Exec(`UPDATE butler_tasks SET next_wakeup_at='' WHERE seq=?`, task); err != nil {
				return 0, b, err
			}
		}
		if err = tx.Commit(); err != nil {
			return 0, b, err
		}
		return 0, b, ErrResearchBudgetExhausted
	}
	if state != "running" {
		return 0, b, ErrResearchConflict
	}
	column := "searches_used"
	if kind == "fetch" {
		column = "fetches_used"
	}
	if _, err = tx.Exec(`UPDATE research_budgets SET `+column+`=`+column+`+1 WHERE task=?`, task); err != nil {
		return 0, b, err
	}
	res, err := tx.Exec(`INSERT INTO research_attempts(task,ledger_attempt,kind,created_at) VALUES(?,?,?,?)`, task, ledgerAttempt, kind, now())
	if err != nil {
		return 0, b, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, b, err
	}
	b, err = researchBudgetTx(tx, task)
	if err != nil {
		return 0, b, err
	}
	if err = tx.Commit(); err != nil {
		return 0, b, err
	}
	return seq, b, nil
}

// FinishResearchAttempt records only accounting status, never publishes a tool
// result. It may settle cancellation after task fencing; results still pass the
// existing ledger's epoch fence. Unknown/crashed reservations remain spent.
func (db *DB) FinishResearchAttempt(taskID string, seq int64, state string) error {
	if state != "succeeded" && state != "failed" && state != "cancelled" {
		return ErrResearchConflict
	}
	task, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	res, err := db.sql.Exec(`UPDATE research_attempts SET state=?,ended_at=? WHERE seq=? AND task=? AND state='reserved'`, state, now(), seq, task)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrResearchConflict
	}
	return nil
}

func (db *DB) GetResearchBudget(taskID string) (ResearchBudget, error) {
	task, err := parseButlerTaskID(taskID)
	if err != nil {
		return ResearchBudget{}, err
	}
	var b ResearchBudget
	err = db.sql.QueryRow(`SELECT searches_used,fetches_used,searches_allowed,fetches_allowed,revision FROM research_budgets WHERE task=?`, task).Scan(&b.SearchesUsed, &b.FetchesUsed, &b.Allowed.Searches, &b.Allowed.Fetches, &b.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return b, err
}

func (db *DB) ListResearchAttempts(taskID string) ([]ResearchAttempt, error) {
	task, err := parseButlerTaskID(taskID)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(`SELECT seq,ledger_attempt,kind,state FROM research_attempts WHERE task=? ORDER BY seq`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResearchAttempt{}
	for rows.Next() {
		var a ResearchAttempt
		if err := rows.Scan(&a.Seq, &a.LedgerAttempt, &a.Kind, &a.State); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (db *DB) SaveResearchCheckpoint(taskID string, epoch int64, c ResearchCheckpoint) error {
	if len(c.Steps) > 100 || (c.NextAction != "search" && c.NextAction != "fetch" && c.NextAction != "read" && c.NextAction != "report") {
		return ErrResearchConflict
	}
	if err := db.CheckHouseholdExecution(); err != nil {
		return err
	}
	task, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = db.researchFenceTx(tx); err != nil {
		return err
	}
	var valid int
	if err = tx.QueryRow(`SELECT count(*) FROM butler_tasks WHERE seq=? AND lease_epoch=? AND state IN ('running','waiting')`, task, epoch).Scan(&valid); err != nil {
		return err
	}
	if valid != 1 {
		return ErrAttemptFenced
	}
	seen := map[int64]bool{}
	for _, step := range c.Steps {
		if seen[step] {
			return ErrResearchConflict
		}
		seen[step] = true
		if err = tx.QueryRow(`SELECT count(*) FROM butler_task_steps WHERE seq=? AND task=? AND state='verified'`, step, task).Scan(&valid); err != nil {
			return err
		}
		if valid != 1 {
			return ErrResearchConflict
		}
	}
	data, err := json.Marshal(c.Steps)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO research_checkpoints(task,created_at,steps_json,next_action) VALUES(?,?,?,?)`, task, now(), string(data), c.NextAction); err != nil {
		return err
	}
	return tx.Commit()
}
func (db *DB) LatestResearchCheckpoint(taskID string) (ResearchCheckpoint, error) {
	var c ResearchCheckpoint
	task, err := parseButlerTaskID(taskID)
	if err != nil {
		return c, err
	}
	var data string
	err = db.sql.QueryRow(`SELECT seq,steps_json,next_action FROM research_checkpoints WHERE task=? ORDER BY seq DESC LIMIT 1`, task).Scan(&c.Seq, &data, &c.NextAction)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal([]byte(data), &c.Steps)
	return c, err
}

// ContinueResearch is a trusted human-policy write, not a runtime capability.
// The caller must authorize the actor and exact totals against current policy.
// CAS and absolute totals make duplicate approvals harmless; history is kept.
func (db *DB) ContinueResearch(taskID, actor string, expected int64, limits ResearchLimits) error {
	if !limits.valid() || strings.TrimSpace(actor) == "" || len(actor) > 128 {
		return ErrResearchConflict
	}
	if err := db.CheckHouseholdExecution(); err != nil {
		return err
	}
	task, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = db.researchFenceTx(tx); err != nil {
		return err
	}
	b, err := researchBudgetTx(tx, task)
	if err != nil {
		return err
	}
	if b.Revision != expected || limits.Searches < b.Allowed.Searches || limits.Fetches < b.Allowed.Fetches || limits == b.Allowed || limits.Searches <= b.SearchesUsed || limits.Fetches <= b.FetchesUsed {
		return ErrResearchConflict
	}
	var wait int64
	err = tx.QueryRow(`SELECT w.seq FROM research_budgets b JOIN butler_tasks t ON t.seq=b.task JOIN butler_task_waits w ON w.seq=b.wait_seq
 WHERE b.task=? AND t.state='waiting' AND w.task=t.seq AND w.reason='budget-exhausted' AND w.resolved_at=''`, task).Scan(&wait)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrResearchConflict
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO research_continuations(task,revision,actor,created_at,searches_allowed,fetches_allowed) VALUES(?,?,?,?,?,?)`, task, expected+1, actor, now(), limits.Searches, limits.Fetches); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE research_budgets SET searches_allowed=?,fetches_allowed=?,revision=revision+1,wait_seq=NULL WHERE task=?`, limits.Searches, limits.Fetches, task); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE butler_task_waits SET resolved_at=?,resolved_by=? WHERE seq=?`, now(), actor, wait); err != nil {
		return err
	}
	if err = transitionButlerTaskTx(tx, task, ButlerTaskRunning, "authorized research continuation", actor); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE butler_tasks SET next_wakeup_at='' WHERE seq=?`, task); err != nil {
		return err
	}
	return tx.Commit()
}

// IsResearchBudgetWait recognizes only the current wait owned by accounting.
// Runtime Work.Wait acknowledgements cannot create, replace or resolve it.
func (db *DB) IsResearchBudgetWait(taskID string) (bool, error) {
	task, err := parseButlerTaskID(taskID)
	if err != nil {
		return false, err
	}
	var n int
	err = db.sql.QueryRow(`SELECT count(*) FROM research_budgets b
 JOIN butler_tasks t ON t.seq=b.task JOIN butler_task_waits w ON w.seq=b.wait_seq
 WHERE b.task=? AND t.state='waiting' AND w.task=t.seq AND w.reason='budget-exhausted' AND w.resolved_at=''`, task).Scan(&n)
	return n == 1, err
}
