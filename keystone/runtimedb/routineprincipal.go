package runtimedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrRoutineDenied = errors.New("routine version is not authorized")

// RoutinePrincipal is the exact approved Plan, never a Person or Agent grant.
// Its fields are private so callers cannot construct a different principal.
// Every resource use rechecks the live version instead of trusting this value.
type RoutinePrincipal struct {
	db   *DB
	plan int64
}

func (p *RoutinePrincipal) PlanID() string {
	if p == nil {
		return ""
	}
	return planID(p.plan)
}

type routineVersion struct {
	routine   int64
	revision  int
	agent     int64
	agentName string
	model     string
	audience  string
}

func (db *DB) routineVersion(tx *sql.Tx, p *RoutinePrincipal) (routineVersion, error) {
	var v routineVersion
	if p == nil || p.db != db || p.plan <= 0 {
		return v, ErrRoutineDenied
	}
	var assigned, currentAssigned, ownership, state, planState, signature, secretJSON, memoryJSON string
	var approved sql.NullString
	err := tx.QueryRow(`SELECT v.routine,v.revision,v.assigned_butler,r.butler,r.ownership,r.state,p.state,
 v.approved_by,v.output_audience,COALESCE(b.seq,0),COALESCE(m.kind,''),r.resource_signature,v.secret_ids,v.memory_ids
 FROM routine_plan_versions v JOIN household_routines r ON r.seq=v.routine
 JOIN plans p ON p.seq=v.plan
 LEFT JOIN butlers b ON b.name=v.assigned_butler AND b.enabled=1 AND b.retired_at=''
 LEFT JOIN models m ON m.seq=b.model AND m.enabled=1
 WHERE v.plan=? AND r.revision=v.revision AND r.approved_revision=v.revision
 AND r.document=v.document AND r.output_audience=v.output_audience
 AND r.disabled_at='' AND r.revoked_at='' AND r.deleted_at='' AND r.paused_reason=''`, p.plan).Scan(&v.routine, &v.revision, &assigned, &currentAssigned, &ownership, &state, &planState, &approved, &v.audience, &v.agent, &v.model, &signature, &secretJSON, &memoryJSON)
	if err != nil || assigned != currentAssigned || ownership != RoutineWildstone || state != RoutineEnabled || planState != string(PlanApplied) || !approved.Valid || approved.String == "" {
		return routineVersion{}, ErrRoutineDenied
	}
	v.agentName = assigned
	if assigned != "" && (v.agent == 0 || v.model == "") {
		return routineVersion{}, ErrRoutineDenied
	}
	var resources RoutineResources
	if json.Unmarshal([]byte(secretJSON), &resources.SecretIDs) != nil || json.Unmarshal([]byte(memoryJSON), &resources.MemoryIDs) != nil {
		return routineVersion{}, ErrRoutineDenied
	}
	_, expected, err := normalizeRoutineResources(resources)
	if err != nil || signature != expected {
		return routineVersion{}, ErrRoutineDenied
	}
	return v, nil
}

