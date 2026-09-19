package runtimedb

// Service entitlements (ADR-W033 design §4, §17.3, §18.2; slice 8, #209).
//
// The negative cases are the point of this file. An entitlement system
// is only worth having if it refuses, and every way it can wrongly admit
// somebody — an inherited allow beating an explicit refusal, a dormant
// row still granting, an offboarded person still counted — is a person
// reaching a service the household said they could not.

import (
	"strings"
	"testing"
)

// entitle is the two-line setup most of these tests want.
func entitle(t *testing.T, db *DB, service, kind, subject, effect string) {
	t.Helper()
	if err := db.SetEntitlement(service, kind, subject, effect, "mika"); err != nil {
		t.Fatalf("SetEntitlement(%s, %s, %s): %v", service, kind, effect, err)
	}
}

// §18.2: a new Member inherits the default access group.
func TestNewMemberInheritsTheDefaultGroup(t *testing.T) {
	db := open(t)
	person := personOf(t, db, "giulia", RoleMember)

	family := groupID(t, db, SeedFamily)
	in, err := db.PersonIsInGroup(person, family)
	if err != nil {
		t.Fatal(err)
	}
	if !in {
		t.Fatal("an admitted Member is not in the default access group; §18.2 " +
			"says they inherit it, and with entitlements defaulting to deny a " +
			"Member in no group reaches nothing at all")
	}

	// And inheriting it grants nothing by itself (§4). The group has to
	// be entitled to something before anyone in it reaches anything.
	ok, err := db.EntitledTo(person, "photos")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("membership of the default group admitted somebody to a service " +
			"nothing had entitled it to — one namespace is not one permission")
	}

	entitle(t, db, "photos", EntitlementSubjectGroup, family, EntitlementAllow)
	if ok, err := db.EntitledTo(person, "photos"); err != nil || !ok {
		t.Errorf("the Member did not inherit the group's entitlement (%v, %v)", ok, err)
	}
}

// MANDATORY negative case (#209): explicit restriction overrides
// inherited allow.
func TestExplicitRestrictionOverridesInheritedAllow(t *testing.T) {
	db := open(t)
	adult := personOf(t, db, "mika", RoleOwner)
	teen := personOf(t, db, "sam", RoleMember)

	// Both are in Family, and Family may use the password manager.
	family := groupID(t, db, SeedFamily)
	entitle(t, db, "passwords", EntitlementSubjectGroup, family, EntitlementAllow)
	for _, p := range []string{adult, teen} {
		if ok, err := db.EntitledTo(p, "passwords"); err != nil || !ok {
			t.Fatalf("the group allow did not reach %s (%v, %v)", p, ok, err)
		}
	}

	// One of them is restricted by name.
	entitle(t, db, "passwords", EntitlementSubjectPerson, teen, EntitlementRestrict)

	if ok, err := db.EntitledTo(teen, "passwords"); err != nil || ok {
		t.Error("a Person restricted by name still reached the service through a " +
			"group that allows it; the explicit refusal must outrank the " +
			"inherited allow, or a restriction can only be expressed by " +
			"removing the whole group's access")
	}
	// And it is a refusal of one person, not of the group.
	if ok, err := db.EntitledTo(adult, "passwords"); err != nil || !ok {
		t.Errorf("restricting one Person removed everybody's access (%v, %v)", ok, err)
	}
}

// The other direction: an explicit ALLOW outranks a group restriction.
//
// Not stated in #209, and worth pinning because the rule is about
// specificity rather than about refusal winning. A household that
// restricts `Kids` from the password manager and then allows one
// teenager by name has said something coherent; an implementation where
// restrict always wins silently discards the second half of it.
func TestAnExplicitAllowOutranksAGroupRestriction(t *testing.T) {
	db := open(t)
	personOf(t, db, "mika", RoleOwner)
	teen := personOf(t, db, "sam", RoleMember)

	kids := groupID(t, db, SeedKids)
	if err := db.AddPersonToGroup(kids, teen, "mika"); err != nil {
		t.Fatal(err)
	}
	family := groupID(t, db, SeedFamily)
	entitle(t, db, "passwords", EntitlementSubjectGroup, family, EntitlementAllow)
	entitle(t, db, "passwords", EntitlementSubjectGroup, kids, EntitlementRestrict)

	// Restricted through Kids, with no row naming them.
	if ok, err := db.EntitledTo(teen, "passwords"); err != nil || ok {
		t.Fatalf("the group restriction did not hold (%v, %v)", ok, err)
	}

	entitle(t, db, "passwords", EntitlementSubjectPerson, teen, EntitlementAllow)
	if ok, err := db.EntitledTo(teen, "passwords"); err != nil || !ok {
		t.Error("an explicit allow did not outrank a group restriction; the rule " +
			"is that the row naming the Person is the more specific statement, " +
			"so it wins whichever way it points")
	}
}

