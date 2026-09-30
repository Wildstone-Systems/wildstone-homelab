package runtimedb

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

var workspaceSnapshotFunctionID atomic.Uint64

// A test-only SQL function commits the competing policy update while the
// public delivery gate is reading the role. WAL lets the writer commit while
// that reader retains its snapshot. No sleeps or scheduler timing are involved.
func TestWorkspaceDeliveryAdmissionUsesOneSnapshot(t *testing.T) {
	db := householdWith(t, "sam", "mika")
	if err := db.SetButlerMemberRole("mika", RoleOwner, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerMemberRole("sam", RoleOwner, "root-admin"); err != nil {
		t.Fatal(err)
	}
	at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)
	bind(t, db, at, "sam", "synthetic-account")
	if _, err := db.sql.Exec(`UPDATE connection_admission SET subject_kind='group' WHERE connection=?`, at.Seq); err != nil {
		t.Fatal(err)
	}
	if live, err := db.HasLivePersonChatBinding(at.Seq, "sam"); err != nil || live {
		t.Fatalf("initial denied state: %v %v", live, err)
	}

	var changed atomic.Bool
	function := fmt.Sprintf("workspace_snapshot_role_%d", workspaceSnapshotFunctionID.Add(1))
	err := sqlite.RegisterScalarFunction(function, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if changed.CompareAndSwap(false, true) {
			tx, err := db.sql.Begin()
			if err != nil {
				return nil, err
			}
			defer tx.Rollback()
			if _, err = tx.Exec(`UPDATE butler_members SET role='member' WHERE member='sam'`); err != nil {
				return nil, err
			}
			if _, err = tx.Exec(`UPDATE connection_admission SET admits='owners',subject_kind='person' WHERE connection=?`, at.Seq); err != nil {
				return nil, err
			}
			if err = tx.Commit(); err != nil {
				return nil, err
			}
		}
		return args[0], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	readerSQL, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", db.path))
	if err != nil {
		t.Fatal(err)
	}
	defer readerSQL.Close()
	readerSQL.SetMaxOpenConns(1)
	// Shadow only the reader's Person table. Every row/value still comes from
	// the real table; the role expression introduces the deterministic barrier.
	_, err = readerSQL.Exec(`CREATE TEMP VIEW butler_members AS SELECT person_id,member,` + function + `(role) AS role,admitted,disabled_at,offboarded_at FROM main.butler_members`)
	if err != nil {
		t.Fatal(err)
	}
	reader := &DB{sql: readerSQL}
	live, err := reader.HasLivePersonChatBinding(at.Seq, "sam")
	if err != nil {
		t.Fatal(err)
	}
	if !changed.Load() {
		t.Fatal("competing policy update was not exercised")
	}
	if live {
		t.Fatal("admitted impossible old-role/new-policy combination")
	}
	if live, err := db.HasLivePersonChatBinding(at.Seq, "sam"); err != nil || live {
		t.Fatalf("final denied state: %v %v", live, err)
	}
}
