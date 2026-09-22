package runtimedb

import (
	"context"
	"fmt"
)

// PurgeDeletedRoutine irreversibly erases a soft-deleted routine's payloads.
// Its identity, deletion tombstone and effect receipts remain for audit.
// The API requires fresh Console authentication; this store also requires
// an active Owner in the same transaction.
func (db *DB) PurgeDeletedRoutine(name, actor string) error {
	tx, err := db.sql.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := activeOwnerForStop(tx, actor); err != nil {
		return err
	}
	var seq int64
	if err := tx.QueryRow(`SELECT seq FROM household_routines
 WHERE name=? AND ownership='wildstone' AND deleted_at<>'' AND purged_at=''`, name).Scan(&seq); err != nil {
		return fmt.Errorf("runtimedb: routine must be archived before purge: %w", err)
	}
	stamp := now()
	if _, err := tx.Exec(`UPDATE household_routines SET purged_at=?,document='[purged]',resource_signature='',paused_reason='',updated_at=?,updated_by=? WHERE seq=?`, stamp, stamp, actor, seq); err != nil {
		return err
	}
	for _, statement := range []string{
		`UPDATE household_routine_revisions SET document='[purged]',note='[purged]' WHERE routine=?`,
		`UPDATE routine_plan_versions SET document='[purged]',secret_ids='[]',memory_ids='[]' WHERE routine=?`,
		`UPDATE plans SET plan_json='{}',summary='[purged routine]',risk='low',state=CASE WHEN state='proposed' THEN 'superseded' ELSE state END WHERE seq IN (SELECT plan FROM routine_plan_versions WHERE routine=?)`,
		`DELETE FROM household_secret_routine_grants WHERE plan IN (SELECT plan FROM routine_plan_versions WHERE routine=?)`,
		`DELETE FROM routine_memory_grants WHERE plan IN (SELECT plan FROM routine_plan_versions WHERE routine=?)`,
	} {
		if _, err := tx.Exec(statement, seq); err != nil {
			return fmt.Errorf("runtimedb: purging routine payloads: %w", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO deletion_journal(subject_kind,subject_id,action,at,actor,detail)
 VALUES('routine',?,'deleted',?,?,'payload purged; audit tombstone and effect receipts retained')`, name, stamp, actor); err != nil {
		return err
	}
	return tx.Commit()
}
