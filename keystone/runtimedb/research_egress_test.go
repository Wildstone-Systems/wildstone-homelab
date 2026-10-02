package runtimedb

import (
	"context"
	"errors"
	"testing"
)

func TestResearchEgressWorkspaceResumeAndCurrentAuthority(t *testing.T) {
	db, w, origin := workspaceAuthorityFixture(t)
	sourceTask, _ := parseButlerTaskID(origin)
	workspacePublish(t, db, workspaceIntent(t, db, w, sourceTask, "source", "source-r1", "", 1), "receipt")
	task, session := workspacePrivacyTask(t, db)
	ctx, _, access := workspacePrivacyTurn(t, db, task, ModelLocal, MemoryDestination{BuiltIn: true})
	if _, err := db.ReadWorkspaceRevision(ctx, access, w.ID, "source", "source-r1"); err != nil {
		t.Fatal(err)
	}
	if err := CheckResearchEgress(ctx); err != nil {
		t.Fatal(err)
	}
	// A model-authored "sanitized" derivative still carries its source edges.
	if err := db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "derived", WorkspaceID: w.ID, Path: "derived.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateWorkspaceRevisionForTask(ctx, access, w.ID, "", workspaceNewRevision("derived-r1", "derived")); err != nil {
		t.Fatal(err)
	}
	seq, _ := parseButlerTaskID(task)
	workspaceFinishRevision(t, db, w, seq, "derived", "derived-r1", "")
	next, _ := workspacePrivacyTask(t, db)
	derived, _, derivedAccess := workspacePrivacyTurn(t, db, next, ModelLocal, MemoryDestination{BuiltIn: true})
	if _, err := db.ReadWorkspaceRevision(derived, derivedAccess, w.ID, "derived", "derived-r1"); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"local_only", "trusted_only"} {
		if _, err := db.sql.Exec(`INSERT INTO content_privacy(object,` + flag + `) VALUES('wa:source',1) ON CONFLICT(object) DO UPDATE SET ` + flag + `=1`); err != nil {
			t.Fatal(err)
		}
		for _, c := range []context.Context{ctx, derived} {
			if err := CheckResearchEgress(c); !errors.Is(err, ErrResearchPrivacy) {
				t.Fatalf("%s disclosed: %v", flag, err)
			}
		}
		resumed, _, _ := workspacePrivacyTurn(t, db, task, ModelLocal, MemoryDestination{BuiltIn: true})
		if err := CheckResearchEgress(resumed); !errors.Is(err, ErrResearchPrivacy) {
			t.Fatalf("resume lost %s: %v", flag, err)
		}
		if _, err := db.sql.Exec(`DELETE FROM content_privacy WHERE object='wa:source'`); err != nil {
			t.Fatal(err)
		}
	}
	// A new task on retained session history also inherits the original source.
	nextTask := newTask(t, db, func(task *ButlerTask) {
		task.AgentClass, task.SessionID, task.OriginConnection = AgentClassOwner, session, "console"
	})
	if err := db.SetTaskButler(nextTask, "alfred"); err != nil {
		t.Fatal(err)
	}
	history, _, _ := workspacePrivacyTurn(t, db, nextTask, ModelLocal, MemoryDestination{BuiltIn: true})
	if err := db.RevokeButlerAccess("alfred", "mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []context.Context{ctx, derived, history} {
		if err := CheckResearchEgress(c); !errors.Is(err, ErrResearchPrivacy) {
			t.Fatalf("revoked source permitted: %v", err)
		}
	}
}

