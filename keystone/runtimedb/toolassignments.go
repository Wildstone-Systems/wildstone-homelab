package runtimedb

// What an Agent is assigned (ADR-W033 design §6.2, §6.3, §6.4).
//
// A governed Agent reaches exactly what it is assigned: an explicit tool
// by name, or a tool a live bundle covers. Every Agent is governed —
// new ones when they are created, ones that predate 0059 on the first
// start afterwards — so the unenrolled state is a window during an
// upgrade rather than a second model anybody runs on.
//
// It still has to behave correctly, because that window is real: an
// unenrolled Agent reaches what it always did. Failing open there is
// deliberate, and it is the only place in this file that does.
//
// OPT-IN NEVER ARRIVES BY BUNDLE. §6.2 exists so an unusually
// consequential tool can sit inside a service without being swept up by
// "all Member tools for X", and §6.4 keeps it out of a new Agent's
// defaults for the same reason. It can still be assigned EXPLICITLY, by
// name, one tool at a time — which is the whole point: somebody has to
// mean it.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// isNoRows is sql.ErrNoRows, asked the way the rest of this package
// does. A local helper because these lookups ask it five times and a
// misspelled errors.Is would read as "no row" forever.
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// ToolAssignment is one explicitly named tool.
type ToolAssignment struct {
	Seq        int64     `json:"seq"`
	Butler     int64     `json:"butler"`
	Tool       string    `json:"tool"`
	AssignedAt time.Time `json:"assignedAt"`
	AssignedBy string    `json:"assignedBy"`
}

// ToolBundle is a service and a class, standing.
type ToolBundle struct {
	Seq     int64  `json:"seq"`
	Butler  int64  `json:"butler"`
	Service string `json:"service"`
	// AgentClass is the class of the TOOLS covered, not the Agent's.
	AgentClass string    `json:"agentClass"`
	GrantedAt  time.Time `json:"grantedAt"`
	GrantedBy  string    `json:"grantedBy"`
}

// ServiceSystem is the service Keystone's own tools belong to.
//
// Named rather than left empty so a bundle over it is expressible and
// looks like any other, and so an empty service in a query is a missing
// value rather than an accidental match on the core tools.
const ServiceSystem = "system"

// GovernAgent enrolls an Agent in the assigned-tools model.
//
// One way only, and deliberately: there is no UngovernAgent. Taking an
// Agent back out would hand it everything its gateway registers, which
// is the opposite of what anybody enrolling it wanted, and would make
// "remove the last assignment" a way to widen an Agent by accident.
func (db *DB) GovernAgent(butler int64, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: governing an agent needs who did it")
	}
	if _, err := db.GetButlerBySeq(butler); err != nil {
		return err
	}
	if _, err := db.sql.Exec(
		`INSERT INTO butler_tool_governance (butler, since, enrolled_by)
		 VALUES (?, ?, ?) ON CONFLICT(butler) DO NOTHING`,
		butler, now(), by); err != nil {
		return fmt.Errorf("runtimedb: governing agent %d: %w", butler, err)
	}
	return nil
}

// AgentIsGoverned reports whether this Agent is default-deny.
func (db *DB) AgentIsGoverned(butler int64) (bool, error) {
	var one int
	err := db.sql.QueryRow(
		`SELECT 1 FROM butler_tool_governance WHERE butler = ?`, butler).Scan(&one)
	switch {
	case err == nil:
		return true, nil
	case isNoRows(err):
		return false, nil
	default:
		return false, fmt.Errorf("runtimedb: reading governance for %d: %w", butler, err)
	}
}

// AssignTool gives one named tool to one Agent.
//
// An opt-in tool is allowed here, and only here. §6.2 keeps it out of
// bundles and defaults; naming it explicitly is how somebody says they
// meant it.
func (db *DB) AssignTool(butler int64, tool, by string) error {
	if tool == "" || by == "" {
		return fmt.Errorf("runtimedb: assigning a tool needs a tool and who assigned it")
	}
	if _, err := db.GetButlerBySeq(butler); err != nil {
		return err
	}
	if _, err := db.sql.Exec(
		`INSERT INTO butler_tool_assignments (butler, tool, assigned_at, assigned_by)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (butler, tool) WHERE revoked_at = '' DO NOTHING`,
		butler, tool, now(), by); err != nil {
		return fmt.Errorf("runtimedb: assigning %q: %w", tool, err)
	}
	return nil
}

// UnassignTool takes one named tool away.
func (db *DB) UnassignTool(butler int64, tool, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: unassigning a tool needs who did it")
	}
	_, err := db.sql.Exec(
		`UPDATE butler_tool_assignments SET revoked_at = ?, revoked_by = ?
		 WHERE butler = ? AND tool = ? AND revoked_at = ''`, now(), by, butler, tool)
	if err != nil {
		return fmt.Errorf("runtimedb: unassigning %q: %w", tool, err)
	}
	return nil
}

