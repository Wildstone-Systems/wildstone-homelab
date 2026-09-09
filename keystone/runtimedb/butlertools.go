package runtimedb

// Tool controls, grants, and capability requests (ADR-W030 A7/A8, P7).
//
// The effective-authority resolver at the bottom of this file is the one
// place that decides whether a dispatch may happen. Every caller goes
// through it, and it fails closed at each step.
//
// The order of the checks is the design:
//
//   1. Structural. If the profile's gateway does not register the tool,
//      nothing here can conjure it. A grant that could widen the ceiling
//      would make the structural boundary advisory, and the reason it
//      was built structurally is that it is not.
//   2. Disabled. An administrator's switch wins over a cached manifest,
//      a saved task, and a grant issued a minute ago. "Takes effect
//      immediately" is only true if disable is checked before anything
//      that could override it.
//   3. Enabled. An explicit switch on.
//   4. Grant. For a tool that needs one, a live grant bound to THIS
//      task or routine.
//   5. Default. Otherwise the tool's own default.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Tool control states.
const (
	ToolEnabled  = "enabled"
	ToolDisabled = "disabled"
)

// Grant scopes.
const (
	GrantScopeTask    = "task"
	GrantScopeRoutine = "routine"
)

// Capability request kinds and states.
const (
	RequestSupportedTool = "supported-tool"
	RequestDevelopment   = "development"

	RequestOpen        = "open"
	RequestApproved    = "approved"
	RequestDeclined    = "declined"
	RequestImplemented = "implemented"
)

// ToolControl is an administrator's switch for one profile and tool.
type ToolControl struct {
	Profile   string    `json:"profile"`
	Tool      string    `json:"tool"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updatedAt"`
	UpdatedBy string    `json:"updatedBy"`
	Reason    string    `json:"reason,omitempty"`
}

// SetToolControl records a switch. Setting a control for a profile that
// does not structurally have the tool is refused by the caller, not
// here: this table describes intent, and intent about a tool that does
// not exist on a surface is a mistake worth naming at the API.
func (db *DB) SetToolControl(profile, tool, state, by, reason string) error {
	if state != ToolEnabled && state != ToolDisabled {
		return fmt.Errorf("runtimedb: tool control state must be enabled|disabled, got %q", state)
	}
	if by == "" {
		return fmt.Errorf("runtimedb: a tool control requires the administrator who set it (ADR-W030 A7)")
	}
	_, err := db.sql.Exec(
		`INSERT INTO butler_tool_controls (profile, tool, state, updated_at, updated_by, reason)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(profile, tool) DO UPDATE SET
		   state = excluded.state, updated_at = excluded.updated_at,
		   updated_by = excluded.updated_by, reason = excluded.reason`,
		profile, tool, state, now(), by, reason)
	if err != nil {
		return fmt.Errorf("runtimedb: setting tool control: %w", err)
	}
	return nil
}

// ClearToolControl removes a switch, returning the tool to its default.
func (db *DB) ClearToolControl(profile, tool string) error {
	_, err := db.sql.Exec(`DELETE FROM butler_tool_controls WHERE profile = ? AND tool = ?`, profile, tool)
	if err != nil {
		return fmt.Errorf("runtimedb: clearing tool control: %w", err)
	}
	return nil
}