// §17.3 / §18.2: removing a service makes entitlements dormant, not
// deleted.
func TestRemovingAServiceMakesEntitlementsDormantNotDeleted(t *testing.T) {
	db := open(t)
	person := personOf(t, db, "mika", RoleOwner)
	family := groupID(t, db, SeedFamily)
	entitle(t, db, "photos", EntitlementSubjectGroup, family, EntitlementAllow)

	if err := db.MakeServiceDormant("photos"); err != nil {
		t.Fatalf("MakeServiceDormant: %v", err)
	}

	// Dormant grants nothing.
	if ok, err := db.EntitledTo(person, "photos"); err != nil || ok {
		t.Error("a dormant entitlement still admitted somebody; an uninstalled " +
			"service has no access to grant")
	}
	// But the decision survives, which is the whole of §17.3.
	rows, err := db.ListEntitlements("photos")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("removing the service left %d entitlement rows, want 1 — "+
			"'removing a package does not erase its logical assignments'", len(rows))
	}
	if !rows[0].Dormant() {
		t.Error("the surviving row is not marked dormant")
	}
}

// §17.3: reinstalling the same compatible service restores dormant
// assignments.
func TestReinstallingACompatibleServiceRestoresDormantAssignments(t *testing.T) {
	db := open(t)
	person := personOf(t, db, "mika", RoleOwner)
	other := personOf(t, db, "giulia", RoleMember)
	family := groupID(t, db, SeedFamily)

	// What the household decided: the default group may use it, and one
	// person may not.
	entitle(t, db, "photos", EntitlementSubjectGroup, family, EntitlementAllow)
	entitle(t, db, "photos", EntitlementSubjectPerson, other, EntitlementRestrict)
	if err := db.MakeServiceDormant("photos"); err != nil {
		t.Fatal(err)
	}

	// Reinstalled — a DIFFERENT profile of the same capability, which is
	// what "compatible" means and why the rows are keyed on the
	// capability rather than on the package that provided it.
	if err := db.SeedServiceEntitlements("photos", []string{SeedFamily}, "installer"); err != nil {
		t.Fatalf("SeedServiceEntitlements: %v", err)
	}

	if ok, err := db.EntitledTo(person, "photos"); err != nil || !ok {
		t.Errorf("the group's assignment did not come back (%v, %v)", ok, err)
	}
	// The restriction came back too. A reinstall that restored only the
	// allows would quietly un-restrict somebody, which is the failure
	// that matters here — the other direction is merely inconvenient.
	if ok, err := db.EntitledTo(other, "photos"); err != nil || ok {
		t.Error("reinstalling the service un-restricted a Person the household " +
			"had refused; a restoration that restores only the permissive half " +
			"is worse than not restoring at all")
	}
}

// Seeding states a default and must never overwrite a decision.
func TestSeedingDoesNotOverwriteWhatTheHouseholdDecided(t *testing.T) {
	db := open(t)
	person := personOf(t, db, "mika", RoleOwner)
	family := groupID(t, db, SeedFamily)

	// The household refused the default group outright.
	entitle(t, db, "photos", EntitlementSubjectGroup, family, EntitlementRestrict)
	// The package says it is for `family`, and is installed again.
	if err := db.SeedServiceEntitlements("photos", []string{SeedFamily}, "installer"); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.EntitledTo(person, "photos"); err != nil || ok {
		t.Error("re-running the installer overrode the household's refusal with " +
			"the manifest's default; what a package is FOR and what a household " +
			"DECIDED are different things")
	}
}