// GrantToolBundle gives an Agent a service/class bundle.
func (db *DB) GrantToolBundle(butler int64, service, class, by string) error {
	if service == "" || by == "" {
		return fmt.Errorf("runtimedb: a bundle needs a service and who granted it")
	}
	if !ValidAgentClass(class) {
		return fmt.Errorf("runtimedb: a bundle names an agent class, must be %s|%s, got %q (design §6.3)",
			AgentClassOwner, AgentClassMember, class)
	}
	b, err := db.GetButlerBySeq(butler)
	if err != nil {
		return err
	}
	// A Member Agent cannot hold an owner-class bundle, for the same
	// reason it cannot hold an owner-class grant: the tools are not on
	// its gateway, so the bundle would be a permission covering nothing
	// — and one that silently became real if the Agent were ever
	// upgraded.
	if !AgentClassAdmits(b.AgentClass, class) {
		return fmt.Errorf(
			"runtimedb: cannot give %s a %s bundle, which is a %s agent (design §6.1)",
			b.Name, class, b.AgentClass)
	}
	if _, err := db.sql.Exec(
		`INSERT INTO butler_tool_bundles (butler, service, agent_class, granted_at, granted_by)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (butler, service, agent_class) WHERE revoked_at = '' DO NOTHING`,
		butler, service, class, now(), by); err != nil {
		return fmt.Errorf("runtimedb: granting bundle %s/%s: %w", service, class, err)
	}
	return nil
}

// RevokeToolBundle withdraws one bundle.
func (db *DB) RevokeToolBundle(butler int64, service, class, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: revoking a bundle needs who did it")
	}
	_, err := db.sql.Exec(
		`UPDATE butler_tool_bundles SET revoked_at = ?, revoked_by = ?
		 WHERE butler = ? AND service = ? AND agent_class = ? AND revoked_at = ''`,
		now(), by, butler, service, class)
	if err != nil {
		return fmt.Errorf("runtimedb: revoking bundle %s/%s: %w", service, class, err)
	}
	return nil
}

// ListToolAssignments returns one Agent's live explicit assignments.
func (db *DB) ListToolAssignments(butler int64) ([]ToolAssignment, error) {
	rows, err := db.sql.Query(
		`SELECT seq, butler, tool, assigned_at, assigned_by
		 FROM butler_tool_assignments WHERE butler = ? AND revoked_at = '' ORDER BY tool`, butler)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing assignments: %w", err)
	}
	defer rows.Close()
	var out []ToolAssignment
	for rows.Next() {
		var a ToolAssignment
		var at string
		if err := rows.Scan(&a.Seq, &a.Butler, &a.Tool, &at, &a.AssignedBy); err != nil {
			return nil, err
		}
		if a.AssignedAt, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListToolBundles returns one Agent's live bundles.
func (db *DB) ListToolBundles(butler int64) ([]ToolBundle, error) {
	rows, err := db.sql.Query(
		`SELECT seq, butler, service, agent_class, granted_at, granted_by
		 FROM butler_tool_bundles WHERE butler = ? AND revoked_at = ''
		 ORDER BY service, agent_class`, butler)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing bundles: %w", err)
	}
	defer rows.Close()
	var out []ToolBundle
	for rows.Next() {
		var b ToolBundle
		var at string
		if err := rows.Scan(&b.Seq, &b.Butler, &b.Service, &b.AgentClass, &at, &b.GrantedBy); err != nil {
			return nil, err
		}
		if b.GrantedAt, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// assignedTo answers whether a governed Agent may reach one tool.
//
// The order is the design's, and the two halves answer different
// questions:
//
//   - an EXPLICIT assignment names this tool, so it is allowed whatever
//     else is true — including an opt-in tool, which is the only way one
//     is ever reached.
//   - a BUNDLE covers the tool's service and class, and the tool is not
//     opt-in. This is the half that absorbs tools nobody has heard of
//     yet, so it is also the half that must never absorb an opt-in one.
func (db *DB) assignedTo(butler int64, q ToolQuery) (bool, string, error) {
	var one int
	err := db.sql.QueryRow(
		`SELECT 1 FROM butler_tool_assignments
		 WHERE butler = ? AND tool = ? AND revoked_at = ''`, butler, q.Tool).Scan(&one)
	switch {
	case err == nil:
		return true, "assignment", nil
	case isNoRows(err):
	default:
		return false, "", fmt.Errorf("runtimedb: reading assignments: %w", err)
	}

	if q.OptIn {
		// §6.2. Not "no bundle covered it" — no bundle CAN. Reported
		// separately so the reason a person reads says which rule
		// refused, and does not send them looking for a bundle to add.
		return false, "opt-in", nil
	}
	if q.Service == "" || q.ToolClass == "" {
		// A tool whose service or class the caller did not supply
		// cannot be matched against a bundle, and guessing would be
		// guessing in the widening direction. Fail closed.
		return false, "unclassified", nil
	}
	err = db.sql.QueryRow(
		`SELECT 1 FROM butler_tool_bundles
		 WHERE butler = ? AND service = ? AND agent_class = ? AND revoked_at = ''`,
		butler, q.Service, q.ToolClass).Scan(&one)
	switch {
	case err == nil:
		return true, "bundle", nil
	case isNoRows(err):
		return false, "unassigned", nil
	default:
		return false, "", fmt.Errorf("runtimedb: reading bundles: %w", err)
	}
}
