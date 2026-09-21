package runtimedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
)

type RoutinePlan = plan.RoutineChange

// SubmitRoutinePlan snapshots the exact reviewed behavior. Submission never
// grants execution, and the queue is inserted by the shared plans trigger.
func (db *DB) SubmitRoutinePlan(name, actor, agent, audience, delivery, expectedDocument string) (string, error) {
	tx, err := db.sql.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	person, err := activeMemoryActor(tx, actor, false)
	if err != nil {
		return "", err
	}
	var r RoutinePlan
	var seq int64
	var ownership, previousAudience string
	if err = tx.QueryRow("SELECT seq,name,revision,document,ownership,output_audience,butler FROM household_routines WHERE name=?", name).Scan(&seq, &r.Name, &r.Revision, &r.Document, &ownership, &previousAudience, &r.AssignedButler); err != nil {
		return "", err
	}
	if r.Document != expectedDocument {
		return "", fmt.Errorf("routine changed while preparing the Plan; try again")
	}
	if ownership != RoutineWildstone {
		return "", fmt.Errorf("native routines are managed in their own application")
	}
	if audience == "" || delivery == "" {
		return "", fmt.Errorf("a Plan needs an output audience and delivery behavior")
	}
	var audienceExists int
	if err = tx.QueryRow("SELECT count(*) FROM audiences WHERE audience_id=? AND kind<>'owner'", audience).Scan(&audienceExists); err != nil || audienceExists != 1 {
		return "", fmt.Errorf("unknown output audience")
	}
	var agentSeq any
	if agent != "" {
		var id int64
		if err = tx.QueryRow(`SELECT b.seq FROM butlers b JOIN butler_members p ON p.person_id=?
 WHERE b.name=? AND b.enabled=1 AND b.retired_at='' AND (b.agent_class='member' OR p.role='owner')
 AND (p.role='owner' OR EXISTS(SELECT 1 FROM butler_access a WHERE a.butler=b.seq AND a.person_id=p.person_id AND a.revoked_at=''))`, person, agent).Scan(&id); err != nil {
			return "", fmt.Errorf("Agent is not admitted for this proposal")
		}
		agentSeq = id
	}
	if previousAudience != audience {
		r.Revision++
		if _, err = tx.Exec("UPDATE household_routines SET output_audience=?,revision=?,updated_at=?,updated_by=? WHERE seq=?", audience, r.Revision, now(), actor, seq); err != nil {
			return "", err
		}
		if _, err = tx.Exec("INSERT INTO household_routine_revisions(routine,revision,document,author,created_at,note) VALUES(?,?,?,?,?,?)", seq, r.Revision, r.Document, actor, now(), "output audience changed; requires review"); err != nil {
			return "", err
		}
	}
	r.OutputAudience, r.Delivery = audience, delivery
	r.ProposingAgent = agent
	principal := actor
	if agent != "" {
		principal = "butler"
	}
	raw, err := json.Marshal(map[string]any{"summary": "Activate routine " + name, "routine": r})
	if err != nil {
		return "", err
	}
	res, err := tx.Exec(`INSERT INTO plans(created_at,principal,acting_for,summary,risk,state,plan_json) VALUES(?,?,?,?,?,'proposed',?)`, now(), principal, actor, "Activate routine "+name, "medium", string(raw))
	if err != nil {
		return "", err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", err
	}
	// Agent proposals may contain recalled private content. Until their source
	// history is recorded, keep the snapshot on the trusted Console surface.
	// Mark it in the insertion transaction so no reader sees an unguarded Plan.
	if agent != "" {
		if _, err = tx.Exec("INSERT INTO plan_private_origins(plan) VALUES(?)", id); err != nil {
			return "", err
		}
	}
	if _, err = tx.Exec(`INSERT INTO routine_plan_versions(plan,routine,revision,document,output_audience,delivery,assigned_butler) VALUES(?,?,?,?,?,?,?)`, id, seq, r.Revision, r.Document, audience, delivery, r.AssignedButler); err != nil {
		return "", err
	}
	if _, err = tx.Exec("UPDATE owner_review_queue SET proposing_agent=?,proposer_person_id=? WHERE plan=?", agentSeq, person, id); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return planID(id), nil
}