// BindRoutinePrincipal is called by the trusted executor with the approved
// Plan ID. Possessing the value alone does not preserve authority after edit,
// disable, Agent retirement or policy change.
func (db *DB) BindRoutinePrincipal(id string) (*RoutinePrincipal, error) {
	plan, err := parsePlanID(id)
	if err != nil {
		return nil, ErrRoutineDenied
	}
	p := &RoutinePrincipal{db: db, plan: plan}
	tx, err := db.sql.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = db.routineVersion(tx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// CheckRoutineVersion supplies §10.2's first leg at every execution boundary.
// The assigned Agent is checked here, before resource grants can be evaluated.
func (db *DB) CheckRoutineVersion(p *RoutinePrincipal, agent string) error {
	tx, err := db.sql.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	v, err := db.routineVersion(tx, p)
	if err != nil {
		return err
	}
	if agent != v.agentName {
		return ErrRoutineDenied
	}
	return nil
}

func (db *DB) routineMemoryIDs(p *RoutinePrincipal, destination MemoryDestination, limit int, only int64) ([]int64, routineVersion, error) {
	var empty routineVersion
	if err := db.gate(GateRetrieval); err != nil {
		return nil, empty, err
	}
	if destination.Group || (destination.BuiltIn && destination.ConnectionRef != 0) ||
		(!destination.BuiltIn && destination.ConnectionRef <= 0) {
		return nil, empty, ErrRoutineDenied
	}
	tx, err := db.sql.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, empty, err
	}
	defer tx.Rollback()
	v, err := db.routineVersion(tx, p)
	if err != nil {
		return nil, empty, err
	}
	if v.agent == 0 {
		return nil, empty, ErrRoutineDenied
	}
	outputPeople, err := db.ResolveAudience(v.audience, "")
	if err != nil {
		return nil, empty, err
	}
	if len(outputPeople) == 0 {
		return nil, empty, ErrRoutineDenied
	}
	if !destination.BuiltIn {
		// The trusted executor supplies the resolved recipient. Recheck the
		// live binding, Agent, admission and disclosure, even for zero grants.
		allowed := false
		for _, person := range outputPeople {
			if person == destination.RecipientPerson {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, empty, ErrRoutineDenied
		}
		var live int
		err = tx.QueryRow(`SELECT 1 FROM butler_addresses a
 JOIN chat_bindings binding ON binding.connection=a.seq AND binding.revoked_at=''
 JOIN butler_members recipient ON recipient.person_id=binding.person_id
 JOIN connection_admission admission ON admission.connection=a.seq
 JOIN connection_disclosure disclosure ON disclosure.connection=a.seq
 WHERE a.seq=? AND a.revoked_at='' AND binding.person_id=?
 AND recipient.admitted=1 AND recipient.disabled_at='' AND recipient.offboarded_at=''
 AND EXISTS(SELECT 1 FROM butler_access access WHERE access.person_id=recipient.person_id
            AND access.butler=a.butler AND access.revoked_at='')
 AND admission.subject_kind='person'
 AND (admission.admits='household' OR (admission.admits='owners' AND recipient.role='owner'))
 AND disclosure.reach IN ('local','external')
 AND disclosure.encryption IN ('none','transport','end_to_end')
 AND disclosure.retention IN ('none','provider')
 AND disclosure.surface IN ('trusted_ui','external_messaging') LIMIT 1`,
			destination.ConnectionRef, destination.RecipientPerson).Scan(&live)
		if err != nil {
			return nil, empty, ErrRoutineDenied
		}
	}
	// Query IDs only. Audience and inherited restrictions are checked for
	// every output recipient before any content enters the process.
	query := `SELECT m.seq FROM routine_memory_grants g JOIN butler_memory m ON m.seq=g.memory
 WHERE g.plan=? AND m.forgotten_at='' AND m.superseded_by IS NULL
 AND m.scope IN ('household','agent_local')
 AND (m.scope<>'agent_local' OR m.butler=?)
 AND NOT EXISTS(SELECT 1 FROM memory_agent_restrictions mr WHERE mr.agent=? AND mr.scope=m.scope)
 AND NOT EXISTS(WITH RECURSIVE sources(seq) AS (
   SELECT d.source FROM butler_memory_derivations d WHERE d.derivative=m.seq
   UNION SELECT d.source FROM butler_memory_derivations d JOIN sources s ON d.derivative=s.seq
 ) SELECT 1 FROM sources s JOIN butler_memory ancestor ON ancestor.seq=s.seq WHERE ancestor.scope='personal')`
	args := []any{p.plan, v.agentName, v.agent}
	if only != 0 {
		query += " AND m.seq=?"
		args = append(args, only)
	}
	for _, person := range outputPeople {
		visible, visibleArgs, e := db.visibleAudienceClause("m.audience", "m.owner_person_id", person)
		if e != nil {
			return nil, empty, e
		}
		inherited, inheritedArgs, e := db.memoryConstraintClause("m", person)
		if e != nil {
			return nil, empty, e
		}
		query += " AND " + visible + " AND " + inherited
		args = append(args, visibleArgs...)
		args = append(args, inheritedArgs...)
	}
	if !destination.BuiltIn {
		disclosure, disclosureArgs := memoryDisclosureClause("m", &MemoryReadContext{
			person: destination.RecipientPerson, agent: v.agent, destination: destination,
		})
		query += disclosure
		args = append(args, disclosureArgs...)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query += " ORDER BY m.seq DESC LIMIT ?"
	args = append(args, limit)
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, empty, fmt.Errorf("routine memory candidates: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, empty, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, empty, err
	}
	return ids, v, nil
}

// RetrieveRoutineMemory assembles only exact-version granted content whose
// audience includes everyone the routine is approved to address.
func (db *DB) RetrieveRoutineMemory(p *RoutinePrincipal, destination MemoryDestination, limit int) ([]MemoryRecord, error) {
	ids, _, err := db.routineMemoryIDs(p, destination, limit, 0)
	if err != nil {
		return nil, err
	}
	out := make([]MemoryRecord, 0, len(ids))
	for _, id := range ids {
		// The candidate and content reads are separate. Recheck the full
		// resource predicate so a revocation between them cannot admit it.
		model, e := db.routineMemoryModel(p, id, destination)
		if e != nil {
			err = e
			if errors.Is(err, ErrPrivacy) {
				continue
			}
			return nil, err
		}
		// Re-evaluate before loading content so a concurrent version change
		// cannot turn a stale candidate list into authorized prompt context.
		m, e := db.GetMemory(id)
		if e != nil {
			return nil, e
		}
		if !m.ForgottenAt.IsZero() || m.SupersededBy != 0 {
			return nil, ErrRoutineDenied
		}
		m.Privacy, err = db.MemoryPrivacy(id)
		if err != nil {
			return nil, err
		}
		if err = m.Privacy.Check(model, destination); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// CheckRoutineMemory is the per-resource leg used by a routine tool before
// dispatch. Retrieval uses the same candidate filter before loading content.
func (db *DB) CheckRoutineMemory(p *RoutinePrincipal, seq int64, destination MemoryDestination) error {
	_, err := db.routineMemoryModel(p, seq, destination)
	return err
}

func (db *DB) routineMemoryModel(p *RoutinePrincipal, seq int64, destination MemoryDestination) (string, error) {
	if seq <= 0 {
		return "", ErrRoutineDenied
	}
	ids, v, err := db.routineMemoryIDs(p, destination, 1, seq)
	if err != nil {
		return "", err
	}
	if len(ids) != 1 {
		return "", ErrRoutineDenied
	}
	return v.model, db.CheckMemoryPrivacy(seq, v.model, destination)
}
