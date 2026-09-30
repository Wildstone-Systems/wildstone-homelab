package runtimedb

import (
	"errors"
	"testing"
)

func workspaceAuthorityFixture(t *testing.T) (*DB, Workspace, string) {
	t.Helper()
	db := agentFixture(t)
	p, err := db.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.GrantButlerAccess(b.Name, p.Member, "root-admin"); err != nil {
		t.Fatal(err)
	}
	w := Workspace{ID: "private-workspace", PersonID: p.PersonID, AgentID: b.Seq}
	if err := db.CreateWorkspace(w); err != nil {
		t.Fatal(err)
	}
	id := newTask(t, db, func(task *ButlerTask) { task.AgentClass = AgentClassOwner; task.OriginConnection = "console" })
	if err := db.SetTaskButler(id, b.Name); err != nil {
		t.Fatal(err)
	}
	return db, w, id
}

func TestWorkspaceTaskAuthorityIsolation(t *testing.T) {
	db, w, id := workspaceAuthorityFixture(t)
	if err := db.CheckWorkspaceTask("mika", "alfred", id, "console", w.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, person, agent, task, connection, workspace string }{
		{"second person", "sam", "alfred", id, "console", w.ID},
		{"other agent", "mika", "hobson", id, "console", w.ID},
		{"missing person", "", "alfred", id, "console", w.ID},
		{"missing task", "mika", "alfred", "", "console", w.ID},
		{"unknown task", "mika", "alfred", "bt-99999", "console", w.ID},
		{"missing connection", "mika", "alfred", id, "", w.ID},
		{"wrong surface", "mika", "alfred", id, "household_portal", w.ID},
		{"unknown workspace", "mika", "alfred", id, "console", "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := db.CheckWorkspaceTask(tc.person, tc.agent, tc.task, tc.connection, tc.workspace); !errors.Is(err, ErrWorkspaceDenied) {
				t.Fatalf("want opaque denial, got %v", err)
			}
		})
	}
	if err := db.SetButlerMemberRole("sam", RoleOwner, "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckWorkspaceTask("sam", "alfred", id, "console", w.ID); !errors.Is(err, ErrWorkspaceDenied) {
		t.Fatalf("Owner gained private access: %v", err)
	}
	if err := db.SetButlerMemberRole("mika", RoleMember, "sam"); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckWorkspaceTask("mika", "alfred", id, "console", w.ID); !errors.Is(err, ErrWorkspaceDenied) {
		t.Fatalf("demotion retained access: %v", err)
	}
}

func TestWorkspaceTaskAuthorityRechecksRevocation(t *testing.T) {
	db, w, id := workspaceAuthorityFixture(t)
	if err := db.RevokeButlerAccess("alfred", "mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckWorkspaceTask("mika", "alfred", id, "console", w.ID); !errors.Is(err, ErrWorkspaceDenied) {
		t.Fatalf("revoked access survived: %v", err)
	}
}

func TestWorkspaceTaskAuthorityRefusesRoutineAndUntrustedOrigin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ButlerTask)
	}{
		{"routine", func(task *ButlerTask) { task.Kind = ButlerTaskRoutine }},
		{"group", func(task *ButlerTask) { task.Audience = ButlerAudienceShared }},
		{"unclassified origin", func(task *ButlerTask) { task.OriginConnection = "unclassified" }},
		{"portal owner agent", func(task *ButlerTask) { task.OriginConnection = "household_portal" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, w, _ := workspaceAuthorityFixture(t)
			task := ButlerTask{Kind: ButlerTaskConversation, Author: "mika", Owner: "mika", AgentClass: AgentClassOwner, Surface: ButlerSurfaceAdmin, Audience: ButlerAudiencePrivate, OriginConnection: "console", Objective: "test workspace authority"}
			tc.mutate(&task)
			id, err := db.InsertButlerTask(task)
			if err != nil {
				t.Fatal(err)
			}
			if err = db.SetTaskButler(id, "alfred"); err != nil {
				t.Fatal(err)
			}
			if err = db.CheckWorkspaceTask("mika", "alfred", id, task.OriginConnection, w.ID); !errors.Is(err, ErrWorkspaceDenied) {
				t.Fatalf("want denial, got %v", err)
			}
		})
	}
}
