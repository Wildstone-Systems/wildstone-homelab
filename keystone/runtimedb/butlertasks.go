package runtimedb

// Durable Butler work (ADR-W030 A4, work package P1).
//
// A task is one unit of work the Butler owes someone: a conversational
// request, or one occurrence of a Wildstone-owned routine. Both run on
// the same machinery, and both survive a restart, because the ledger is
// here rather than in the process.
//
// The rules this file exists to enforce, all of them from A4:
//
//   - A tool call is checkpointed BEFORE dispatch and again after its
//     result. StartButlerAttempt / FinishButlerAttempt are that pair.
//   - A result from a superseded attempt is rejected, not applied. Every
//     attempt carries the task's lease epoch at dispatch; finishing with
//     a stale one fails.
//   - Completion requires evidence. CompleteButlerTask writes the
//     evidence and the state change in one transaction, and the schema
//     refuses the transition if there is none — so a crash between the
//     two leaves work to finish, never a lie about finished work.
//   - Authority metadata is mandatory. A task with no owner, surface,
//     class or audience cannot be permission-filtered later, so it is
//     refused at write time.
//
// This file stores work; it does not run it. The runner refactor is P3.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// TaskEnvelopeVersion is the schema version stamped on every serialized
// payload written by this package. It is stored per row rather than
// inferred from the migration, so a later package can change a payload
// shape and still read what an older Keystone wrote (ADR-W030 A5.1).
const TaskEnvelopeVersion = 1

// ButlerTaskKind separates a conversational task from a routine run.
// They share execution machinery; the routine's DEFINITION lives
// elsewhere and each occurrence is its own task (ADR-W030 A4).
type ObjectiveKind string

const (
	ObjectiveInformational ObjectiveKind = "informational"
	ObjectiveEffectful     ObjectiveKind = "effectful"
)

type ButlerTaskKind string

const (
	ButlerTaskConversation ButlerTaskKind = "conversation"
	ButlerTaskRoutine      ButlerTaskKind = "routine"
)

// ButlerTaskState is the lifecycle of one task.
type ButlerTaskState string

const (
	ButlerTaskPending   ButlerTaskState = "pending"
	ButlerTaskRunning   ButlerTaskState = "running"
	ButlerTaskWaiting   ButlerTaskState = "waiting"
	ButlerTaskCompleted ButlerTaskState = "completed"
	ButlerTaskFailed    ButlerTaskState = "failed"
	ButlerTaskCancelled ButlerTaskState = "cancelled"
	// ButlerTaskPausedOffboarded is where an offboarded member's
	// unfinished personal work goes: no retries, no notices, no model
	// calls, and no deletion of what it already recorded (A4).
	ButlerTaskPausedOffboarded ButlerTaskState = "paused-offboarded"
)

// butlerTaskTransitions is the closed state machine. A transition absent
// here is a bug in the caller, refused rather than recorded — the same
// treatment plans get.
//
// paused-offboarded can resume because a SHARED task transfers to the
// administrator and resumes after grant review; a personal one simply is
// never resumed by policy above this layer (A4).
var butlerTaskTransitions = map[ButlerTaskState][]ButlerTaskState{
	ButlerTaskPending: {ButlerTaskRunning, ButlerTaskCancelled, ButlerTaskPausedOffboarded},
	ButlerTaskRunning: {ButlerTaskWaiting, ButlerTaskCompleted, ButlerTaskFailed,
		ButlerTaskCancelled, ButlerTaskPausedOffboarded},
	ButlerTaskWaiting: {ButlerTaskRunning, ButlerTaskFailed, ButlerTaskCancelled,
		ButlerTaskPausedOffboarded},
	ButlerTaskPausedOffboarded: {ButlerTaskRunning, ButlerTaskCancelled},
}

// ButlerTaskAudience is the disclosure label every task, evidence row
// and trace event carries. Shared history does not mean shared
// disclosure (ADR-W030 A2), and a row whose audience is unknown can
// never be safely shown, so it is never written.
type ButlerTaskAudience string

// The two audiences every record had before slice 7, now expressed as
// rows in `audiences` rather than as words.
//
// The values are audience ids; the names are kept because these are the
// same two concepts the design names — "the owning Person alone" and
// "the household" — and renaming 130 call sites in the slice that
// changes what they MEAN would bury the change that matters in a
// rename. An audience naming a group is not one of these: it is a row
// somebody created, reached by id.
const (
	ButlerAudiencePrivate ButlerTaskAudience = "aud_owner"
	ButlerAudienceShared  ButlerTaskAudience = "aud_household"
)

