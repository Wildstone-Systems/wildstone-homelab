package runtimedb

// What each Butler can see and use (ADR-W031 A3/A4/A5, work package B2).
//
// A4 is the decision this file implements: one Butler's material does
// not reach another Butler's model. Everything here exists to make that
// structural rather than a filter somebody remembers to apply, because
// the failure is silent — by the time anyone notices, the material has
// already been sent.
//
// The three scopes do not compose the same way, and the differences are
// deliberate:
//
//	Memory ADDS a dimension. A record already says whose it is; it now
//	also says which Butler holds it. Both filters apply. Swapping one
//	for the other would have made a Butler a route to somebody else's
//	private material.
//
//	Tools SUBTRACT only. A Butler control can disable what its profile
//	allows and can never enable what the profile denied (A3).
//
//	Connections FAIL CLOSED. Absent means no. A new Butler seeing every
//	integration by default is the crossover A4 forbids, arriving on the
//	day somebody forgets to scope one.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SharedPool is the Butler scope any Butler may read: household facts
// that are nobody's specialism — "we are vegetarian" (A5).
//
// It is the empty string so that a caller which forgets to name a
// Butler sees the shared pool and nothing else. Omission fails closed,
// which is the only direction it can safely fail.
const SharedPool = ""

// AllButlers is the Console holder's scope. A10 gives them every
// Butler, so their own view is not narrowed by any one Butler's.
//
// It is a sentinel rather than a bool because a scope that widens
// should be something a caller has to write down. "" already means
// something, and a bool next to a string field is exactly the pair
// somebody sets to the wrong one.
const AllButlers = "*"

// scopeClause builds the Butler half of a permission filter.
//
// It returns SQL and args to be ANDed with the reader's half, never to
// replace it. A caller that used this alone would be asking "does this
// Butler hold it" without asking "may this person see it".
func scopeClause(column, butler string) (string, []any) {
	switch butler {
	case AllButlers:
		return "", nil
	case SharedPool:
		return fmt.Sprintf(" AND %s = ''", column), nil
	default:
		return fmt.Sprintf(" AND (%s = ? OR %s = '')", column, column), []any{butler}
	}
}

// GrantButlerConnection lets one Butler read one connection.
func (db *DB) GrantButlerConnection(butler, connection, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: granting a butler a connection needs who granted it")
	}
	b, err := db.GetButler(butler)
	if err != nil {
		return err
	}
	c, err := db.GetConnection(connection)
	if err != nil {
		return err
	}
	if _, err := db.sql.Exec(
		`INSERT INTO butler_connection_scope (butler, connection, granted_at, granted_by)
		 VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`, b.Seq, c.Seq, now(), by); err != nil {
		return fmt.Errorf("runtimedb: granting a butler a connection: %w", err)
	}
	return nil
}

