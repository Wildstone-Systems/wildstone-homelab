package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/operation"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestCalendarCallUsesRoutinePrincipalWithoutBorrowingPerson(t *testing.T) {
	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, who := range []struct{ name, role string }{{"mika", runtimedb.RoleOwner}, {"sam", runtimedb.RoleMember}} {
		if err = db.RecordButlerMember(who.name, who.role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err = db.AdmitButlerMember(who.name, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.UpsertModel(runtimedb.Model{Name: "local", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateButler(runtimedb.Butler{Name: "shared", Directive: "family", Surface: runtimedb.ButlerSurface("family"), AgentClass: runtimedb.AgentClassMember, Model: "local"}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"mika", "sam"} {
		if err = db.GrantButlerAccess("shared", who, "mika"); err != nil {
			t.Fatal(err)
		}
	}
	vault, err := db.OpenSecretVault(filepath.Join(t.TempDir(), "household.key"))
	if err != nil {
		t.Fatal(err)
	}
	personal, err := vault.Create("sam", runtimedb.SecretInput{Scope: "personal", Label: "Personal calendar", Value: "private-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.AgentGrant("sam", personal.ID, "shared", "calendar.get_agenda", true); err != nil {
		t.Fatal(err)
	}
	if err = vault.BindCalendar("sam", personal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "calendar-run", Ownership: runtimedb.RoutineWildstone, Author: "sam", Document: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err = db.AssignRoutineButler("calendar-run", "shared", "mika"); err != nil {
		t.Fatal(err)
	}
	id, err := db.SubmitRoutinePlan("calendar-run", "sam", "", runtimedb.AudienceHousehold, "notification", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ApproveRoutinePlan(id, "mika"); err != nil {
		t.Fatal(err)
	}
	p, err := db.BindRoutinePrincipal(id)
	if err != nil {
		t.Fatal(err)
	}
	callback := calendarSecretUse(db, vault)
	ctx := runtimedb.WithRoutineSecretUseContext(context.Background(), p)
	ctx = runtimedb.WithSecretUseContext(ctx, db.BindMemoryRead("sam", "shared", runtimedb.MemoryDestination{BuiltIn: true}))
	called := false
	bound, err := callback(ctx, domestic.Call{Tool: "calendar.get_agenda", Attended: false}, func(string) error { called = true; return nil })
	if !bound || err == nil || called {
		t.Fatalf("routine borrowed Personal binding: bound=%v err=%v called=%v", bound, err, called)
	}

	household, err := vault.Create("mika", runtimedb.SecretInput{Scope: "household", Label: "Family calendar", Value: "shared-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.AgentGrant("mika", household.ID, "shared", "calendar.get_agenda", true); err != nil {
		t.Fatal(err)
	}
	if err = vault.BindCalendar("mika", household.ID); err != nil {
		t.Fatal(err)
	}
	// Agent capability and an Owner's approval still do not provide the
	// exact-version Household secret grant.
	bound, err = callback(ctx, domestic.Call{Tool: "calendar.get_agenda", Attended: false}, func(string) error { called = true; return nil })
	if !bound || err == nil || called {
		t.Fatalf("ungranted Household secret used: bound=%v err=%v called=%v", bound, err, called)
	}
	next, err := db.SubmitRoutinePlan("calendar-run", "sam", "", runtimedb.AudienceHousehold, "notification", "fixture", runtimedb.RoutineResources{SecretIDs: []string{household.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ApproveRoutinePlan(next, "mika"); err != nil {
		t.Fatal(err)
	}
	approved, err := db.BindRoutinePrincipal(next)
	if err != nil {
		t.Fatal(err)
	}
	ctx = runtimedb.WithRoutineSecretUseContext(context.Background(), approved)
	bound, err = callback(ctx, domestic.Call{Tool: "calendar.get_agenda", Attended: false}, func(value string) error {
		called = true
		if value != "shared-token" {
			t.Fatal("wrong credential reached the calendar")
		}
		return nil
	})
	if !bound || err != nil || !called {
		t.Fatalf("exact-version Household grant unusable: bound=%v err=%v called=%v", bound, err, called)
	}
}

// TestCalendarCallResolvesPersonSecretFromTheTaskIdentity is Commit 3's
// own evidence (ADR-W039 Phase 4, C11): the attended/Person branch no
// longer reads a MemoryReadContext someone else stuffed into ctx — it
// derives one itself, gateway-side, from operation.Identity's TaskID
// alone, by reading the task's own admission back.
func TestCalendarCallResolvesPersonSecretFromTheTaskIdentity(t *testing.T) {
	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.RecordButlerMember("sam", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModel(runtimedb.Model{Name: "local", Kind: runtimedb.ModelLocal, Provider: "ollama",
		ProviderModel: "llama", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateButler(runtimedb.Butler{Name: "alfred", Directive: "family",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "local"}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantButlerAccess("alfred", "sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	vault, err := db.OpenSecretVault(filepath.Join(t.TempDir(), "household.key"))
	if err != nil {
		t.Fatal(err)
	}
	personal, err := vault.Create("sam", runtimedb.SecretInput{Scope: "personal", Label: "Personal calendar", Value: "sams-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.AgentGrant("sam", personal.ID, "alfred", "calendar.get_agenda", true); err != nil {
		t.Fatal(err)
	}
	if err := vault.BindCalendar("sam", personal.ID); err != nil {
		t.Fatal(err)
	}
	taskID, err := db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "sam", Owner: "sam",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember,
		Audience: runtimedb.ButlerAudiencePrivate, OriginConnection: "console",
		Objective: "what's on today", ObjectiveKind: runtimedb.ObjectiveInformational,
		Retention: runtimedb.ButlerRetentionConversation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetTaskButler(taskID, "alfred"); err != nil {
		t.Fatal(err)
	}

	callback := calendarSecretUse(db, vault)
	ctx := operation.With(context.Background(), operation.Identity{TaskID: taskID})
	var got string
	bound, err := callback(ctx, domestic.Call{Tool: "calendar.get_agenda", Attended: true}, func(raw string) error {
		got = raw
		return nil
	})
	if !bound || err != nil {
		t.Fatalf("bound=%v err=%v, want bound and no error", bound, err)
	}
	if got != "sams-token" {
		t.Fatalf("got %q, want sam's own personal calendar token", got)
	}

	// A call with no task identity at all is refused, never guessed.
	bound, err = callback(context.Background(), domestic.Call{Tool: "calendar.get_agenda", Attended: true}, func(string) error {
		t.Fatal("the callback ran with no task identity at all")
		return nil
	})
	if !bound || err == nil {
		t.Fatalf("no-task-identity call: bound=%v err=%v, want bound=true and a refusal", bound, err)
	}
}