// ButlerTaskRetention is the configured lifetime of the task record.
// The default across the product is manual deletion (ADR-W030 A10);
// "conversation" follows the originating conversation's policy.
type ButlerTaskRetention string

const (
	ButlerRetentionManual       ButlerTaskRetention = "manual"
	ButlerRetentionConversation ButlerTaskRetention = "conversation"
	ButlerRetentionEphemeral    ButlerTaskRetention = "ephemeral"
)

// ButlerTask is one durable unit of work.
//
// Author never changes; Owner does, when offboarding transfers a shared
// task to the administrator. Surface and AgentClass are snapshotted at
// admission: a resume rechecks authority against what the task claimed,
// which is only meaningful if the claim was written down.
type ButlerTask struct {
	ProposalOnly bool            `json:"proposalOnly,omitempty"`
	Seq          int64           `json:"seq"`
	ID           string          `json:"id"` // "bt-<seq>"
	CreatedAt    time.Time       `json:"createdAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
	Kind         ButlerTaskKind  `json:"kind"`
	State        ButlerTaskState `json:"state"`

	Author string `json:"author"`
	Owner  string `json:"owner"`

	Surface    ButlerSurface      `json:"surface"`
	AgentClass string             `json:"agentClass"`
	Audience   ButlerTaskAudience `json:"audience"`

	// Session and OriginTurn point at the existing conversation store.
	// Nothing in butler_sessions is rewritten to carry a task id: the
	// link is one-way, so a conversation's attribution stays exactly what
	// it was when it was written (ADR-W030 P1).
	SessionID  string `json:"sessionId,omitempty"` // "bs-<seq>", empty for routines
	OriginTurn *int   `json:"originTurn,omitempty"`

	// OriginConnection is the Connection the request that created this
	// task arrived over (design §10.1, migration 0062).
	//
	// It is a POINTER at that request, not a grant: nothing is issued,
	// held or revoked here, and the value can only ever narrow what the
	// task may do. Empty means the leg does not apply — a routine runs
	// unattended and borrows nobody's Connection (§10.2).
	OriginConnection string `json:"originConnection,omitempty"`
	// OriginConnectionRef is the Connection ROW it arrived over, which
	// an address alone cannot identify: addresses are reusable, and a
	// re-added bot would otherwise resurrect a withdrawn Connection's
	// work. Zero means the leg does not apply — see migration 0062.
	OriginConnectionRef int64 `json:"originConnectionRef,omitempty"`

	// CorrelationID ties a routine run to the W024 household timeline.
	// The timeline stays authoritative for deterministic outcomes;
	// anything mirrored onto the task is a projection (A4).
	CorrelationID string `json:"correlationId,omitempty"`

	Objective          string        `json:"objective"`
	ObjectiveKind      ObjectiveKind `json:"objectiveKind"`
	CompletionCriteria string        `json:"completionCriteria"`

	GrantsJSON   string    `json:"grantsJson,omitempty"`
	BudgetJSON   string    `json:"budgetJson,omitempty"`
	DeadlineAt   time.Time `json:"deadlineAt,omitzero"`
	NextWakeupAt time.Time `json:"nextWakeupAt,omitzero"`

	LeaseEpoch int64 `json:"leaseEpoch"`

	// Uncertainty is set when an action's outcome could not be
	// established. A resume reads it and pauses rather than blindly
	// retrying an effect that may already have happened (A4).
	Uncertainty   string `json:"uncertainty,omitempty"`
	ResultSummary string `json:"resultSummary,omitempty"`

	Retention       ButlerTaskRetention `json:"retention"`
	EnvelopeVersion int                 `json:"envelopeVersion"`

	// AsOwner and SystemExtra are the request-time facts a task handle
	// alone cannot recover (migration 0083, ADR-W039 Phase 2b, #337):
	// what the CALLER was when the turn that opened this task was
	// admitted, not a decision the runtime makes or a fact about the
	// Butler. They mirror butlerlive.Turn.AsOwner/SystemExtra exactly,
	// and keystone/agentgateway reads them back to rebuild that Turn
	// for a handle-resolved call.
	AsOwner     bool   `json:"asOwner,omitempty"`
	SystemExtra string `json:"systemExtra,omitempty"`
}

func butlerTaskID(seq int64) string { return fmt.Sprintf("bt-%d", seq) }

// parseButlerTaskID inverts butlerTaskID; it rejects anything else.
func parseButlerTaskID(id string) (int64, error) {
	var seq int64
	if _, err := fmt.Sscanf(id, "bt-%d", &seq); err != nil || butlerTaskID(seq) != id {
		return 0, fmt.Errorf("runtimedb: malformed butler task id %q", id)
	}
	return seq, nil
}

// InsertButlerTask records a new task and returns its "bt-<seq>" id.
//
// It refuses a task missing any of the fields a later permission check
// needs — owner, author, surface, class, audience, objective — because
// the alternative is a row that cannot be filtered, shown, or resumed.
// State is always pending: a task that begins life finished is the
// partial-write failure this schema exists to prevent.
func (db *DB) InsertButlerTask(t ButlerTask) (string, error) {
	// Only a trusted caller's explicit informational classification lowers the
	// evidence requirement. Missing or unknown classifications fail safely.
	if t.ObjectiveKind != ObjectiveInformational {
		t.ObjectiveKind = ObjectiveEffectful
	}
	if t.Kind != ButlerTaskConversation && t.Kind != ButlerTaskRoutine {
		return "", fmt.Errorf("runtimedb: butler task kind must be conversation or routine, got %q", t.Kind)
	}
	if t.Author == "" || t.Owner == "" {
		return "", fmt.Errorf("runtimedb: butler task requires author and owner (ADR-W030 A4)")
	}
	if !ValidButlerSurface(t.Surface) {
		return "", fmt.Errorf("runtimedb: butler task requires a valid surface (admin|family), got %q (ADR-W026 A1)", t.Surface)
	}
	if t.AgentClass == "" {
		return "", fmt.Errorf("runtimedb: butler task requires an agent class (ADR-W030 A1, design §5.2)")
	}
	// See validateMemory: named only, with the foreign key as backstop.
	if t.Audience == "" {
		return "", fmt.Errorf("runtimedb: butler task requires an audience — who it may be disclosed to (ADR-W030 A2, design §10 layer 3)")
	}
	if t.Objective == "" {
		return "", fmt.Errorf("runtimedb: butler task requires an objective (ADR-W030 A4)")
	}
	if t.Retention == "" {
		t.Retention = ButlerRetentionManual
	}
	switch t.Retention {
	case ButlerRetentionManual, ButlerRetentionConversation, ButlerRetentionEphemeral:
	default:
		return "", fmt.Errorf("runtimedb: butler task retention must be manual|conversation|ephemeral, got %q (ADR-W030 A10)", t.Retention)
	}
	grants, err := jsonObjectOrEmpty(t.GrantsJSON, "grants")
	if err != nil {
		return "", err
	}
	budget, err := jsonObjectOrEmpty(t.BudgetJSON, "budget")
	if err != nil {
		return "", err
	}
	var session any
	if t.SessionID != "" {
		var seq int64
		if _, err := fmt.Sscanf(t.SessionID, "bs-%d", &seq); err != nil {
			return "", fmt.Errorf("runtimedb: malformed butler session id %q", t.SessionID)
		}
		session = seq
	}
	var originTurn any
	if t.OriginTurn != nil {
		originTurn = *t.OriginTurn
	}

	stamp := now()
	tx, err := db.sql.Begin()
	if err != nil {
		return "", fmt.Errorf("runtimedb: inserting butler task: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Author and owner are people, so they are stored as identities.
	// Resolved inside this transaction: a rename between the resolve and
	// the insert would otherwise attach the task to the wrong snapshot
	// of who exists.
	author, err := resolvePersonID(tx, t.Author)
	if err != nil {
		return "", fmt.Errorf("runtimedb: inserting butler task: the author %w", err)
	}
	owner, err := resolvePersonID(tx, t.Owner)
	if err != nil {
		return "", fmt.Errorf("runtimedb: inserting butler task: the owner %w", err)
	}
	res, err := tx.Exec(
		`INSERT INTO butler_tasks (created_at, updated_at, kind, state, author_person_id, owner_person_id,
		     surface, agent_class, audience, session, origin_turn, origin_connection,
		     origin_connection_ref, correlation_id, objective,
		     completion_criteria, grants_json, budget_json, deadline_at, next_wakeup_at,
		     lease_epoch, uncertainty, result_summary, retention, envelope_version, objective_kind, proposal_only,
		     as_owner, system_extra)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', '', ?, ?, ?, ?, ?, ?)`,
		stamp, stamp, string(t.Kind), string(ButlerTaskPending), author, owner,
		string(t.Surface), t.AgentClass, string(t.Audience), session, originTurn,
		t.OriginConnection, t.OriginConnectionRef,
		t.CorrelationID, t.Objective, t.CompletionCriteria, grants, budget,
		timeText(t.DeadlineAt), timeText(t.NextWakeupAt),
		string(t.Retention), TaskEnvelopeVersion, t.ObjectiveKind, t.ProposalOnly,
		t.AsOwner, t.SystemExtra)
	if err != nil {
		return "", fmt.Errorf("runtimedb: inserting butler task: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return "", err
	}
	if t.Kind == ButlerTaskConversation {
		var parent int64
		if session != nil {
			err = tx.QueryRow(`SELECT seq FROM conversation_work WHERE session=? AND owner_person_id=?`, session, owner).Scan(&parent)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return "", err
			}
		}
		if parent == 0 {
			used := 0
			if session != nil {
				if err := tx.QueryRow(`SELECT count(*) FROM butler_task_steps s JOIN butler_tasks t ON t.seq=s.task
				 WHERE s.kind='tool' AND (t.session=? OR EXISTS (SELECT 1 FROM session_task_sources l WHERE l.session=? AND l.task=t.seq))`, session, session).Scan(&used); err != nil {
					return "", err
				}
			}
			created, err := tx.Exec(`INSERT INTO conversation_work(session,owner_person_id,tool_calls,created_at,updated_at) VALUES(?,?,?,?,?)`, session, owner, used, stamp, stamp)
			if err != nil {
				return "", err
			}
			parent, err = created.LastInsertId()
			if err != nil {
				return "", err
			}
		}
		if _, err := tx.Exec("UPDATE butler_tasks SET conversation_work=? WHERE seq=?", parent, seq); err != nil {
			return "", err
		}
	}
	// The opening transition is written with the row so the history is
	// complete from the first state, not from the first change.
	if _, err := tx.Exec(
		`INSERT INTO butler_task_transitions (task, at, from_state, to_state, reason, actor)
		 VALUES (?, ?, '', ?, 'created', ?)`,
		seq, stamp, string(ButlerTaskPending), t.Author); err != nil {
		return "", fmt.Errorf("runtimedb: inserting butler task: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("runtimedb: inserting butler task: %w", err)
	}
	return butlerTaskID(seq), nil
}

