package runtimedb

import (
	"context"
	"database/sql"
)

// ChangeReviewedRoutine is an immediate Owner control. Revocation fences the
// current approved revision; only approval of a later revision can clear it.
func (db *DB) ChangeReviewedRoutine(name, actor, action string) error {
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
	var result sql.Result
	switch action {
	case "disable":
		result, err = tx.Exec(`UPDATE household_routines SET state='disabled',disabled_at=?,disabled_by=?,updated_at=?,updated_by=?
   WHERE name=? AND ownership='wildstone' AND deleted_at=''`, stamp, person, stamp, person, name)
	case "enable":
		result, err = tx.Exec(`UPDATE household_routines SET state='enabled',disabled_at='',disabled_by='',updated_at=?,updated_by=?
   WHERE name=? AND ownership='wildstone' AND state='disabled' AND deleted_at='' AND revoked_at='' AND paused_reason=''
   AND revision=approved_revision`, stamp, person, name)
	case "revoke":
		result, err = tx.Exec(`UPDATE household_routines SET state='disabled',disabled_at=?,disabled_by=?,revoked_at=?,revoked_revision=revision,updated_at=?,updated_by=?
   WHERE name=? AND ownership='wildstone' AND deleted_at=''`, stamp, person, stamp, stamp, person, name)
	case "delete":
		result, err = tx.Exec(`UPDATE household_routines SET state='disabled',disabled_at=?,disabled_by=?,deleted_at=?,updated_at=?,updated_by=?
   WHERE name=? AND ownership='wildstone' AND deleted_at=''`, stamp, person, stamp, stamp, person, name)
	case "restore":
		result, err = tx.Exec(`UPDATE household_routines SET deleted_at='',updated_at=?,updated_by=?
 WHERE name=? AND ownership='wildstone' AND deleted_at<>''`, stamp, person, name)
	case "resolve-recipient":
		result, err = tx.Exec(`UPDATE household_routines SET paused_reason='',updated_at=?,updated_by=?
   WHERE name=? AND ownership='wildstone' AND deleted_at='' AND paused_reason<>''`, stamp, person, name)
	default:
		return ErrRoutineDenied
	}
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrRoutineDenied
	}
	return tx.Commit()
}

// PauseRoutineRecipient records a revoked destination as a routine-wide hold.
// It requires explicit Owner resolution and never heals on a capability poll.
func (db *DB) PauseRoutineRecipient(name string) error {
	result, err := db.sql.Exec(`UPDATE household_routines SET paused_reason='delivery recipient is no longer admitted',updated_at=?
 WHERE name=? AND ownership='wildstone' AND deleted_at='' AND paused_reason=''`, now(), name)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		var paused string
		if err := db.sql.QueryRow("SELECT paused_reason FROM household_routines WHERE name=? AND ownership='wildstone' AND deleted_at=''", name).Scan(&paused); err == nil && paused != "" {
			return nil
		}
		return ErrRoutineDenied
	}
	return nil
}
