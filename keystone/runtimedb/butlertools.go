package runtimedb

// Tool controls, grants, and capability requests (ADR-W030 A7/A8, P7).
//
// The effective-authority resolver at the bottom of this file is the one
// place that decides whether a dispatch may happen. Every caller goes
// through it, and it fails closed at each step.
//
// The order of the checks is the design:
//
//   1. Structural. If the class's gateway does not register the tool,
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
	"strings"
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

// ToolControl is an administrator's switch for one agent class and tool.
type ToolControl struct {
	AgentClass string    `json:"agentClass"`
	Tool       string    `json:"tool"`
	State      string    `json:"state"`
	UpdatedAt  time.Time `json:"updatedAt"`
	UpdatedBy  string    `json:"updatedBy"`
	Reason     string    `json:"reason,omitempty"`
}

// SetToolControl records a switch. Setting a control for a class that
// does not structurally have the tool is refused by the caller, not
// here: this table describes intent, and intent about a tool that does
// not exist on a surface is a mistake worth naming at the API.
func (db *DB) SetToolControl(class, tool, state, by, reason string) error {
	if state != ToolEnabled && state != ToolDisabled {
		return fmt.Errorf("runtimedb: tool control state must be enabled|disabled, got %q", state)
	}
	if !ValidAgentClass(class) {
		return fmt.Errorf("runtimedb: a tool control names an agent class, must be %s|%s, got %q (design §5.2)",
			AgentClassOwner, AgentClassMember, class)
	}
	if by == "" {
		return fmt.Errorf("runtimedb: a tool control requires the administrator who set it (ADR-W030 A7)")
	}
	_, err := db.sql.Exec(
		`INSERT INTO butler_tool_controls (agent_class, tool, state, updated_at, updated_by, reason)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(agent_class, tool) DO UPDATE SET
		   state = excluded.state, updated_at = excluded.updated_at,
		   updated_by = excluded.updated_by, reason = excluded.reason`,
		class, tool, state, now(), by, reason)
	if err != nil {
		return fmt.Errorf("runtimedb: setting tool control: %w", err)
	}
	return nil
}

// ClearToolControl removes a switch, returning the tool to its default.
func (db *DB) ClearToolControl(class, tool string) error {
	_, err := db.sql.Exec(`DELETE FROM butler_tool_controls WHERE agent_class = ? AND tool = ?`, class, tool)
	if err != nil {
		return fmt.Errorf("runtimedb: clearing tool control: %w", err)
	}
	return nil
}

