package runtimedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrHouseholdStopped = errors.New("household execution is stopped by an Owner")

type HouseholdExecutionStop struct {
	Active     bool   `json:"active"`
	Generation int64  `json:"generation"`
	EnteredAt  string `json:"enteredAt,omitempty"`
	EnteredBy  string `json:"enteredBy,omitempty"`
	Reason     string `json:"reason,omitempty"`
	ReleasedAt string `json:"releasedAt,omitempty"`
	ReleasedBy string `json:"releasedBy,omitempty"`
}

func (db *DB) GetHouseholdExecutionStop() (HouseholdExecutionStop, error) {
	return getHouseholdExecutionStop(db.sql)
}

func getHouseholdExecutionStop(q queryRower) (HouseholdExecutionStop, error) {
	var s HouseholdExecutionStop
	var active int
	err := q.QueryRow(`SELECT active,generation,entered_at,entered_by,reason,released_at,released_by
 FROM household_execution_stop WHERE singleton=1`).Scan(&active, &s.Generation,
		&s.EnteredAt, &s.EnteredBy, &s.Reason, &s.ReleasedAt, &s.ReleasedBy)
	s.Active = active != 0
	return s, err
}

func activeOwnerForStop(tx *sql.Tx, actor string) (string, error) {
	person, err := resolvePersonID(tx, actor)
	if err != nil {
		return "", ErrRoutineDenied
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM butler_members
 WHERE person_id=? AND role='owner' AND admitted=1 AND disabled_at='' AND offboarded_at=''`, person).Scan(&n); err != nil || n != 1 {
		return "", ErrRoutineDenied
	}
	return person, nil
}

// StopHouseholdExecution is an immediate Owner operation, not a Plan. Every
// effectful store/gateway boundary reads this switch live.
func (db *DB) StopHouseholdExecution(actor, reason string) error {
	if reason == "" {
		return fmt.Errorf("a household stop needs a reason")
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
	stamp := now()
	if _, err := tx.Exec(`UPDATE household_execution_stop SET active=1,generation=generation+1,
 entered_at=?,entered_by=?,reason=?,released_at='',released_by='' WHERE singleton=1`,
		stamp, person, reason); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO household_execution_stop_events(generation,action,at,actor,reason)
 SELECT generation,'stop',?,?,? FROM household_execution_stop WHERE singleton=1`, stamp, person, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// ResumeHouseholdExecution releases only the incident stop. If restore
// quarantine also holds, its journal-replay release remains mandatory.
func (db *DB) ResumeHouseholdExecution(actor string) error {
	tx, err := db.sql.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	person, err := activeOwnerForStop(tx, actor)
	if err != nil {
		return err
	}
	stamp := now()
	res, err := tx.Exec(`UPDATE household_execution_stop SET active=0,generation=generation+1,
 released_at=?,released_by=? WHERE singleton=1 AND active=1`, stamp, person)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`INSERT INTO household_execution_stop_events(generation,action,at,actor,reason)
 SELECT generation,'resume',?,?,reason FROM household_execution_stop WHERE singleton=1`, stamp, person); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) checkHouseholdStop(what string) error {
	return checkHouseholdStop(db.sql, what)
}

func checkHouseholdStop(q queryRower, what string) error {
	stop, err := getHouseholdExecutionStop(q)
	if err != nil {
		return fmt.Errorf("%w: cannot check before %s: %v", ErrHouseholdStopped, what, err)
	}
	if stop.Active {
		return fmt.Errorf("%w: %s (%s)", ErrHouseholdStopped, what, stop.Reason)
	}
	return nil
}
