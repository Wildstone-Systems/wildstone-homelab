package runtimedb

import (
	"database/sql"
	"database/sql/driver"
	"errors"
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

// Deterministically interleave room discovery between the earlier gateway
// lookup and the delivery transaction. The supplied destination is deliberately
// stale; both workspace-only and memory-derived history must be withheld.
func TestWorkspaceDeliveryRechecksRoomInTransaction(t *testing.T) {
	for _, memorySource := range []bool{false, true} {
		t.Run(fmt.Sprintf("memory=%v", memorySource), func(t *testing.T) {
			db, w, id := workspaceAuthorityFixture(t)
			at := connect(t, db, "alfred", "telegram", ButlerSurfaceAdmin)
			bind(t, db, at, "mika", "synthetic-account")

			ctx, _, access := workspacePrivacyTurn(t, db, id, ModelLocal, MemoryDestination{BuiltIn: true})
			if memorySource {
				memory, err := db.RecordMemory(MemoryRecord{Scope: MemoryScopePersonal, Kind: MemoryFact, Content: "synthetic source", Owner: "mika", Author: "mika", Butler: "alfred", Audience: ButlerAudiencePrivate, PermissionLabel: "household", SourceKind: "conversation", SourceID: id})
				if err != nil {
					t.Fatal(err)
				}
				if err = TrackMemoryPrivacy(ctx, memory, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "output", WorkspaceID: w.ID, Path: "output.txt"}); err != nil {
				t.Fatal(err)
			}
			if err := db.CreateWorkspaceRevisionForTask(ctx, access, w.ID, "", workspaceNewRevision("output-r1", "output")); err != nil {
				t.Fatal(err)
			}
			seq, _ := parseButlerTaskID(id)
			workspaceFinishRevision(t, db, w, seq, "output", "output-r1", "")
			if _, err := db.sql.Exec(`UPDATE chat_bindings SET conversation='synthetic-room-id' WHERE connection=?`, at.Seq); err != nil {
				t.Fatal(err)
			}
			if at.ExternalID == "synthetic-room-id" {
				t.Fatal("fixture must distinguish account and conversation")
			}
			seqID, _ := parseButlerTaskID(id)
			if _, err := db.sql.Exec(`UPDATE butler_tasks SET origin_connection=?,origin_connection_ref=?,origin_conversation=? WHERE seq=?`, at.Name(), at.Seq, "synthetic-room-id", seqID); err != nil {
				t.Fatal(err)
			}

			destination := MemoryDestination{ConnectionRef: at.Seq, Group: db.IsChatRoom(at.Seq, "synthetic-room-id")}
			if destination.Group {
				t.Fatal("initial destination is a group")
			}
			if err := db.CheckTaskDeliveryPrivacy(id, ModelLocal, destination); err != nil {
				t.Fatalf("initial delivery: %v", err)
			}
			// An unrelated room on the same account is not this destination.
			if err := db.NoteChatRoom(at.Seq, "unrelated-room"); err != nil {
				t.Fatal(err)
			}
			if err := db.CheckTaskDeliveryPrivacy(id, ModelLocal, destination); err != nil {
				t.Fatal(err)
			}
			// Neither an unknown task conversation nor a different current
			// binding conversation may borrow this Person's other chat.
			for _, change := range []string{
				"UPDATE butler_tasks SET origin_conversation='' WHERE seq=?",
				"UPDATE butler_tasks SET origin_conversation='other-conversation' WHERE seq=?",
			} {
				if _, err := db.sql.Exec(change, seqID); err != nil {
					t.Fatal(err)
				}
				if err := db.CheckTaskDeliveryPrivacy(id, ModelLocal, destination); !errors.Is(err, ErrPrivacy) {
					t.Fatalf("missing/mismatched conversation: %v", err)
				}
			}
			if _, err := db.sql.Exec(`UPDATE butler_tasks SET origin_conversation='synthetic-room-id' WHERE seq=?`, seqID); err != nil {
				t.Fatal(err)
			}
			if err := db.NoteChatRoom(at.Seq, "synthetic-room-id"); err != nil {
				t.Fatal(err)
			}
			if err := db.CheckTaskDeliveryPrivacy(id, ModelLocal, destination); !errors.Is(err, ErrPrivacy) {
				t.Fatalf("stale direct-chat result authorized delivery after room discovery: %v", err)
			}
		})
	}
}