// ListToolControls returns every switch for a profile ("" for all).
func (db *DB) ListToolControls(profile string) ([]ToolControl, error) {
	where, args := "", []any{}
	if profile != "" {
		where = " WHERE profile = ?"
		args = append(args, profile)
	}
	rows, err := db.sql.Query(
		`SELECT profile, tool, state, updated_at, updated_by, reason
		 FROM butler_tool_controls`+where+` ORDER BY profile, tool`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing tool controls: %w", err)
	}
	defer rows.Close()
	var out []ToolControl
	for rows.Next() {
		var c ToolControl
		var updated string
		if err := rows.Scan(&c.Profile, &c.Tool, &c.State, &updated, &c.UpdatedBy, &c.Reason); err != nil {
			return nil, err
		}
		if c.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MaxGrantWindow bounds a temporary grant. A grant that outlives the
// afternoon it was issued for is a permanent permission with extra
// paperwork.
const MaxGrantWindow = 24 * time.Hour

// ToolGrant is one temporary permission bound to a piece of work.
type ToolGrant struct {
	Seq         int64     `json:"seq"`
	Scope       string    `json:"scope"`
	ScopeID     string    `json:"scopeId"`
	Profile     string    `json:"profile"`
	Tool        string    `json:"tool"`
	GrantedAt   time.Time `json:"grantedAt"`
	GrantedBy   string    `json:"grantedBy"`
	ExpiresAt   time.Time `json:"expiresAt"`
	RevokedAt   time.Time `json:"revokedAt,omitzero"`
	RevokedBy   string    `json:"revokedBy,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	CleanupPlan string    `json:"cleanupPlan,omitempty"`
}

// GrantTool issues a temporary, work-bound grant.
func (db *DB) GrantTool(g ToolGrant, window time.Duration) (int64, error) {
	if g.Scope != GrantScopeTask && g.Scope != GrantScopeRoutine {
		return 0, fmt.Errorf("runtimedb: grant scope must be task|routine, got %q", g.Scope)
	}
	if g.ScopeID == "" || g.Tool == "" || g.GrantedBy == "" {
		return 0, fmt.Errorf("runtimedb: a grant requires a scope id, a tool, and who granted it (ADR-W030 A8)")
	}
	if window <= 0 || window > MaxGrantWindow {
		return 0, fmt.Errorf("runtimedb: a grant window must be between 0 and %s, got %s", MaxGrantWindow, window)
	}
	// A task grant is checked against the task, so a grant for a task
	// that does not exist is refused rather than becoming a permission
	// nothing can expire.
	if g.Scope == GrantScopeTask {
		if _, err := db.GetButlerTask(g.ScopeID); err != nil {
			return 0, fmt.Errorf("runtimedb: cannot grant against task %s: %w", g.ScopeID, err)
		}
	}
	res, err := db.sql.Exec(
		`INSERT INTO butler_tool_grants (scope, scope_id, profile, tool, granted_at, granted_by,
		     expires_at, reason, cleanup_plan)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.Scope, g.ScopeID, g.Profile, g.Tool, now(), g.GrantedBy,
		time.Now().UTC().Add(window).Format(time.RFC3339Nano), g.Reason, g.CleanupPlan)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: granting tool: %w", err)
	}
	return res.LastInsertId()
}

