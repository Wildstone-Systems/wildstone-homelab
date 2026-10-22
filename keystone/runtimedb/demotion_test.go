package runtimedb

import (
	"errors"
	"testing"
)

// Demotion must retain every Member-level resource. These assertions come
// first because over-revocation is the dangerous failure mode here.
func TestDemotionPreservesMemberResourcesAndDoesNotOffboard(t *testing.T) {
	db, vault, value := secretFixture(t)
	if err := db.SetButlerMemberRole("sam", RoleOwner, "mika"); err != nil {
		t.Fatal(err)
	}
	sam, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	entitle(t, db, "photos", EntitlementSubjectPerson, sam.PersonID, EntitlementAllow)
	memberConnection := connect(t, db, "shared", "telegram", ButlerSurfaceFamily)
	bind(t, db, memberConnection, "sam", "sam-member")
	if _, err := db.CreateButler(Butler{Name: "sam-agent", Directive: "household helper", Surface: ButlerSurfaceFamily,
		AgentClass: AgentClassMember, Model: "kitchen-box"}, "sam"); err != nil {
		t.Fatal(err)
	}
	memory := scopedFact(t, MemoryScopePersonal, "shared", "sam")
	memory.Content = "Sam's personal preference"
	memoryID, err := db.RecordMemory(memory)
	if err != nil {
		t.Fatal(err)
	}
	personalSecret, err := vault.Create("sam", SecretInput{Scope: "personal", Label: "Personal", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	var journalBefore, offboardingBefore int
	if err := db.sql.QueryRow(`SELECT count(*) FROM deletion_journal`).Scan(&journalBefore); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`SELECT count(*) FROM offboarding_records`).Scan(&offboardingBefore); err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerMemberRole("sam", RoleMember, "mika"); err != nil {
		t.Fatal(err)
	}
	sam, err = db.GetButlerMember("sam")
	if err != nil || !sam.Active() || sam.Role != RoleMember {
		t.Fatalf("Member identity lost: %+v %v", sam, err)
	}
	if allowed, err := db.EntitledTo(sam.PersonID, "photos"); err != nil || !allowed {
		t.Fatalf("service entitlement lost: %v %v", allowed, err)
	}
	if _, err := db.LookupChatBinding(memberConnection, "sam-member"); err != nil {
		t.Fatalf("Member-Agent admission lost: %v", err)
	}
	if agent, err := db.GetButler("sam-agent"); err != nil || !agent.Live() {
		t.Fatalf("demotion retired the Person's Agent: %+v %v", agent, err)
	}
	if record, err := db.GetMemory(memoryID); err != nil || record.Content != memory.Content {
		t.Fatalf("Personal memory lost: %+v %v", record, err)
	}
	hits, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "shared", Kind: memory.Kind, Limit: 10,
		ReadContext: db.BindMemoryRead("sam", "shared", MemoryDestination{BuiltIn: true})})
	if err != nil || len(hits) == 0 || hits[0].Seq != memoryID {
		t.Fatalf("demoted Member cannot retrieve their Personal memory: %+v %v", hits, err)
	}
	secrets, err := vault.List("sam", false)
	if err != nil || len(secrets) != 1 || secrets[0].ID != personalSecret.ID {
		t.Fatalf("Personal secret lost: %+v %v", secrets, err)
	}
	var journalAfter, offboardingAfter int
	if err := db.sql.QueryRow(`SELECT count(*) FROM deletion_journal`).Scan(&journalAfter); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`SELECT count(*) FROM offboarding_records`).Scan(&offboardingAfter); err != nil {
		t.Fatal(err)
	}
	if journalAfter != journalBefore || offboardingAfter != offboardingBefore {
		t.Fatalf("demotion entered offboarding: journal %d->%d, records %d->%d", journalBefore, journalAfter, offboardingBefore, offboardingAfter)
	}
}

func TestDemotionRevokesOwnerAdmissionAndHouseholdSecretManage(t *testing.T) {
	db, vault, value := secretFixture(t)
	if err := db.SetButlerMemberRole("sam", RoleOwner, "mika"); err != nil {
		t.Fatal(err)
	}
	ownerConnection := connect(t, db, "owner-agent", "telegram", ButlerSurfaceAdmin)
	binding := bind(t, db, ownerConnection, "sam", "sam-owner")
	if err := db.SetOwnerReviewPreference("mika", OwnerReviewPreference{BindingID: binding.ID, ConsoleURL: "https://console.home"}); err != nil {
		t.Fatal(err)
	}
	secret, err := vault.Create("mika", SecretInput{Scope: "household", Label: "Wi-Fi", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerMemberRole("sam", RoleMember, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LookupChatBinding(ownerConnection, "sam-owner"); err == nil {
		t.Fatal("Owner-Agent admission survived demotion")
	}
	preference, err := db.GetOwnerReviewPreference()
	if err != nil || preference.BindingID != 0 {
		t.Fatalf("Owner-bound review destination survived demotion: %+v %v", preference, err)
	}
	if err := vault.Rotate("sam", secret.ID, value); !errors.Is(err, ErrSecretDenied) {
		t.Fatalf("Household secret manage survived demotion: %v", err)
	}
}

func TestApprovedRoutineSurvivesItsApproverDemotion(t *testing.T) {
	db, _, _ := secretFixture(t)
	if err := db.SetButlerMemberRole("sam", RoleOwner, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateHouseholdRoutine(HouseholdRoutine{Name: "demotion-routine", Ownership: RoutineWildstone, Author: "mika", Document: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AssignRoutineButler("demotion-routine", "shared", "mika"); err != nil {
		t.Fatal(err)
	}
	plan, err := db.SubmitRoutinePlan("demotion-routine", "mika", "", AudienceHousehold, "household notification", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveRoutinePlan(plan, "sam"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerMemberRole("sam", RoleMember, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BindRoutinePrincipal(plan); err != nil {
		t.Fatalf("household-owned approval was revoked with its approver: %v", err)
	}
}
