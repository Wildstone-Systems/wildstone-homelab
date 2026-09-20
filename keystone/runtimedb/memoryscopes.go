package runtimedb

import (
	"database/sql"
	"fmt"
)

const (
	MemoryScopePersonal   = "personal"
	MemoryScopeHousehold  = "household"
	MemoryScopeAgentLocal = "agent_local"
)

func (db *DB) AgentMemoryRestrictions(agent string) ([]string, error) {
	rows, err := db.sql.Query(`SELECT r.scope FROM memory_agent_restrictions r JOIN butlers b ON b.seq=r.agent WHERE b.name=? ORDER BY r.scope`, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, err
		}
		out = append(out, scope)
	}
	return out, rows.Err()
}

func (db *DB) SetAgentMemoryRestriction(agent, scope string, restricted bool, actor string) error {
	if scope != MemoryScopePersonal && scope != MemoryScopeHousehold {
		return fmt.Errorf("only Personal and Household defaults can be restricted")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := activeMemoryActor(tx, actor, true); err != nil {
		return err
	}
	var id int64
	if err := tx.QueryRow(`SELECT seq FROM butlers WHERE name=? AND retired_at=''`, agent).Scan(&id); err != nil {
		return ErrNotFound
	}
	if restricted {
		_, err = tx.Exec(`INSERT INTO memory_agent_restrictions(agent,scope) VALUES(?,?) ON CONFLICT DO NOTHING`, id, scope)
	} else {
		_, err = tx.Exec(`DELETE FROM memory_agent_restrictions WHERE agent=? AND scope=?`, id, scope)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func defaultMemoryScope(m *MemoryRecord) {
	if m.Scope != "" {
		return
	}
	if m.Owner == HouseholdOwner || m.Audience == ButlerAudienceShared {
		m.Scope = MemoryScopeHousehold
	} else {
		m.Scope = MemoryScopePersonal
	}
}

// Scope and Agent restrictions narrow the independent audience predicate.
// Butler on Personal/Household records is provenance, not an access boundary.
func memoryScopeClause(alias, agent, person string) (string, []any) {
	clause := fmt.Sprintf(" AND (%s.scope <> 'personal' OR %s.owner_person_id = ?)", alias, alias)
	args := []any{person}
	if agent != AllButlers {
		clause += fmt.Sprintf(" AND (%s.scope <> 'agent_local' OR (%s.butler = ? AND ? <> ''))", alias, alias)
		args = append(args, agent, agent)
		clause += fmt.Sprintf(" AND NOT EXISTS (SELECT 1 FROM memory_agent_restrictions mr JOIN butlers mb ON mb.seq = mr.agent WHERE mb.name = ? AND mr.scope = %s.scope)", alias)
		args = append(args, agent)
	}
	return clause, args
}

func (db *DB) memoryConstraintClause(alias, person string) (string, []any, error) {
	visible, args, err := db.visibleAudienceClause("mc.audience", "mc.owner_person_id", person)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM memory_constraints mc WHERE mc.memory = %s.seq AND NOT COALESCE(%s, 0))", alias, visible), args, nil
}

func inheritMemoryConstraints(tx *sql.Tx, target, source int64) error {
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM butler_memory WHERE seq=? AND forgotten_at='' AND superseded_by IS NULL`, source).Scan(&active); err != nil {
		return err
	}
	if active != 1 {
		return fmt.Errorf("source memory is no longer active")
	}
	_, err := tx.Exec(`INSERT INTO memory_constraints(memory, audience, owner_person_id)
 SELECT ?, audience, owner_person_id FROM butler_memory WHERE seq = ?
 UNION SELECT ?, 'aud_owner', owner_person_id FROM butler_memory WHERE seq=? AND scope='personal'
 UNION SELECT ?, audience, owner_person_id FROM memory_constraints WHERE memory = ?`, target, source, target, source, target, source)
	return err
}

// Promotion creates a new record with an approval receipt and a derivation
// link. Forgetting the source therefore also suppresses the approved copy.
func (db *DB) PromotePersonalMemory(seq int64, actor string) (int64, error) {
	if err := db.gate(GateIngestion); err != nil {
		return 0, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	person, err := activeMemoryActor(tx, actor, false)
	if err != nil {
		return 0, err
	}
	m, err := scanMemory(tx.QueryRow(memoryColumns+` WHERE r.seq=? AND r.owner_person_id=? AND r.scope='personal' AND r.forgotten_at='' AND r.superseded_by IS NULL`, seq, person))
	if err != nil {
		return 0, err
	}
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM memory_scope_events WHERE source_memory=? AND kind='promotion'`, seq).Scan(&exists); err != nil {
		return 0, err
	}
	if exists != 0 {
		return 0, fmt.Errorf("memory already promoted")
	}
	// Approval can release only this Person's private constraint. It cannot
	// consent for a different Person or silently widen inherited group rules.
	var foreign int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM memory_constraints WHERE memory=? AND audience='aud_owner' AND (owner_person_id IS NULL OR owner_person_id<>?)`, seq, person).Scan(&foreign); err != nil {
		return 0, err
	}
	if foreign != 0 {
		return 0, fmt.Errorf("promotion requires every source owner's approval")
	}
	m.Scope = MemoryScopeHousehold
	if m.Audience == ButlerAudiencePrivate {
		m.Audience = ButlerAudienceShared
	}
	m.Owner = HouseholdOwner
	promoted, err := insertMemoryTx(tx, m)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`INSERT INTO memory_constraints(memory,audience,owner_person_id) SELECT ?,audience,owner_person_id FROM memory_constraints WHERE memory=? AND audience<>'aud_owner'`, promoted, seq); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`INSERT INTO butler_memory_derivations(derivative,source) VALUES(?,?)`, promoted, seq); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`INSERT INTO memory_scope_events(memory,kind,source_memory,actor_person_id,recorded_at) VALUES(?,'promotion',?,?,?)`, promoted, seq, person, now()); err != nil {
		return 0, err
	}
	if m.Audience == ButlerAudienceShared {
		if err := digestTx(tx, promoted, MemoryRecorded, "shared: "+m.Content, actor, false); err != nil {
			return 0, err
		}
	}
	// Index inside the transaction so the approved record is immediately
	// searchable. SearchIndex still applies all authoritative constraints.
	if _, err = tx.Exec(`INSERT INTO butler_index(source_kind,source_id,memory,owner_person_id,audience,permission_label,content,observed_at,indexed_at,butler)
 VALUES(?,?,?,NULL,?,?,?,?,?,?)`, m.SourceKind, m.SourceID, promoted, m.Audience, m.PermissionLabel, m.Content, timeText(m.ObservedAt), now(), m.Butler); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return promoted, nil
}

func activeMemoryActor(tx *sql.Tx, actor string, ownerOnly bool) (string, error) {
	var person, role string
	err := tx.QueryRow(`SELECT person_id,role FROM butler_members WHERE member=? COLLATE NOCASE AND admitted=1 AND disabled_at='' AND offboarded_at=''`, actor).Scan(&person, &role)
	if err != nil {
		return "", ErrNotAdmitted
	}
	if ownerOnly && role != RoleOwner {
		return "", ErrNotAdmitted
	}
	return person, nil
}

// Retirement is the current deletion lifecycle. Transfer local memory and
// retire atomically, retaining source/derivation metadata and an audit receipt.
func (db *DB) RetireButlerWithMemoryTransfer(source, destination, actor string) error {
	if err := db.gate(GateIngestion); err != nil {
		return err
	}
	if source == destination || destination == "" {
		return fmt.Errorf("choose a different destination Agent")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	person, err := activeMemoryActor(tx, actor, true)
	if err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM butlers WHERE name=? AND retired_at=''`, destination).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	res, err := tx.Exec(`UPDATE butlers SET retired_at=?,retired_by=?,enabled=0,updated_at=?,updated_by=? WHERE name=? AND retired_at=''`, now(), actor, now(), actor, source)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	if _, err = tx.Exec(`INSERT INTO memory_scope_events(memory,kind,source_agent,destination_agent,actor_person_id,recorded_at)
 SELECT seq,'transfer',?,?,?,? FROM butler_memory WHERE scope='agent_local' AND butler=?`, source, destination, person, now(), source); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE butler_memory SET butler=? WHERE scope='agent_local' AND butler=?`, destination, source); err != nil {
		return err
	}
	return tx.Commit()
}

func suppressMemoryDescendants(tx *sql.Tx, root int64, actor string) error {
	rows, err := tx.Query(`WITH RECURSIVE descendants(seq) AS (
 SELECT derivative FROM butler_memory_derivations WHERE source=?
 UNION SELECT d.derivative FROM butler_memory_derivations d JOIN descendants a ON d.source=a.seq)
 SELECT source_kind,source_id,content FROM butler_memory WHERE seq IN (SELECT seq FROM descendants)`, root)
	if err != nil {
		return err
	}
	type source struct{ kind, id, content string }
	var sources []source
	for rows.Next() {
		var s source
		if err := rows.Scan(&s.kind, &s.id, &s.content); err != nil {
			rows.Close()
			return err
		}
		sources = append(sources, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, s := range sources {
		if _, err := tx.Exec(`INSERT INTO butler_memory_suppressed_sources(source_kind,source_id,content_key,suppressed_at,suppressed_by) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, s.kind, s.id, contentKey(s.content), now(), actor); err != nil {
			return err
		}
	}
	return nil
}