// ApproveRoutinePlan is the atomic version check and activation. The API
// additionally requires fresh Owner authentication; no Agent calls this seam.
func (db *DB) ApproveRoutinePlan(id, actor string) error {
	seq, err := parsePlanID(id)
	if err != nil {
		return err
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
	var routine int64
	var version, current int
	var document, live, audience, state, assigned, liveAssigned string
	var proposer sql.NullString
	if err = tx.QueryRow(`SELECT v.routine,v.revision,r.revision,v.document,r.document,v.output_audience,p.state,q.proposer_person_id,v.assigned_butler,r.butler
 FROM routine_plan_versions v JOIN household_routines r ON r.seq=v.routine JOIN plans p ON p.seq=v.plan JOIN owner_review_queue q ON q.plan=p.seq WHERE v.plan=?`, seq).Scan(&routine, &version, &current, &document, &live, &audience, &state, &proposer, &assigned, &liveAssigned); err != nil {
		return err
	}
	if version != current || document != live || assigned != liveAssigned || state != "proposed" {
		return fmt.Errorf("this Plan version no longer matches the routine; submit it again")
	}
	if proposer.Valid {
		var active int
		if err = tx.QueryRow("SELECT count(*) FROM butler_members WHERE person_id=? AND admitted=1 AND disabled_at='' AND offboarded_at=''", proposer.String).Scan(&active); err != nil || active != 1 {
			return fmt.Errorf("proposer is no longer admitted")
		}
	}
	if _, err = tx.Exec("UPDATE routine_plan_versions SET approved_by=? WHERE plan=?", person, seq); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE household_routines SET approved_revision=revision,output_audience=?,state='enabled',disabled_at='',disabled_by='',updated_at=?,updated_by=? WHERE seq=?", audience, now(), actor, routine); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE plans SET state='approved',resolved_by=?,resolved_at=? WHERE seq=?", actor, now(), seq); err != nil {
		return err
	}
	// Activation is the complete Plan effect. Resolve it in this transaction,
	// so the infrastructure reconciler never sees an approved routine Plan and
	// mistakes it for permission to perform Medium/High host repairs.
	for _, state := range []PlanState{PlanApplying, PlanApplied} {
		if _, err = tx.Exec("UPDATE plans SET state=? WHERE seq=?", state, seq); err != nil {
			return err
		}
	}
	// Retries may submit the same snapshot more than once. Only the reviewed
	// Plan is applied; equivalent pending versions are now obsolete.
	if _, err = tx.Exec(`UPDATE plans SET state='superseded',resolved_by=?,resolved_at=? WHERE state='proposed' AND seq IN (SELECT plan FROM routine_plan_versions WHERE routine=? AND revision=? AND plan<>?)`, actor, now(), routine, version, seq); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) OwnerReviewQueue(actor string) ([]PlanRecord, error) {
	tx, err := db.sql.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = activeMemoryActor(tx, actor, true); err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT p.seq,p.created_at,p.principal,p.acting_for,p.summary,p.risk,p.state,p.plan_json,p.resolved_by,p.resolved_at FROM plans p JOIN owner_review_queue q ON q.plan=p.seq WHERE p.state='proposed' ORDER BY p.seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlanRecord{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RoutineUsesOwnerReview distinguishes grandfathered legacy authorizations from
// versions governed by the Owner Review Queue and its forthcoming executor.
func (db *DB) RoutineUsesOwnerReview(name string) (bool, error) {
	var managed bool
	err := db.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM routine_plan_versions v WHERE v.routine=r.seq AND v.approved_by IS NOT NULL) FROM household_routines r WHERE r.name=?`, name).Scan(&managed)
	return managed, err
}