func TestResearchEgressFirstTurnAndFailures(t *testing.T) {
	db, _, task := workspaceAuthorityFixture(t)
	ctx, _, _ := workspacePrivacyTurn(t, db, task, ModelLocal, MemoryDestination{BuiltIn: true})
	if err := CheckResearchEgress(ctx); err != nil {
		t.Fatal(err)
	}
	memory, err := db.RecordMemory(MemoryRecord{Scope: MemoryScopePersonal, Kind: MemoryFact, Content: "synthetic source", Owner: "mika", Author: "mika", Butler: "alfred", Audience: ButlerAudiencePrivate, PermissionLabel: "household", SourceKind: "conversation", SourceID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetMemoryPrivacy("mika", memory, PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	TrackMemoryPrivacy(ctx, memory, nil)
	if err = CheckResearchEgress(ctx); !errors.Is(err, ErrResearchPrivacy) {
		t.Fatalf("local model disclosed first-turn source: %v", err)
	}
	resumed, _, _ := workspacePrivacyTurn(t, db, task, ModelLocal, MemoryDestination{BuiltIn: true})
	if err = CheckResearchEgress(resumed); !errors.Is(err, ErrResearchPrivacy) {
		t.Fatalf("denial dropped provenance: %v", err)
	}
	missing, _, err := db.BeginPrivacyTurn(context.Background(), ModelLocal, MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []context.Context{context.Background(), missing} {
		if err = CheckResearchEgress(c); !errors.Is(err, ErrResearchContext) {
			t.Fatalf("missing context: %v", err)
		}
	}
	TrackMemoryPrivacy(ctx, 0, errors.New("synthetic read failure"))
	if err = CheckResearchEgress(ctx); !errors.Is(err, ErrResearchPrivacy) {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = CheckResearchEgress(resumed); !errors.Is(err, ErrResearchPrivacy) {
		t.Fatalf("policy-read failure allowed: %v", err)
	}
}

func TestResearchEgressRejectsStaleSessionBinding(t *testing.T) {
	db, _, task := workspaceAuthorityFixture(t)
	ctx, _, _ := workspacePrivacyTurn(t, db, task, ModelLocal, MemoryDestination{BuiltIn: true})
	session, err := db.InsertButlerSession(ButlerSession{Principal: "mika", ActingFor: "mika", Butler: "alfred", Surface: ButlerSurfaceAdmin, Mode: ButlerModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetButlerTaskSession(task, session); err != nil {
		t.Fatal(err)
	}
	if err = CheckResearchEgress(ctx); !errors.Is(err, ErrResearchPrivacy) {
		t.Fatalf("stale session context allowed: %v", err)
	}
	fresh, _, _ := workspacePrivacyTurn(t, db, task, ModelLocal, MemoryDestination{BuiltIn: true})
	if err = CheckResearchEgress(fresh); err != nil {
		t.Fatal(err)
	}
}

func TestResearchEgressDeniedReadPersistsForLaterSessionTask(t *testing.T) {
	db, _, _ := workspaceAuthorityFixture(t)
	task, session := workspacePrivacyTask(t, db)
	ctx, _, _ := workspacePrivacyTurn(t, db, task, ModelLocal, MemoryDestination{BuiltIn: true})
	memory, err := db.RecordMemory(MemoryRecord{Scope: MemoryScopePersonal, Kind: MemoryFact, Content: "synthetic protected source", Owner: "mika", Author: "mika", Butler: "alfred", Audience: ButlerAudiencePrivate, PermissionLabel: "household", SourceKind: "conversation", SourceID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetMemoryPrivacy("mika", memory, PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err = TrackMemoryPrivacy(ctx, memory, nil); err != nil {
		t.Fatal(err)
	}
	// No PrivacyTurn.Check runs before or after this refusal.
	if err = CheckResearchEgress(ctx); !errors.Is(err, ErrResearchPrivacy) {
		t.Fatalf("protected in-flight read disclosed: %v", err)
	}
	var retained bool
	history, err := db.GetButlerSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM session_memory_sources WHERE session=? AND memory=?)`, history.Seq, memory).Scan(&retained); err != nil || !retained {
		t.Fatalf("session did not retain denied read: %v %v", retained, err)
	}
	next := newTask(t, db, func(task *ButlerTask) {
		task.AgentClass, task.SessionID, task.OriginConnection = AgentClassOwner, session, "console"
	})
	if err = db.SetTaskButler(next, "alfred"); err != nil {
		t.Fatal(err)
	}
	later, _, _ := workspacePrivacyTurn(t, db, next, ModelLocal, MemoryDestination{BuiltIn: true})
	if err = CheckResearchEgress(later); !errors.Is(err, ErrResearchPrivacy) {
		t.Fatalf("later task lost denied session input: %v", err)
	}
}