// RevokeToolGrant withdraws one grant immediately.
func (db *DB) RevokeToolGrant(seq int64, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: revoking a grant requires who did it")
	}
	res, err := db.sql.Exec(
		`UPDATE butler_tool_grants SET revoked_at = ?, revoked_by = ? WHERE seq = ? AND revoked_at = ''`,
		now(), by, seq)
	if err != nil {
		return fmt.Errorf("runtimedb: revoking grant: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeGrantsForScope withdraws every live grant on one task or
// routine — what a cancellation and a completion both do.
func (db *DB) RevokeGrantsForScope(scope, scopeID, by string) (int, error) {
	res, err := db.sql.Exec(
		`UPDATE butler_tool_grants SET revoked_at = ?, revoked_by = ?
		 WHERE scope = ? AND scope_id = ? AND revoked_at = ''`, now(), by, scope, scopeID)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: revoking grants for %s %s: %w", scope, scopeID, err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ListToolGrants returns the grants on one scope, newest first,
// including expired and revoked ones — the audit view.
func (db *DB) ListToolGrants(scope, scopeID string) ([]ToolGrant, error) {
	rows, err := db.sql.Query(
		`SELECT seq, scope, scope_id, profile, tool, granted_at, granted_by, expires_at,
		        revoked_at, revoked_by, reason, cleanup_plan
		 FROM butler_tool_grants WHERE scope = ? AND scope_id = ? ORDER BY seq DESC`, scope, scopeID)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing grants: %w", err)
	}
	defer rows.Close()
	var out []ToolGrant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func scanGrant(row rowScanner) (ToolGrant, error) {
	var g ToolGrant
	var granted, expires, revoked string
	if err := row.Scan(&g.Seq, &g.Scope, &g.ScopeID, &g.Profile, &g.Tool, &granted, &g.GrantedBy,
		&expires, &revoked, &g.RevokedBy, &g.Reason, &g.CleanupPlan); err != nil {
		return ToolGrant{}, err
	}
	var err error
	if g.GrantedAt, err = time.Parse(time.RFC3339Nano, granted); err != nil {
		return ToolGrant{}, err
	}
	if g.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires); err != nil {
		return ToolGrant{}, err
	}
	if revoked != "" {
		if g.RevokedAt, err = time.Parse(time.RFC3339Nano, revoked); err != nil {
			return ToolGrant{}, err
		}
	}
	return g, nil
}

// ToolAuthority is the resolver's answer: whether a dispatch may happen,
// and — as importantly — why not.
//
// Reason is written for a person, because it is what the model is told
// and what the Console shows. "You do not have that tool here" and "an
// administrator turned that off" are different situations, and a caller
// that reports them identically produces a household that cannot tell a
// missing capability from a withdrawn one.
type ToolAuthority struct {
	Tool    string `json:"tool"`
	Profile string `json:"profile"`
	Allowed bool   `json:"allowed"`
	// Source names which rule decided: "structural", "disabled",
	// "enabled", "grant", or "default".
	Source string `json:"source"`
	Reason string `json:"reason,omitempty"`
	// Requestable is true when the answer is no but a person could
	// change it — the case A7 calls "request a missing capability".
	Requestable bool `json:"requestable,omitempty"`
	// GrantSeq names the grant that allowed it, for the trace.
	GrantSeq int64 `json:"grantSeq,omitempty"`
}

// ToolQuery is what the resolver needs to know about a dispatch. The
// caller supplies the structural facts because only the gateway knows
// them, and the store supplies the administrative ones.
type ToolQuery struct {
	Profile string
	Tool    string
	// Structural is whether the profile's gateway registers this tool at
	// all. False is the end of the conversation.
	Structural bool
	// GrantRequired is the tool's own metadata: a tool that needs an
	// explicit, work-bound grant even when it is structurally present
	// and not disabled.
	GrantRequired bool
	// TaskID scopes a task-bound grant; RoutineName a routine-bound one.
	// Both may be empty, in which case no grant can apply.
	TaskID      string
	RoutineName string
}

// ResolveToolAuthority is the effective-authority resolver every
// dispatch goes through.
func (db *DB) ResolveToolAuthority(q ToolQuery) (ToolAuthority, error) {
	a := ToolAuthority{Tool: q.Tool, Profile: q.Profile}

	// 1. Structural. Nothing below can widen this.
	if !q.Structural {
		a.Source = "structural"
		a.Reason = fmt.Sprintf("this surface does not have %s — it is not a setting, the tool is absent", q.Tool)
		// Requestable: A7 lets someone ASK for a capability, and asking
		// is not having.
		a.Requestable = true
		return a, nil
	}

	// 2. Disabled wins over everything, including a live grant.
	var state, reason string
	err := db.sql.QueryRow(
		`SELECT state, reason FROM butler_tool_controls WHERE profile = ? AND tool = ?`,
		q.Profile, q.Tool).Scan(&state, &reason)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return ToolAuthority{}, fmt.Errorf("runtimedb: resolving tool authority: %w", err)
	case state == ToolDisabled:
		a.Source = "disabled"
		a.Reason = "an administrator has turned that off here"
		if reason != "" {
			a.Reason += ": " + reason
		}
		a.Requestable = true
		return a, nil
	case state == ToolEnabled:
		a.Allowed = true
		a.Source = "enabled"
		return a, nil
	}

	// 3. A tool that needs a grant needs a LIVE one bound to this work.
	if q.GrantRequired {
		grant, ok, err := db.liveGrant(q)
		if err != nil {
			return ToolAuthority{}, err
		}
		if !ok {
			a.Source = "grant"
			a.Reason = fmt.Sprintf("%s needs a grant for this piece of work, and there is not one", q.Tool)
			a.Requestable = true
			return a, nil
		}
		a.Allowed = true
		a.Source = "grant"
		a.GrantSeq = grant.Seq
		return a, nil
	}

	a.Allowed = true
	a.Source = "default"
	return a, nil
}

// liveGrant finds an unrevoked, unexpired grant on this query's work
// whose task, if it names one, is still going.
//
// The task check is what makes "grants expire at task end" true without
// a sweeper: a grant on a completed or cancelled task resolves to
// nothing the moment the task's state changes, whatever its deadline
// says.
func (db *DB) liveGrant(q ToolQuery) (ToolGrant, bool, error) {
	type candidate struct{ scope, id string }
	var candidates []candidate
	if q.TaskID != "" {
		candidates = append(candidates, candidate{GrantScopeTask, q.TaskID})
	}
	if q.RoutineName != "" {
		candidates = append(candidates, candidate{GrantScopeRoutine, q.RoutineName})
	}
	stamp := now()
	for _, c := range candidates {
		if c.scope == GrantScopeTask {
			task, err := db.GetButlerTask(c.id)
			if err != nil {
				continue
			}
			switch task.State {
			case ButlerTaskPending, ButlerTaskRunning, ButlerTaskWaiting:
			default:
				// The work is over. Its grants are over with it.
				continue
			}
		}
		row := db.sql.QueryRow(
			`SELECT seq, scope, scope_id, profile, tool, granted_at, granted_by, expires_at,
			        revoked_at, revoked_by, reason, cleanup_plan
			 FROM butler_tool_grants
			 WHERE scope = ? AND scope_id = ? AND profile = ? AND tool = ?
			   AND revoked_at = '' AND expires_at > ?
			 ORDER BY seq DESC LIMIT 1`, c.scope, c.id, q.Profile, q.Tool, stamp)
		g, err := scanGrant(row)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return ToolGrant{}, false, fmt.Errorf("runtimedb: reading grant: %w", err)
		}
		return g, true, nil
	}
	return ToolGrant{}, false, nil
}

