package runtimedb

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestPersonWorkspaceRetirementRevocationAndIsolation(t *testing.T) {
	db, w, id := workspaceAuthorityFixture(t)
	task, _ := parseButlerTaskID(id)
	workspacePublish(t, db, workspaceIntent(t, db, w, task, "report", "revision", "", 1), "receipt")
	read := func(member string, household bool) error {
		_, entries, _, err := db.PersonWorkspaceEntries(context.Background(), member, "alfred", household, "report", "revision", "")
		if err == nil && len(entries) != 1 {
			t.Fatalf("entries=%v", entries)
		}
		return err
	}
	if err := read("mika", false); err != nil {
		t.Fatal(err)
	}
	workspacePublish(t, db, workspaceIntent(t, db, w, task, "report", "revision-2", "revision", 1), "receipt-2")
	_, history, _, err := db.PersonWorkspaceEntries(context.Background(), "mika", "alfred", false, "report", "", "")
	if err != nil || len(history) != 2 {
		t.Fatalf("retained history: %v %v", history, err)
	}
	if err := read("mika", false); err != nil {
		t.Fatalf("old revision remains readable: %v", err)
	}
	for _, tc := range []struct {
		member    string
		household bool
	}{{"sam", false}, {"mika", true}, {"unknown", false}} {
		if err := read(tc.member, tc.household); !errors.Is(err, ErrWorkspaceDenied) {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	if err := db.RetireButler("alfred", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := read("mika", false); err != nil {
		t.Fatalf("retained human access: %v", err)
	}
	if err := db.RevokeButlerAccess("alfred", "mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := read("mika", false); !errors.Is(err, ErrWorkspaceDenied) {
		t.Fatalf("revoked: %v", err)
	}
}

func TestPersonWorkspaceAdmissionAndMemorySourceRevocation(t *testing.T) {
	for _, denial := range []string{"disable", "offboard", "source-forgotten"} {
		t.Run(denial, func(t *testing.T) {
			db, w, id := workspaceAuthorityFixture(t)
			ctx, _, access := workspacePrivacyTurn(t, db, id, ModelLocal, MemoryDestination{BuiltIn: true})
			memory, err := db.RecordMemory(MemoryRecord{Scope: MemoryScopePersonal, Kind: MemoryFact, Content: "synthetic input", Owner: "mika", Author: "mika", Butler: "alfred", Audience: ButlerAudiencePrivate, PermissionLabel: "household", SourceKind: "conversation", SourceID: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if err = TrackMemoryPrivacy(ctx, memory, nil); err != nil {
				t.Fatal(err)
			}
			if err = db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "derived", WorkspaceID: w.ID, Path: "derived.txt"}); err != nil {
				t.Fatal(err)
			}
			if err = db.CreateWorkspaceRevisionForTask(ctx, access, w.ID, "", workspaceNewRevision("derived-r1", "derived")); err != nil {
				t.Fatal(err)
			}
			task, _ := parseButlerTaskID(id)
			workspaceFinishRevision(t, db, w, task, "derived", "derived-r1", "")
			_, entries, _, err := db.PersonWorkspaceEntries(context.Background(), "mika", "alfred", false, "derived", "derived-r1", "")
			if err != nil || len(entries) != 1 {
				t.Fatalf("before revocation: %v %v", entries, err)
			}
			if denial == "source-forgotten" {
				err = db.ForgetMemory(memory, "mika")
			} else {
				if err = db.RecordButlerMember("spare-owner", RoleOwner, "", ""); err != nil {
					t.Fatal(err)
				}
				if err = db.AdmitButlerMember("spare-owner", "fixture"); err != nil {
					t.Fatal(err)
				}
				if denial == "disable" {
					err = db.DisableButlerMember("mika", "fixture")
				} else {
					err = db.OffboardButlerMember("mika", "fixture")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			_, entries, _, err = db.PersonWorkspaceEntries(context.Background(), "mika", "alfred", false, "derived", "derived-r1", "")
			if denial == "source-forgotten" {
				if err != nil || len(entries) != 0 {
					t.Fatalf("forgotten source: %v %v", entries, err)
				}
			} else if !errors.Is(err, ErrWorkspaceDenied) {
				t.Fatalf("lifecycle denial: %v", err)
			}
		})
	}
}

func TestPersonWorkspaceCommittedPaginationAndTrash(t *testing.T) {
	db, w, id := workspaceAuthorityFixture(t)
	task, _ := parseButlerTaskID(id)
	for i := 0; i < 22; i++ {
		name := fmt.Sprintf("report-%02d", i)
		workspacePublish(t, db, workspaceIntent(t, db, w, task, name, name+"-r1", "", 1), name+"-receipt")
	}
	if err := db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "pending", WorkspaceID: w.ID, Path: "pending.txt"}); err != nil {
		t.Fatal(err)
	}
	_, first, snapshot, err := db.PersonWorkspaceEntries(context.Background(), "mika", "alfred", false, "", "", "")
	if err != nil || len(first) != 21 {
		t.Fatalf("page %d: %v", len(first), err)
	}
	_, second, snapshot2, err := db.PersonWorkspaceEntries(context.Background(), "mika", "alfred", false, "", "", first[19].Artifact.ID+"\x00"+first[19].Revision.ID)
	if err != nil || len(second) != 2 || snapshot != snapshot2 {
		t.Fatalf("next page %d %s %s: %v", len(second), snapshot, snapshot2, err)
	}
	if err := db.TrashWorkspaceArtifact(w.ID, "report-00", "report-00-r1"); err != nil {
		t.Fatal(err)
	}
	_, entries, snapshot3, err := db.PersonWorkspaceEntries(context.Background(), "mika", "alfred", false, "report-00", "report-00-r1", "")
	if err != nil || len(entries) != 0 || snapshot == snapshot3 {
		t.Fatalf("trash read: %v %v", entries, err)
	}
}

func TestPersonWorkspaceSourceRestrictionsRemainCurrent(t *testing.T) {
	db, w, id := workspaceAuthorityFixture(t)
	task, _ := parseButlerTaskID(id)
	workspacePublish(t, db, workspaceIntent(t, db, w, task, "source", "source-r1", "", 1), "source-receipt")
	ctx, _, access := workspacePrivacyTurn(t, db, id, ModelLocal, MemoryDestination{BuiltIn: true})
	if _, err := db.ReadWorkspaceRevision(ctx, access, w.ID, "source", "source-r1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "derived", WorkspaceID: w.ID, Path: "derived.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateWorkspaceRevisionForTask(ctx, access, w.ID, "", workspaceNewRevision("derived-r1", "derived")); err != nil {
		t.Fatal(err)
	}
	workspaceFinishRevision(t, db, w, task, "derived", "derived-r1", "")
	_, entries, _, err := db.PersonWorkspaceEntries(context.Background(), "mika", "alfred", false, "derived", "derived-r1", "")
	if err != nil || len(entries) != 1 {
		t.Fatalf("initial: %v %v", entries, err)
	}
	// The current Agent class no longer matches the origin's class/surface.
	if _, err = db.SetAgentClass("alfred", AgentClassMember, "root-admin"); err != nil {
		t.Fatal(err)
	}
	_, entries, _, err = db.PersonWorkspaceEntries(context.Background(), "mika", "alfred", false, "derived", "derived-r1", "")
	if err != nil || len(entries) != 0 {
		t.Fatalf("source ceiling changed: %v %v", entries, err)
	}
}