// RevokeButlerConnection is an immediate denial: the Butler's next read
// of that connection is refused.
func (db *DB) RevokeButlerConnection(butler, connection, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: revoking a butler's connection needs who revoked it")
	}
	b, err := db.GetButler(butler)
	if err != nil {
		return err
	}
	c, err := db.GetConnection(connection)
	if err != nil {
		return err
	}
	res, err := db.sql.Exec(
		`UPDATE butler_connection_scope SET revoked_at = ?, revoked_by = ?
		 WHERE butler = ? AND connection = ? AND revoked_at = ''`, now(), by, b.Seq, c.Seq)
	if err != nil {
		return fmt.Errorf("runtimedb: revoking a butler's connection: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ButlerConnections lists the connections this Butler may read.
func (db *DB) ButlerConnections(butler string) ([]string, error) {
	b, err := db.GetButler(butler)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT c.name FROM butler_connection_scope s
		 JOIN integration_connections c ON c.seq = s.connection
		 WHERE s.butler = ? AND s.revoked_at = '' ORDER BY c.name`, b.Seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing a butler's connections: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ButlerMayReadConnection is the check every integration read goes
// through.
//
// Absent is NO. That is the whole design: the accounting Butler cannot
// see the gym data because it was never granted it, not because
// something looked at the request and refused.
func (db *DB) ButlerMayReadConnection(butler, connection string) error {
	b, err := db.GetButler(butler)
	if err != nil {
		return err
	}
	c, err := db.GetConnection(connection)
	if err != nil {
		return err
	}
	var n int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM butler_connection_scope
		 WHERE butler = ? AND connection = ? AND revoked_at = ''`, b.Seq, c.Seq).Scan(&n); err != nil {
		return fmt.Errorf("runtimedb: checking a butler's connection: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s has not been given %s", ErrNoButlerAccess, butler, connection)
	}
	return nil
}

// NarrowButlerTool turns a tool off for one Butler.
//
// There is no widening counterpart, and the schema refuses one. A
// Butler narrows what its profile allows; a control that could add to
// it would make A3's ceiling advisory (ADR-W031 A3).
func (db *DB) NarrowButlerTool(butler, tool, reason, by string) error {
	if tool == "" || by == "" {
		return fmt.Errorf("runtimedb: narrowing a butler's tools needs the tool and who did it")
	}
	b, err := db.GetButler(butler)
	if err != nil {
		return err
	}
	_, err = db.sql.Exec(
		`INSERT INTO butler_tool_scope (butler, tool, state, reason, updated_at, updated_by)
		 VALUES (?, ?, 'disabled', ?, ?, ?)
		 ON CONFLICT(butler, tool) DO UPDATE SET reason = excluded.reason,
		   updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		b.Seq, tool, reason, now(), by)
	if err != nil {
		return fmt.Errorf("runtimedb: narrowing a butler's tools: %w", err)
	}
	return nil
}

// WidenButlerTool removes a Butler-level narrowing, returning the tool
// to whatever its profile says.
//
// This is not a widening past the ceiling — it is the removal of a
// narrowing below it. The profile still decides, and a tool the profile
// disabled stays disabled.
func (db *DB) WidenButlerTool(butler, tool string) error {
	b, err := db.GetButler(butler)
	if err != nil {
		return err
	}
	res, err := db.sql.Exec(
		`DELETE FROM butler_tool_scope WHERE butler = ? AND tool = ?`, b.Seq, tool)
	if err != nil {
		return fmt.Errorf("runtimedb: widening a butler's tools: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ButlerToolNarrowings lists the tools this Butler has been denied
// below its profile.
func (db *DB) ButlerToolNarrowings(butler string) (map[string]string, error) {
	b, err := db.GetButler(butler)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT tool, reason FROM butler_tool_scope WHERE butler = ? ORDER BY tool`, b.Seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing a butler's tool narrowings: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var tool, reason string
		if err := rows.Scan(&tool, &reason); err != nil {
			return nil, err
		}
		out[tool] = reason
	}
	return out, rows.Err()
}

// ResolveButlerTool is the effective answer for one Butler and one
// tool, and it is what a dispatch asks.
//
// The order is the point. A Butler's narrowing is checked BEFORE the
// profile's answer, because it can only ever refuse — so checking it
// first costs nothing and means a narrowed tool cannot be re-allowed by
// anything below. Everything after that is ADR-W030 A7's existing
// resolution, unchanged.
func (db *DB) ResolveButlerTool(butler string, q ToolQuery) (ToolAuthority, error) {
	b, err := db.GetButler(butler)
	if err != nil {
		return ToolAuthority{}, err
	}
	// The Butler's own ceiling comes from its record, never from the
	// caller: a query that could name its own class would be a query
	// that could name a wider one. The same goes for the Agent itself —
	// set here rather than trusted from the query, so a session cannot
	// reach for a grant issued to a different Agent.
	q.AgentClass = b.AgentClass
	q.Butler = b.Seq

	var reason string
	err = db.sql.QueryRow(
		`SELECT reason FROM butler_tool_scope WHERE butler = ? AND tool = ?`,
		b.Seq, q.Tool).Scan(&reason)
	switch {
	case err == nil:
		a := ToolAuthority{Tool: q.Tool, AgentClass: q.AgentClass, Source: "butler"}
		a.Reason = fmt.Sprintf("%s does not have %s", b.Name, q.Tool)
		if reason != "" {
			a.Reason += ": " + reason
		}
		// Requestable, because A7 says asking is not having and a
		// person may decide this Butler should have it after all.
		a.Requestable = true
		return a, nil
	case errors.Is(err, sql.ErrNoRows):
	default:
		return ToolAuthority{}, fmt.Errorf("runtimedb: resolving a butler's tool: %w", err)
	}
	// NARROWINGS KEEP THEIR JOB (#211, deciding slice 9's loose end).
	//
	// They looked vestigial once the assignment model arrived: for a
	// governed Agent, assignments already decide reach. But they are
	// not a second way to say the same thing, for two reasons. They are
	// the ONLY per-Agent control an UNGOVERNED Agent has, which is
	// every Agent an existing household already runs; and they SUBTRACT
	// where an assignment ADDS, so the two compose rather than compete
	// — a bundle that grows a tool later does not quietly hand it to an
	// Agent an Owner narrowed away from it.
	//
	// Layer 1 (the Person, added in #211) sits above both and is
	// deliberately not folded into either: an Agent's reach says
	// nothing about who is asking (§18.6).

	// A GOVERNED Agent reaches exactly what it is assigned (§6.3, §6.4).
	//
	// Only when the tool is structurally present: a tool this gateway
	// does not register is ABSENT, and reporting it as "unassigned"
	// would send an Owner looking for an assignment that could never
	// help. Structural stays the first answer, given below.
	//
	// And only for an enrolled Agent. One that is not governed is the
	// pre-0059 model — everything the gateway registers, minus its
	// narrowings — which is every Agent any existing household has.
	if q.Structural {
		governed, err := db.AgentIsGoverned(b.Seq)
		if err != nil {
			return ToolAuthority{}, err
		}
		if governed {
			ok, why, err := db.assignedTo(b.Seq, q)
			if err != nil {
				return ToolAuthority{}, err
			}
			if !ok {
				a := ToolAuthority{Tool: q.Tool, AgentClass: q.AgentClass, Source: "assignment"}
				switch why {
				case "opt-in":
					// Named as its own case so the sentence does not
					// suggest a bundle would fix it. §6.2 is the whole
					// point: this tool is reached by being named.
					a.Reason = fmt.Sprintf(
						"%s is an opt-in tool, so it is never included by a service bundle — "+
							"it has to be given to %s by name", q.Tool, b.Name)
				case "unclassified":
					a.Reason = fmt.Sprintf(
						"%s was offered without a service or a class, so no bundle could be "+
							"matched against it and it was not run", q.Tool)
				default:
					a.Reason = fmt.Sprintf("%s has not been given %s", b.Name, q.Tool)
				}
				a.Requestable = true
				return a, nil
			}
		}
	}

	return db.ResolveToolAuthority(q)
}

// RecordButlerMemory records a memory record held by one Butler.
//
// The Butler is a scope on top of the record's own owner, not a
// replacement for it: the record still belongs to a person, and that is
// still who may correct it, forget it and export it. What the Butler
// decides is which conversations it can come back out of.
func (db *DB) RecordButlerMemory(butler string, m MemoryRecord) (int64, error) {
	if butler != SharedPool {
		if _, err := db.GetButler(butler); err != nil {
			return 0, err
		}
	}
	// Set on the record rather than applied afterwards. A second,
	// untransacted UPDATE would leave a failed call having committed
	// the record into the SHARED POOL while returning an error — the
	// leak arriving through the error path.
	m.Butler = butler
	if m.Scope == "" && butler != "" {
		m.Scope = MemoryScopeAgentLocal
	}
	return db.RecordMemory(m)
}

// PromoteMemoryToSharedPool moves a record into the pool every Butler
// may read (A5).
//
// It returns the Butlers that gain access and whether any of them runs
// on a cloud model, because A5 says promotion is a disclosure decision
// and is presented as one. A pool item is readable by every Butler
// permitted to read it, and some of those send it out of the house —
// so the act names them rather than happening quietly.
func (db *DB) PromoteMemoryToSharedPool(seq int64, by string) (gains []Butler, leavesHouse bool, err error) {
	if err := db.gate(GateIngestion); err != nil {
		return nil, false, err
	}
	if by == "" {
		return nil, false, fmt.Errorf("runtimedb: promoting memory needs who promoted it")
	}
	m, err := db.GetMemory(seq)
	if err != nil {
		return nil, false, err
	}
	if m.Audience == ButlerAudiencePrivate {
		// A5 is about material that is already shareable. Promoting a
		// private record would widen its AUDIENCE as a side effect of
		// widening its scope, which is two decisions wearing one
		// button.
		return nil, false, fmt.Errorf(
			"runtimedb: memory %d is private to %s — share it first if that is the intent; promoting a scope must not widen an audience (ADR-W031 A5)",
			seq, m.Owner)
	}
	// The disclosure is computed the same way the preview computes it,
	// so what a person was shown before deciding and what is recorded
	// afterwards cannot drift apart.
	d, err := db.PreviewPoolPromotion(seq)
	if err != nil {
		return nil, false, err
	}
	for _, name := range d.Butlers {
		b, err := db.GetButler(name)
		if err != nil {
			return nil, false, err
		}
		gains = append(gains, b)
	}
	leavesHouse = d.LeavesHouse

	// The promotion and its record are one transaction. Recording first
	// and updating after left a disclosure describing a promotion that
	// might not have happened; updating first and recording after would
	// leave the opposite. Either way the audit stops matching the data,
	// which is the one thing an audit cannot do.
	tx, err := db.sql.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("runtimedb: promoting memory to the shared pool: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	person, err := activeMemoryActor(tx, by, true)
	if err != nil {
		return nil, false, err
	}
	current, err := scanMemory(tx.QueryRow(memoryColumns+` WHERE r.seq=? AND r.scope='agent_local' AND r.forgotten_at='' AND r.superseded_by IS NULL`, seq))
	if err != nil {
		return nil, false, err
	}
	current.Scope = MemoryScopeHousehold
	promoted, err := insertMemoryTx(tx, current)
	if err != nil {
		return nil, false, err
	}
	if err := inheritMemoryConstraints(tx, promoted, seq); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`INSERT INTO butler_memory_derivations(derivative,source) VALUES(?,?)`, promoted, seq); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`INSERT INTO memory_scope_events(memory,kind,source_memory,actor_person_id,recorded_at) VALUES(?,'promotion',?,?,?)`, promoted, seq, person, now()); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`UPDATE butler_memory SET superseded_by=? WHERE seq=?`, promoted, seq); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`UPDATE butler_index SET memory=? WHERE memory=?`, promoted, seq); err != nil {
		return nil, false, err
	}
	names, err := json.Marshal(struct {
		Butlers []string `json:"butlers"`
		Cloud   []string `json:"cloud"`
	}{d.Butlers, d.CloudButlers})
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(
		`INSERT INTO butler_pool_disclosures (memory, butlers, left_house, promoted_at, promoted_by)
		 VALUES (?, ?, ?, ?, ?)`,
		d.Memory, string(names), boolInt(d.LeavesHouse), now(), by); err != nil {
		return nil, false, fmt.Errorf("runtimedb: recording a pool disclosure: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("runtimedb: promoting memory to the shared pool: %w", err)
	}
	return gains, leavesHouse, nil
}