// ListToolControls returns every switch for a class ("" for all).
func (db *DB) ListToolControls(class string) ([]ToolControl, error) {
	where, args := "", []any{}
	if class != "" {
		where = " WHERE agent_class = ?"
		args = append(args, class)
	}
	rows, err := db.sql.Query(
		`SELECT agent_class, tool, state, updated_at, updated_by, reason
		 FROM butler_tool_controls`+where+` ORDER BY agent_class, tool`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing tool controls: %w", err)
	}
	defer rows.Close()
	var out []ToolControl
	for rows.Next() {
		var c ToolControl
		var updated string
		if err := rows.Scan(&c.AgentClass, &c.Tool, &c.State, &updated, &c.UpdatedBy, &c.Reason); err != nil {
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
	Seq        int64  `json:"seq"`
	Scope      string `json:"scope"`
	ScopeID    string `json:"scopeId"`
	AgentClass string `json:"agentClass"`
	// Butler is WHOSE grant this is, by butlers.seq. Zero means the
	// pre-0058 class-level shape, which a deployment with no Butlers
	// still uses; see the column comment in 0058 for why those rows are
	// kept rather than attributed to a guess.
	Butler      int64     `json:"butler,omitempty"`
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
	if !ValidAgentClass(g.AgentClass) {
		return 0, fmt.Errorf("runtimedb: a grant names an agent class, must be %s|%s, got %q (design §5.2)",
			AgentClassOwner, AgentClassMember, g.AgentClass)
	}
	// A grant that names an Agent must name one that exists, and one
	// whose class ADMITS the grant's. An ordering, not an equality
	// (§6.1): member tools may be assigned to Member or Owner Agents,
	// owner tools only to Owner Agents. Requiring equality here would
	// refuse an Owner Agent the member tools §6.4 says it gets by
	// default, which is most of what any Agent actually does.
	//
	// The refusal that matters is the other direction: an owner-class
	// grant against a Member Agent would be a permission belonging to
	// nobody — invisible to that Agent's resolution, because the member
	// gateway does not register the tool, and immune to a downgrade it
	// will never have.
	if g.Butler != 0 {
		b, err := db.GetButlerBySeq(g.Butler)
		if err != nil {
			return 0, fmt.Errorf("runtimedb: cannot grant against butler %d: %w", g.Butler, err)
		}
		if !AgentClassAdmits(b.AgentClass, g.AgentClass) {
			return 0, fmt.Errorf(
				"runtimedb: cannot issue a %s grant to %s, which is a %s agent (design §6.1)",
				g.AgentClass, b.Name, b.AgentClass)
		}
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
		`INSERT INTO butler_tool_grants (scope, scope_id, agent_class, butler, tool, granted_at, granted_by,
		     expires_at, reason, cleanup_plan)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.Scope, g.ScopeID, g.AgentClass, nullableSeq(g.Butler), g.Tool, now(), g.GrantedBy,
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
		`SELECT seq, scope, scope_id, agent_class, butler, tool, granted_at, granted_by, expires_at,
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
	var butler sql.NullInt64
	if err := row.Scan(&g.Seq, &g.Scope, &g.ScopeID, &g.AgentClass, &butler, &g.Tool, &granted, &g.GrantedBy,
		&expires, &revoked, &g.RevokedBy, &g.Reason, &g.CleanupPlan); err != nil {
		return ToolGrant{}, err
	}
	g.Butler = butler.Int64
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
	Tool       string `json:"tool"`
	AgentClass string `json:"agentClass"`
	Allowed    bool   `json:"allowed"`
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
	AgentClass string
	// Butler scopes a grant to one Agent. Zero means no Agent scope, and
	// only the class-level grants of the pre-0058 shape can then apply —
	// never another Agent's.
	Butler int64
	Tool   string
	// Structural is whether the class's gateway registers this tool at
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
	a := ToolAuthority{Tool: q.Tool, AgentClass: q.AgentClass}

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
	explicitlyEnabled := false
	err := db.sql.QueryRow(
		`SELECT state, reason FROM butler_tool_controls WHERE agent_class = ? AND tool = ?`,
		q.AgentClass, q.Tool).Scan(&state, &reason)
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
		// Explicitly on — but "on" means the tool is available on this
		// surface, NOT that this piece of work may use it. A
		// grant-required tool still needs its grant, so this falls
		// through rather than returning: an administrator enabling a
		// tool must not be a way around the per-work authorization
		// that made it grant-required in the first place.
		explicitlyEnabled = true
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
	if explicitlyEnabled {
		a.Source = "enabled"
	}
	return a, nil
}

// admittedGrantClasses is every grant class an Agent of this class may
// use, widest first.
//
// An Owner Agent admits both; a Member Agent admits only member. The
// ordering in §6.1 read from the resolver's side.
func admittedGrantClasses(agentClass string) []any {
	if agentClass == AgentClassOwner {
		return []any{AgentClassOwner, AgentClassMember}
	}
	return []any{AgentClassMember}
}

// placeholders builds "?, ?, ?" for an IN clause of n values.
func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.Repeat("?, ", n-1) + "?"
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
		// The Agent is part of the key, not a filter applied afterwards.
		// A query with no Agent scope matches only the class-level rows
		// of the pre-0058 shape; it must never fall through to another
		// Agent's grant, which is the whole reason the column exists.
		//
		// The CLASS is matched by what the Agent ADMITS, not by equality
		// (§6.1). A grant's agent_class is the class the TOOL requires,
		// not a label for the Agent, so an Owner Agent legitimately
		// holds member-class grants — and after an upgrade it holds the
		// ones §6.4 says are preserved. Equality here would leave those
		// rows present but unreachable, which is worse than revoking
		// them: the grant looks preserved and refuses anyway.
		admitted := admittedGrantClasses(q.AgentClass)
		row := db.sql.QueryRow(
			`SELECT seq, scope, scope_id, agent_class, butler, tool, granted_at, granted_by, expires_at,
			        revoked_at, revoked_by, reason, cleanup_plan
			 FROM butler_tool_grants
			 WHERE scope = ? AND scope_id = ? AND agent_class IN (`+placeholders(len(admitted))+`) AND tool = ?
			   AND butler IS ? AND revoked_at = '' AND expires_at > ?
			 ORDER BY seq DESC LIMIT 1`,
			append(append([]any{c.scope, c.id}, admitted...), q.Tool, nullableSeq(q.Butler), stamp)...)
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
	AgentClass   string    `json:"agentClass"`
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
		`INSERT INTO butler_capability_requests (kind, requested_by, agent_class, tool, description, state, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.Kind, r.RequestedBy, r.AgentClass, r.Tool, r.Description, RequestOpen, now())
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
		`SELECT seq, kind, requested_by, agent_class, tool, description, state, created_at,
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
		if err := rows.Scan(&r.Seq, &r.Kind, &r.RequestedBy, &r.AgentClass, &r.Tool, &r.Description,
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
