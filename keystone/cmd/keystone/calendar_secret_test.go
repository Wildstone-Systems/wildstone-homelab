package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
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
	callback := calendarSecretUse(vault)
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