// ButlerScopeSummary is what a Console shows about one Butler's reach.
type ButlerScopeSummary struct {
	Butler      string            `json:"butler"`
	Model       string            `json:"model"`
	ModelKind   string            `json:"modelKind"`
	Connections []string          `json:"connections"`
	Narrowed    map[string]string `json:"narrowedTools,omitempty"`
	MemoryCount int               `json:"memoryCount"`
	Since       time.Time         `json:"since,omitzero"`
}

// ButlerScope gathers what one Butler can reach, so a person can answer
// "what does the accounting Butler see" in one look rather than by
// checking three screens.
func (db *DB) ButlerScope(butler string) (ButlerScopeSummary, error) {
	b, err := db.GetButler(butler)
	if err != nil {
		return ButlerScopeSummary{}, err
	}
	s := ButlerScopeSummary{
		Butler: b.Name, Model: b.Model, ModelKind: b.ModelKind, Since: b.CreatedAt,
	}
	if s.Connections, err = db.ButlerConnections(butler); err != nil {
		return ButlerScopeSummary{}, err
	}
	if s.Narrowed, err = db.ButlerToolNarrowings(butler); err != nil {
		return ButlerScopeSummary{}, err
	}
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM butler_memory
		 WHERE butler = ? AND forgotten_at = '' AND superseded_by IS NULL`,
		butler).Scan(&s.MemoryCount); err != nil {
		return ButlerScopeSummary{}, fmt.Errorf("runtimedb: counting a butler's memory: %w", err)
	}
	return s, nil
}