// GetButlerTask returns one task by id.
func (db *DB) GetButlerTask(id string) (ButlerTask, error) {
	seq, err := parseButlerTaskID(id)
	if err != nil {
		return ButlerTask{}, err
	}
	return scanButlerTask(db.sql.QueryRow(butlerTaskColumns+` WHERE t.seq = ?`, seq))
}

// SetButlerTaskSession points a task at the conversation it belongs to.
//
// It exists because of an ordering that cannot be avoided: a session id
// is only known once a turn has been recorded, and a turn cannot be
// recorded until the work has run — but the work needs a task before it
// starts. So the task is opened first and linked afterwards.
//
// The link is written ONCE and never moved. Re-pointing a task at a
// different conversation would let one thread inherit another's step
// ledger, and a resume would then serve tool results gathered for a
// different question. A task that already names a session keeps it.
func (db *DB) SetButlerTaskSession(taskID, sessionID string) error {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	var sessionSeq int64
	if _, err := fmt.Sscanf(sessionID, "bs-%d", &sessionSeq); err != nil {
		return fmt.Errorf("runtimedb: malformed butler session id %q", sessionID)
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`UPDATE butler_tasks SET session = ?, updated_at = ?
		 WHERE seq = ? AND session IS NULL`, sessionSeq, now(), seq)
	if err != nil {
		return fmt.Errorf("runtimedb: linking a butler task to its session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either the task is gone, or it is already linked. Both are
		// answered by reading it back: an idempotent re-link of the same
		// session is not an error, and a DIFFERENT one must be.
		var existing sql.NullInt64
		if err := tx.QueryRow("SELECT session FROM butler_tasks WHERE seq=?", seq).Scan(&existing); err != nil {
			return err
		}
		if !existing.Valid || existing.Int64 != sessionSeq {
			return fmt.Errorf("runtimedb: task %s already belongs to conversation %v (ADR-W031 A4)",
				taskID, existing)
		}
	}
	var current sql.NullInt64
	if err := tx.QueryRow("SELECT conversation_work FROM butler_tasks WHERE seq=?", seq).Scan(&current); err != nil {
		return err
	}
	if current.Valid {
		var existing int64
		err := tx.QueryRow(`SELECT seq FROM conversation_work WHERE session=? AND owner_person_id=(SELECT owner_person_id FROM butler_tasks WHERE seq=?)`, sessionSeq, seq).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if existing != 0 && existing != current.Int64 {
			merge, err := tx.Exec(`UPDATE conversation_work SET tool_calls=tool_calls+(SELECT tool_calls FROM conversation_work WHERE seq=?),
			 uncertain_outcome=MAX(uncertain_outcome,(SELECT uncertain_outcome FROM conversation_work WHERE seq=?)),
			 uncertain_task=COALESCE(uncertain_task,(SELECT uncertain_task FROM conversation_work WHERE seq=?)),updated_at=?
			 WHERE seq=? AND NOT (uncertain_outcome=1 AND (SELECT uncertain_outcome FROM conversation_work WHERE seq=?)=1)`, current.Int64, current.Int64, current.Int64, now(), existing, current.Int64)
			if err != nil {
				return err
			}
			if n, _ := merge.RowsAffected(); n != 1 {
				return ErrConversationWorkHeld
			}
			if _, err := tx.Exec("UPDATE butler_tasks SET conversation_work=? WHERE seq=?", existing, seq); err != nil {
				return err
			}
			if _, err := tx.Exec("DELETE FROM conversation_work WHERE seq=?", current.Int64); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(`UPDATE conversation_work SET session=?,updated_at=? WHERE seq=? AND (session IS NULL OR session=?)`, sessionSeq, now(), current.Int64, sessionSeq); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// TaskOrigin is the small part of a task an authority check needs: who
// may carry it on, and what it arrived over.
type TaskOrigin struct {
	Owner            string
	OriginConnection string
	Ref              int64
}

// ReadTaskOrigin fetches just those three, for the check that runs on
// EVERY tool authority question.
//
// GetButlerTask reads twenty-odd columns across two joins to answer
// "who owns this", and Capabilities asks once per registered tool — so
// describing a surface went from zero task reads to one per tool. This
// keeps the one join it actually needs (the owner's current handle) and
// drops the rest.
func (db *DB) ReadTaskOrigin(id string) (TaskOrigin, error) {
	seq, err := parseButlerTaskID(id)
	if err != nil {
		return TaskOrigin{}, ErrNotFound
	}
	var o TaskOrigin
	err = db.sql.QueryRow(
		`SELECT w.member, t.origin_connection, t.origin_connection_ref
		 FROM butler_tasks t
		 JOIN butler_members w ON w.person_id = t.owner_person_id
		 WHERE t.seq = ?`, seq).Scan(&o.Owner, &o.OriginConnection, &o.Ref)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskOrigin{}, ErrNotFound
	}
	if err != nil {
		return TaskOrigin{}, fmt.Errorf("runtimedb: reading a butler task's origin: %w", err)
	}
	return o, nil
}

// ButlerTaskForSession returns the conversation task a Butler session is
// being driven by, or ErrNotFound.
//
// This is what makes a conversation RESUMABLE rather than merely
// durable. A restart loses everything in memory, and the only handle a
// chat thread still holds afterwards is the session it was pointed at;
// without a way back from that to the task, the next message opens a
// second task and re-dispatches work the first one had already verified
// — which is the repeated dispatch the step ledger exists to prevent.
//
// The newest task wins. A session that somehow carries more than one is
// a bug rather than a shape to support, and answering with the newest
// keeps the conversation moving instead of failing on ambiguity a
// person cannot resolve.
func (db *DB) ButlerTaskForSession(sessionID string) (ButlerTask, error) {
	var seq int64
	if _, err := fmt.Sscanf(sessionID, "bs-%d", &seq); err != nil {
		return ButlerTask{}, fmt.Errorf("runtimedb: malformed butler session id %q", sessionID)
	}
	return scanButlerTask(db.sql.QueryRow(
		butlerTaskColumns+` WHERE t.session = ? OR EXISTS (SELECT 1 FROM session_task_sources l WHERE l.session=? AND l.task=t.seq) ORDER BY t.seq DESC LIMIT 1`, seq, seq))
}

// Author and owner come back as the handles those people carry now, so
// a task reads the same after either of them is renamed.
const butlerTaskColumns = `SELECT t.seq, t.created_at, t.updated_at, t.kind, t.state,
	 a.member, o.member,
	 t.surface, t.agent_class, t.audience, t.session, t.origin_turn, t.origin_connection,
	 t.origin_connection_ref, t.correlation_id, t.objective,
	 t.completion_criteria, t.grants_json, t.budget_json, t.deadline_at, t.next_wakeup_at,
	 t.lease_epoch, t.uncertainty, t.result_summary, t.retention, t.envelope_version, t.objective_kind, t.proposal_only,
	 t.as_owner, t.system_extra
	 FROM butler_tasks t
	 JOIN butler_members a ON a.person_id = t.author_person_id
	 JOIN butler_members o ON o.person_id = t.owner_person_id`

func scanButlerTask(row rowScanner) (ButlerTask, error) {
	var t ButlerTask
	var created, updated, kind, state, surface, audience, retention string
	var deadline, wakeup string
	var session sql.NullInt64
	var originTurn sql.NullInt64
	err := row.Scan(&t.Seq, &created, &updated, &kind, &state, &t.Author, &t.Owner,
		&surface, &t.AgentClass, &audience, &session, &originTurn, &t.OriginConnection,
		&t.OriginConnectionRef, &t.CorrelationID,
		&t.Objective, &t.CompletionCriteria, &t.GrantsJSON, &t.BudgetJSON,
		&deadline, &wakeup, &t.LeaseEpoch, &t.Uncertainty, &t.ResultSummary,
		&retention, &t.EnvelopeVersion, &t.ObjectiveKind, &t.ProposalOnly,
		&t.AsOwner, &t.SystemExtra)
	if errors.Is(err, sql.ErrNoRows) {
		return ButlerTask{}, ErrNotFound
	}
	if err != nil {
		return ButlerTask{}, fmt.Errorf("runtimedb: scanning butler task: %w", err)
	}
	t.ID = butlerTaskID(t.Seq)
	t.Kind = ButlerTaskKind(kind)
	t.State = ButlerTaskState(state)
	t.Surface = ButlerSurface(surface)
	t.Audience = ButlerTaskAudience(audience)
	t.Retention = ButlerTaskRetention(retention)
	if session.Valid {
		t.SessionID = butlerSessionID(session.Int64)
	}
	if originTurn.Valid {
		n := int(originTurn.Int64)
		t.OriginTurn = &n
	}
	for _, f := range []struct {
		text string
		into *time.Time
		name string
	}{
		{created, &t.CreatedAt, "created_at"},
		{updated, &t.UpdatedAt, "updated_at"},
		{deadline, &t.DeadlineAt, "deadline_at"},
		{wakeup, &t.NextWakeupAt, "next_wakeup_at"},
	} {
		if f.text == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, f.text)
		if err != nil {
			return ButlerTask{}, fmt.Errorf("runtimedb: bad butler task %s %q: %w", f.name, f.text, err)
		}
		*f.into = parsed
	}
	return t, nil
}

// ListButlerTasks returns the newest limit tasks, newest first. An empty
// owner lists every owner's; naming one lists that member's work.
func (db *DB) ListButlerTasks(limit int, owner string) ([]ButlerTask, error) {
	if limit <= 0 {
		limit = 50
	}
	where, args := "", []any{}
	if owner != "" {
		// Listing somebody else's work is a read, so an owner nobody
		// knows lists nothing rather than failing.
		id, err := personIDForRead(db.sql, owner)
		if err != nil {
			return nil, err
		}
		where = " WHERE t.owner_person_id = ?"
		args = append(args, id)
	}
	args = append(args, limit)
	rows, err := db.sql.Query(butlerTaskColumns+where+` ORDER BY t.seq DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler tasks: %w", err)
	}
	defer rows.Close()
	return collectButlerTasks(rows)
}

// ButlerTasksInState returns every task in state, oldest first — the
// resume queue after a restart.
func (db *DB) ButlerTasksInState(state ButlerTaskState) ([]ButlerTask, error) {
	rows, err := db.sql.Query(butlerTaskColumns+` WHERE t.state = ? ORDER BY t.seq`, string(state))
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler tasks in %s: %w", state, err)
	}
	defer rows.Close()
	return collectButlerTasks(rows)
}

func collectButlerTasks(rows *sql.Rows) ([]ButlerTask, error) {
	var out []ButlerTask
	for rows.Next() {
		t, err := scanButlerTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TransitionButlerTask moves a task to a new state, enforcing the closed
// table and appending to the transition history in the same transaction.
//
// It deliberately cannot reach ButlerTaskCompleted: completion needs
// evidence, so it goes through CompleteButlerTask. The schema trigger
// enforces the same thing a layer down, for anything that reaches SQL by
// another route.
func (db *DB) TransitionButlerTask(id string, to ButlerTaskState, reason, actor string) error {
	if to == ButlerTaskCompleted {
		return fmt.Errorf("runtimedb: butler task %s: completion requires evidence — use CompleteButlerTask (ADR-W030 A4)", id)
	}
	seq, err := parseButlerTaskID(id)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: transition butler task: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := transitionButlerTaskTx(tx, seq, to, reason, actor); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: transition butler task: %w", err)
	}
	return nil
}

func transitionButlerTaskTx(tx *sql.Tx, seq int64, to ButlerTaskState, reason, actor string) error {
	var cur string
	err := tx.QueryRow(`SELECT state FROM butler_tasks WHERE seq = ?`, seq).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: transition butler task: %w", err)
	}
	allowed := false
	for _, next := range butlerTaskTransitions[ButlerTaskState(cur)] {
		if next == to {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("runtimedb: butler task %s: illegal transition %s → %s",
			butlerTaskID(seq), cur, to)
	}
	stamp := now()
	if _, err := tx.Exec(`UPDATE butler_tasks SET state = ?, updated_at = ? WHERE seq = ?`,
		string(to), stamp, seq); err != nil {
		return fmt.Errorf("runtimedb: transition butler task: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO butler_task_transitions (task, at, from_state, to_state, reason, actor)
		 VALUES (?, ?, ?, ?, ?, ?)`, seq, stamp, cur, string(to), reason, actor); err != nil {
		return fmt.Errorf("runtimedb: recording butler task transition: %w", err)
	}
	return nil
}

// ButlerTaskTransition is one row of a task's append-only history.
type ButlerTaskTransition struct {
	At        time.Time `json:"at"`
	FromState string    `json:"fromState"`
	ToState   string    `json:"toState"`
	Reason    string    `json:"reason,omitempty"`
	Actor     string    `json:"actor,omitempty"`
}

// ListButlerTaskTransitions returns a task's history, oldest first.
func (db *DB) ListButlerTaskTransitions(id string) ([]ButlerTaskTransition, error) {
	seq, err := parseButlerTaskID(id)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT at, from_state, to_state, reason, actor
		 FROM butler_task_transitions WHERE task = ? ORDER BY seq`, seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler task transitions: %w", err)
	}
	defer rows.Close()
	var out []ButlerTaskTransition
	for rows.Next() {
		var t ButlerTaskTransition
		var at string
		if err := rows.Scan(&at, &t.FromState, &t.ToState, &t.Reason, &t.Actor); err != nil {
			return nil, err
		}
		if t.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("runtimedb: bad butler transition timestamp %q: %w", at, err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// jsonObjectOrEmpty validates an optional JSON payload, defaulting to
// "{}". A payload that does not parse is refused here rather than
// becoming a row nothing downstream can read.
func jsonObjectOrEmpty(raw, field string) (string, error) {
	if raw == "" {
		return "{}", nil
	}
	if !json.Valid([]byte(raw)) {
		return "", fmt.Errorf("runtimedb: butler task %s is not valid JSON", field)
	}
	return raw, nil
}

// timeText renders an optional timestamp in the storage format; the zero
// time becomes "", which is how the schema spells "unset".
func timeText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