// CapabilityRequest is the Butler asking for something it does not have.
type CapabilityRequest struct {
	Seq          int64     `json:"seq"`
	Kind         string    `json:"kind"`
	RequestedBy  string    `json:"requestedBy"`
	Profile      string    `json:"profile"`
	Tool         string    `json:"tool,omitempty"`
	Description  string    `json:"description"`
	State        string    `json:"state"`
	CreatedAt    time.Time `json:"createdAt"`
	DecidedAt    time.Time `json:"decidedAt,omitzero"`
	DecidedBy    string    `json:"decidedBy,omitempty"`
	DecisionNote string    `json:"decisionNote,omitempty"`
}

// FileCapabilityRequest records a request. It grants nothing, and
// deliberately cannot: approving a request and enabling a tool are two
// acts, so that approving one cannot turn something on by accident.
func (db *DB) FileCapabilityRequest(r CapabilityRequest) (int64, error) {
	if r.Kind != RequestSupportedTool && r.Kind != RequestDevelopment {
		return 0, fmt.Errorf("runtimedb: request kind must be supported-tool|development, got %q", r.Kind)
	}
	if r.RequestedBy == "" || r.Description == "" {
		return 0, fmt.Errorf("runtimedb: a capability request needs who it is for and what is being asked for (ADR-W030 A7)")
	}
	res, err := db.sql.Exec(
		`INSERT INTO butler_capability_requests (kind, requested_by, profile, tool, description, state, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.Kind, r.RequestedBy, r.Profile, r.Tool, r.Description, RequestOpen, now())
	if err != nil {
		return 0, fmt.Errorf("runtimedb: filing capability request: %w", err)
	}
	return res.LastInsertId()
}

// DecideCapabilityRequest records a person's answer. Approving does NOT
// enable the tool; SetToolControl does, separately and visibly.
func (db *DB) DecideCapabilityRequest(seq int64, state, by, note string) error {
	switch state {
	case RequestApproved, RequestDeclined, RequestImplemented:
	default:
		return fmt.Errorf("runtimedb: a request is decided approved|declined|implemented, got %q", state)
	}
	if by == "" {
		return fmt.Errorf("runtimedb: deciding a request requires who decided")
	}
	res, err := db.sql.Exec(
		`UPDATE butler_capability_requests SET state = ?, decided_at = ?, decided_by = ?, decision_note = ?
		 WHERE seq = ?`, state, now(), by, note, seq)
	if err != nil {
		return fmt.Errorf("runtimedb: deciding capability request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListCapabilityRequests returns requests in one state ("" for all),
// newest first.
func (db *DB) ListCapabilityRequests(state string) ([]CapabilityRequest, error) {
	where, args := "", []any{}
	if state != "" {
		where = " WHERE state = ?"
		args = append(args, state)
	}
	rows, err := db.sql.Query(
		`SELECT seq, kind, requested_by, profile, tool, description, state, created_at,
		        decided_at, decided_by, decision_note
		 FROM butler_capability_requests`+where+` ORDER BY seq DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing capability requests: %w", err)
	}
	defer rows.Close()
	var out []CapabilityRequest
	for rows.Next() {
		var r CapabilityRequest
		var created, decided string
		if err := rows.Scan(&r.Seq, &r.Kind, &r.RequestedBy, &r.Profile, &r.Tool, &r.Description,
			&r.State, &created, &decided, &r.DecidedBy, &r.DecisionNote); err != nil {
			return nil, err
		}
		if r.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		if decided != "" {
			if r.DecidedAt, err = time.Parse(time.RFC3339Nano, decided); err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