// §4: being entitled to a service grants nothing in §11 or §12.
//
// The "one namespace is not one permission" rule, tested where it is
// easiest to break: the same group is the subject of an entitlement and
// of an audience, and the two must stay independent.
func TestEntitlementDoesNotGrantMemoryOrSecrets(t *testing.T) {
	db := open(t)
	owner := personOf(t, db, "mika", RoleOwner)
	other := personOf(t, db, "giulia", RoleMember)

	// A private record belonging to the Owner.
	if _, err := db.RecordMemory(MemoryRecord{
		Kind: "fact", Subject: "mika", Content: "a private thing",
		Audience: ButlerAudiencePrivate, PermissionLabel: "mika", Owner: "mika",
		SourceKind: "conversation", SourceID: "c-1", Author: "mika", EnvelopeVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Entitle the whole household to everything in sight.
	family := groupID(t, db, SeedFamily)
	for _, svc := range []string{"photos", "passwords", "assistant"} {
		entitle(t, db, svc, EntitlementSubjectGroup, family, EntitlementAllow)
	}
	if ok, err := db.EntitledTo(other, "passwords"); err != nil || !ok {
		t.Fatalf("the entitlement did not take (%v, %v)", ok, err)
	}

	// It bought them no memory. The audiences they can see are decided
	// by slice 7 and have not moved.
	visible, err := db.AudiencesVisibleTo(other)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range visible {
		if v == AudienceOwner {
			t.Error("an entitlement made the owner audience visible")
		}
	}
	hits, err := db.RetrieveMemory(MemoryQuery{Reader: "giulia"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Content == "a private thing" {
			t.Error("entitling the household to services disclosed the Owner's " +
				"private memory; §4 says membership of a group is an identity " +
				"primitive and what it authorizes is each resource's own decision")
		}
	}
	_ = owner
}

// Owner is a Wildstone role, not a service group (#209).
//
// Structural as well as behavioural, because the way this breaks is not
// a bug in a check: it is somebody two slices from now adding an
// `owners` seed key because the role and the group look like the same
// idea. They are two axes — a household can have an Owner who is not in
// `Adults`, and an adult who is not an Owner — and the moment one is
// derived from the other, neither can be changed without changing both.
func TestOwnerRoleIsNotProjectedAsAServiceEntitlement(t *testing.T) {
	db := open(t)
	owner := personOf(t, db, "mika", RoleOwner)

	// The role put them in no group but the default one.
	groups, err := db.GroupsForPerson(owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].SeedKey != SeedFamily {
		t.Errorf("an Owner is in %v; the role must not imply a group beyond the "+
			"default one every member inherits", groups)
	}

	// And it entitled them to nothing.
	if ok, err := db.EntitledTo(owner, "passwords"); err != nil || ok {
		t.Error("being an Owner entitled somebody to a service by itself; the " +
			"role governs Wildstone's own surfaces, not who may open an app")
	}

	// Structural: there is no group whose role is the Owner role. The
	// seeded vocabulary is the three §4 names and nothing else.
	rows, err := db.sql.Query(`SELECT seed_key FROM person_groups WHERE seed_key IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		switch key {
		case SeedFamily, SeedAdults, SeedKids:
		default:
			t.Errorf("seed key %q exists; a role that became a group is how the "+
				"two axes stop being separable", key)
		}
	}
}

// Default deny (#209's central property, at the layer this PR builds).
func TestAServiceNobodyWasEntitledToReachesNobody(t *testing.T) {
	db := open(t)
	person := personOf(t, db, "mika", RoleOwner)

	if ok, err := db.EntitledTo(person, "photos"); err != nil || ok {
		t.Error("a service with no entitlement rows admitted somebody; the " +
			"absence of a decision is a refusal, not a permission")
	}
	// A caller with no Person at all reaches nothing either, and that is
	// not an error — an unauthenticated request is a real state.
	if ok, err := db.EntitledTo("", "photos"); err != nil || ok {
		t.Errorf("an unnamed caller was admitted (%v, %v)", ok, err)
	}
}

// An offboarded person holds no inherited entitlement (the standing rule
// slice 6's resolver applies, reaching this layer through it).
func TestAnOffboardedPersonInheritsNothing(t *testing.T) {
	db := open(t)
	personOf(t, db, "mika", RoleOwner)
	leaver := personOf(t, db, "sam", RoleMember)

	family := groupID(t, db, SeedFamily)
	entitle(t, db, "photos", EntitlementSubjectGroup, family, EntitlementAllow)
	if ok, err := db.EntitledTo(leaver, "photos"); err != nil || !ok {
		t.Fatalf("setup: the member was not entitled (%v, %v)", ok, err)
	}

	if err := db.DisableButlerMember("sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.EntitledTo(leaver, "photos"); err != nil || ok {
		t.Error("a disabled Person still held their group's entitlement; " +
			"membership must go through slice 6's resolver, which counts only " +
			"people in good standing")
	}
}

// Removing a decision is not the same as refusing (#209's vocabulary,
// which has three states and not two).
func TestRemovingADecisionIsNotRefusing(t *testing.T) {
	db := open(t)
	person := personOf(t, db, "sam", RoleMember)
	family := groupID(t, db, SeedFamily)
	entitle(t, db, "photos", EntitlementSubjectGroup, family, EntitlementAllow)
	entitle(t, db, "photos", EntitlementSubjectPerson, person, EntitlementRestrict)

	if ok, err := db.EntitledTo(person, "photos"); err != nil || ok {
		t.Fatalf("setup: the restriction did not hold (%v, %v)", ok, err)
	}
	if err := db.RemoveEntitlement("photos", EntitlementSubjectPerson, person); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.EntitledTo(person, "photos"); err != nil || !ok {
		t.Error("removing a restriction did not return the Person to what their " +
			"groups say; without that there is no way to change one's mind")
	}
}

// One resolver, structurally (#209's sibling of the slice-6 and slice-7
// rules).
func TestEntitlementIsDecidedInOnePlace(t *testing.T) {
	src := readSourceFile(t, "entitlements.go")
	if n := strings.Count(src, "func (db *DB) EntitledTo"); n != 1 {
		t.Errorf("EntitledTo is defined %d times", n)
	}
	// EntitledServices must ask the resolver rather than reimplement the
	// precedence rules in SQL — a join expressing "allow unless
	// restricted" is a second answer that agrees until one is edited.
	if !strings.Contains(src, "db.EntitledTo(personID, s)") {
		t.Error("EntitledServices no longer goes through EntitledTo; the " +
			"precedence rules must have exactly one implementation")
	}
}
