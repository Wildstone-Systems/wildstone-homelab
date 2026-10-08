package runtimedb

// The one principal namespace (ADR-W033 design §4, §18.1; slice 6).
//
// The defining test in this file is the one that asserts NOTHING
// happened. Every other property here — the union, the rename, the
// immediacy — is about the namespace being usable; only
// TestGroupMembershipGrantsNothingByItself is about it staying a
// namespace rather than quietly becoming a permission model.

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// readSourceFile reads a file of this package, for the structural
// assertions below. A test that greps its own source is unusual; it is
// here because "there is only one implementation" is a claim about the
// code rather than about any behaviour, and a behavioural test cannot
// see a second resolver until the two disagree.
func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(raw)
}

// functionBody returns the source of one method, from its signature to
// the closing brace in column one.
func functionBody(t *testing.T, src, name string) string {
	t.Helper()
	at := strings.Index(src, ") "+name+"(")
	if at < 0 {
		t.Fatalf("%s is gone from this package", name)
	}
	body := src[at:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	return body
}

// personOf admits a member and returns their person_id, which is what
// groups are keyed on.
func personOf(t *testing.T, db *DB, member, role string) string {
	t.Helper()
	admitted(t, db, member, role)
	m, err := db.GetButlerMember(member)
	if err != nil {
		t.Fatalf("GetButlerMember(%s): %v", member, err)
	}
	return m.PersonID
}

func groupID(t *testing.T, db *DB, seedKey string) string {
	t.Helper()
	g, err := db.GroupBySeedKey(seedKey)
	if err != nil {
		t.Fatalf("GroupBySeedKey(%s): %v", seedKey, err)
	}
	return g.ID
}

// THE defining negative case for this slice (#207).
//
// Membership is an identity primitive. Putting somebody in `Adults`
// must leave every other thing about them exactly as it was — §4:
// "Being in `Adults` does not grant every adult service, every
// adults-only memory record, and every adults-only secret."
//
// Asserted as an absence across the surfaces that exist today, plus a
// structural check that no grants table appeared. The structural half
// matters more than it looks: the way this property dies is not a bug
// in a grant check, it is somebody adding `group_grants` in a later
// slice because it seemed like the natural place, and no behavioural
// test would notice that until the first grant was written.
func TestGroupMembershipGrantsNothingByItself(t *testing.T) {
	db := open(t)
	person := personOf(t, db, "giulia", RoleMember)
	adults := groupID(t, db, SeedAdults)

	before, err := db.GetButlerMember("giulia")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddPersonToGroup(adults, person, "mika"); err != nil {
		t.Fatalf("AddPersonToGroup: %v", err)
	}
	after, err := db.GetButlerMember("giulia")
	if err != nil {
		t.Fatal(err)
	}

	// Role, preset and standing are untouched. A namespace that could
	// change any of these would be a permission model.
	if after.Role != before.Role {
		t.Errorf("joining a group changed role: %q -> %q", before.Role, after.Role)
	}
	if after.Profile != before.Profile {
		t.Errorf("joining a group changed the preset: %q -> %q", before.Profile, after.Profile)
	}
	if after.Admitted != before.Admitted || after.Generation != before.Generation {
		t.Error("joining a group changed admission or generation")
	}

	// And no grants table exists to hang a permission on.
	rows, err := db.sql.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE '%group%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "person_groups", "person_group_members":
		default:
			t.Errorf("an unexpected group table exists: %q — §4 says membership "+
				"authorizes nothing, and a grants table here is where that stops "+
				"being true", name)
		}
	}
}

// The preset and the group are separate mechanisms, and neither creates
// the other (#207, §18.1).
//
// A child is a Member with the `child` preset. That is a constraint on
// what they may do; `Kids` is a set of people. Inferring one from the
// other is exactly the drift this slice exists to prevent — and it
// would arrive looking like a convenience.
func TestChildPresetDoesNotImplyKidsMembership(t *testing.T) {
	db := open(t)
	// A guardian first: the schema refuses a child without one.
	guardian := personOf(t, db, "mika", RoleOwner)
	if err := db.RecordButlerMember("sam", RoleMember, "child", "mika"); err != nil {
		t.Fatalf("RecordButlerMember(child): %v", err)
	}
	_ = guardian

	kids := groupID(t, db, SeedKids)
	people, err := db.ResolveGroupMembers(kids)
	if err != nil {
		t.Fatalf("ResolveGroupMembers: %v", err)
	}
	if len(people) != 0 {
		t.Errorf("the child preset put somebody in Kids: %v — the preset constrains "+
			"what a Member may do, the group is a set of people, and neither "+
			"creates the other", people)
	}

	// And the converse: joining Kids does not apply the preset.
	other := personOf(t, db, "alex", RoleMember)
	if err := db.AddPersonToGroup(kids, other, "mika"); err != nil {
		t.Fatal(err)
	}
	m, err := db.GetButlerMember("alex")
	if err != nil {
		t.Fatal(err)
	}
	if m.Profile != "" {
		t.Errorf("joining Kids applied the child preset (%q); a group cannot "+
			"constrain what somebody may do", m.Profile)
	}
}

// One resolver, asserted by construction (#207 allows a grep-based
// test, and this is one).
//
// The risk is not a style violation. Two implementations of "who is in
// this group" are two answers, differing on some edge — an offboarded
// person, say — that nobody notices until one of the three consumers
// disagrees with the other two about who counts as an adult.
func TestOneResolverServesEveryConsumer(t *testing.T) {
	src := readSourceFile(t, "persongroups.go")
	// Every membership question must be answered by calling the
	// resolver, so the table is READ in exactly one place.
	//
	// Reads only. The first version of this counted every "FROM
	// person_group_members" and caught the DELETE in
	// RemovePersonFromGroup — which is a write, and writes are not
	// competing definitions of who is in a group. A test that fires on
	// the wrong thing gets edited until it stops firing, and what gets
	// edited away is usually the part that was right.
	reads := strings.Count(src, "FROM person_group_members") -
		strings.Count(src, "DELETE FROM person_group_members")
	if reads != 1 {
		t.Errorf("person_group_members is read in %d places; membership has one "+
			"definition and must have one implementation", reads)
	}
	// The two conveniences must be built on it rather than beside it.
	for _, fn := range []string{"PersonIsInGroup", "GroupsForPerson"} {
		body := functionBody(t, src, fn)
		if !strings.Contains(body, "ResolveGroupMembers") && !strings.Contains(body, "PersonIsInGroup") {
			t.Errorf("%s does not go through the resolver; it is a second "+
				"definition of membership", fn)
		}
	}
}

// The same group resolves to the same people however it is asked
// (§18.1). Today the three consumers do not exist, so the claim is
// tested where it can be: every route into membership agrees.
func TestGroupsResolveIdenticallyAcrossConsumers(t *testing.T) {
	db := open(t)
	a := personOf(t, db, "mika", RoleOwner)
	b := personOf(t, db, "giulia", RoleMember)
	adults := groupID(t, db, SeedAdults)
	for _, p := range []string{a, b} {
		if err := db.AddPersonToGroup(adults, p, "mika"); err != nil {
			t.Fatal(err)
		}
	}

	members, err := db.ResolveGroupMembers(adults)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("ResolveGroupMembers = %v, want both", members)
	}
	for _, p := range members {
		in, err := db.PersonIsInGroup(p, adults)
		if err != nil {
			t.Fatal(err)
		}
		if !in {
			t.Errorf("%s is in the resolved set but PersonIsInGroup says no", p)
		}
		groups, err := db.GroupsForPerson(p)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, g := range groups {
			if g.ID == adults {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is in the resolved set but GroupsForPerson omits the group", p)
		}
	}
}

// Renaming a group does not re-point the rules that named it (§18.1).
//
// This is why a group has an id at all. A rule that stored the word
// "Adults" would follow the rename to whatever the word now means,
// which is the silent kind of authorization change.
func TestGroupRenameDoesNotRepointRules(t *testing.T) {
	db := open(t)
	person := personOf(t, db, "mika", RoleOwner)
	adults := groupID(t, db, SeedAdults)
	if err := db.AddPersonToGroup(adults, person, "mika"); err != nil {
		t.Fatal(err)
	}

	if err := db.RenameGroup(adults, "Grown-ups"); err != nil {
		t.Fatalf("RenameGroup: %v", err)
	}

	// The id a rule would have stored still resolves to the same people.
	people, err := db.ResolveGroupMembers(adults)
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 1 || people[0] != person {
		t.Errorf("after the rename the id resolves to %v, want [%s]", people, person)
	}
	// And the seed key still finds it, because it names the role rather
	// than following the household's word for it.
	g, err := db.GroupBySeedKey(SeedAdults)
	if err != nil {
		t.Fatalf("GroupBySeedKey after rename: %v", err)
	}
	if g.ID != adults {
		t.Errorf("the seed key now points at %s, want %s", g.ID, adults)
	}
	if g.Name != "Grown-ups" {
		t.Errorf("the rename did not take: %q", g.Name)
	}
}

// Removal takes effect at the next evaluation, not on a cache expiry
// (#207).
func TestRemovalTakesEffectImmediately(t *testing.T) {
	db := open(t)
	person := personOf(t, db, "giulia", RoleMember)
	adults := groupID(t, db, SeedAdults)
	if err := db.AddPersonToGroup(adults, person, "mika"); err != nil {
		t.Fatal(err)
	}
	if in, _ := db.PersonIsInGroup(person, adults); !in {
		t.Fatal("the person was never in the group")
	}

	if err := db.RemovePersonFromGroup(adults, person); err != nil {
		t.Fatalf("RemovePersonFromGroup: %v", err)
	}

	// The very next evaluation, with nothing invalidated in between.
	if in, err := db.PersonIsInGroup(person, adults); err != nil || in {
		t.Errorf("still in the group after removal (in=%v err=%v); a cache here "+
			"would leave a removed person with access for as long as the entry "+
			"lived", in, err)
	}
}

// A Person in several groups resolves to the union, not a winner
// (#207). §4's own examples put somebody in `Adults` and `Media` at
// once.
func TestAPersonInSeveralGroupsResolvesToTheUnion(t *testing.T) {
	db := open(t)
	person := personOf(t, db, "mika", RoleOwner)
	media, err := db.CreateGroup("Media")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	for _, id := range []string{groupID(t, db, SeedAdults), groupID(t, db, SeedFamily), media.ID} {
		if err := db.AddPersonToGroup(id, person, "mika"); err != nil {
			t.Fatal(err)
		}
	}

	groups, err := db.GroupsForPerson(person)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 3 {
		t.Errorf("GroupsForPerson = %d groups, want all 3 — resolution returns the "+
			"set, not a winner", len(groups))
	}
	// A household group gets no seed key: nothing in Wildstone should
	// have an opinion about a group it did not define.
	if media.SeedKey != "" {
		t.Errorf("a household-made group was given the seed key %q", media.SeedKey)
	}
}

// Somebody who is not active is not resolved, even though the record
// of their membership stays.
//
// Every caller of the resolver is about to make an authorization
// decision, and counting an offboarded person as an adult is the
// failure that ordering avoids. The row remains because it is the
// household's record of what they were in, which offboarding (slice 23)
// will want.
func TestAnInactivePersonIsNotResolved(t *testing.T) {
	db := open(t)
	// Two Owners: the Last Owner invariant refuses to disable the only
	// active one, so the household needs a second before this is even
	// reachable.
	keeper := personOf(t, db, "mika", RoleOwner)
	leaver := personOf(t, db, "sam", RoleOwner)
	adults := groupID(t, db, SeedAdults)
	for _, p := range []string{keeper, leaver} {
		if err := db.AddPersonToGroup(adults, p, "mika"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DisableButlerMember("sam", "mika"); err != nil {
		t.Fatalf("DisableButlerMember: %v", err)
	}

	people, err := db.ResolveGroupMembers(adults)
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 1 || people[0] != keeper {
		t.Errorf("resolved %v, want only the active Owner — a disabled person "+
			"counted as an adult is the failure this ordering avoids", people)
	}
	// The record survives, because it is a record. Counted in the
	// ADULTS group specifically: since slice 8 an admitted Person is
	// also in `Family`, and a count over every group would pass on the
	// wrong row.
	var n int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM person_group_members WHERE person_id = ? AND group_id = ?`,
		leaver, adults).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the membership row was deleted (%d); disabling is a state, not a "+
			"removal, and the household's record of what they were in should stand", n)
	}
}

// The seeds §4 names exist, empty, and are reachable by role.
func TestTheDesignsGroupsAreSeededEmpty(t *testing.T) {
	db := open(t)
	for _, key := range []string{SeedFamily, SeedAdults, SeedKids} {
		g, err := db.GroupBySeedKey(key)
		if err != nil {
			t.Fatalf("GroupBySeedKey(%s): %v", key, err)
		}
		people, err := db.ResolveGroupMembers(g.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(people) != 0 {
			t.Errorf("%s was seeded with members %v; who is in a group is the "+
				"household's to say", key, people)
		}
	}
	if _, err := db.GroupBySeedKey("nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown seed key resolved: %v", err)
	}
}

// A lookup that failed is not a lookup that found nothing.
//
// My own review pass found this: GroupBySeedKey folded every error into
// ErrNotFound, so a database that was briefly unreadable reported that
// the adults group does not exist. The caller's reasonable response to
// "not found" is to create one, which would leave a household with two
// groups both claiming the same role — and the seed_key UNIQUE
// constraint would then refuse the second, turning a transient read
// failure into a permanent write failure somewhere else entirely.
func TestAFailedGroupLookupIsNotAMissingGroup(t *testing.T) {
	db := open(t)

	// Closed, so the query fails for a reason that is not "no rows".
	db.Close()

	_, err := db.GroupBySeedKey(SeedAdults)
	if err == nil {
		t.Fatal("a lookup on a closed database succeeded")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a failed lookup reported the group as missing; a caller that " +
			"creates one on ErrNotFound would give the household two groups " +
			"claiming the same role")
	}
}

// The namespace cannot hold a membership for somebody who does not
// exist.
//
// Enforced by the schema rather than by a check in Go, which is what
// makes it true of every write path including a future one. Worth a
// test because it depends on foreign_keys being ON — a pragma set in
// the DSN, far from this file, and silently a no-op if it were ever
// dropped.
func TestAGroupCannotHoldAPersonWhoDoesNotExist(t *testing.T) {
	db := open(t)
	adults := groupID(t, db, SeedAdults)

	if err := db.AddPersonToGroup(adults, "psn_nobody", "mika"); err == nil {
		t.Error("a group accepted a person_id with no Person behind it; " +
			"membership would then resolve to an identity nothing else knows")
	}
	// And the converse: a real Person cannot join a group that is gone.
	person := personOf(t, db, "mika", RoleOwner)
	if err := db.AddPersonToGroup("grp_nothing", person, "mika"); err == nil {
		t.Error("a Person joined a group that does not exist")
	}
}

func TestCreateGroupRefusesEmptyAndDuplicateNames(t *testing.T) {
	db := open(t)
	if _, err := db.CreateGroup("   "); !errors.Is(err, ErrGroupNameEmpty) {
		t.Fatalf("blank name: got %v, want ErrGroupNameEmpty", err)
	}
	g, err := db.CreateGroup("  Neighbours ")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if g.Name != "Neighbours" || g.SeedKey != "" {
		t.Fatalf("created %+v, want trimmed name and no seed key", g)
	}
	if _, err := db.CreateGroup("neighbours"); !errors.Is(err, ErrGroupNameTaken) {
		t.Fatalf("case-variant name: got %v, want ErrGroupNameTaken", err)
	}
	groups, err := db.ListGroups()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range groups {
		found = found || x.ID == g.ID
	}
	if !found {
		t.Fatal("created group is not listed")
	}
}

func TestDeleteGroupRefusesSeededAndUnknown(t *testing.T) {
	db := open(t)
	if err := db.DeleteGroup(groupID(t, db, SeedAdults)); !errors.Is(err, ErrSeededGroup) {
		t.Fatalf("seeded: got %v, want ErrSeededGroup", err)
	}
	if err := db.DeleteGroup("grp_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: got %v, want ErrNotFound", err)
	}
	if _, err := db.GroupBySeedKey(SeedAdults); err != nil {
		t.Fatalf("seeded group disappeared: %v", err)
	}
}

func TestDeleteGroupRefusesWhileReferenced(t *testing.T) {
	owner := personOf // keep the helper in scope for the secret case below
	refs := map[string]func(t *testing.T, db *DB, group string){
		"entitlement": func(t *testing.T, db *DB, group string) {
			if _, err := db.sql.Exec(
				`INSERT INTO service_entitlements (service, subject_kind, subject, effect, created_at)
				 VALUES ('photos', 'group', ?, 'allow', 'now')`, group); err != nil {
				t.Fatal(err)
			}
		},
		"audience": func(t *testing.T, db *DB, group string) {
			if _, err := db.sql.Exec(
				`INSERT INTO audiences (audience_id, kind, created_at) VALUES ('aud_t', 'set', 'now')`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.sql.Exec(
				`INSERT INTO audience_members (audience_id, kind, ref) VALUES ('aud_t', 'group', ?)`, group); err != nil {
				t.Fatal(err)
			}
		},
		"secret grant": func(t *testing.T, db *DB, group string) {
			person := owner(t, db, "mika", RoleOwner)
			if _, err := db.sql.Exec(
				`INSERT INTO household_secrets (id, scope, owner_person_id, label, state, ciphertext, created_at)
				 VALUES ('sec_t', 'household', ?, 'l', 'active', x'00', 'now')`, person); err != nil {
				t.Fatal(err)
			}
			if _, err := db.sql.Exec(
				`INSERT INTO household_secret_grants (secret_id, group_id, verb) VALUES ('sec_t', ?, 'use')`, group); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, add := range refs {
		t.Run(name, func(t *testing.T) {
			db := open(t)
			g, err := db.CreateGroup("Book club")
			if err != nil {
				t.Fatal(err)
			}
			add(t, db, g.ID)
			if err := db.DeleteGroup(g.ID); !errors.Is(err, ErrGroupInUse) {
				t.Fatalf("got %v, want ErrGroupInUse", err)
			}
			groups, _ := db.ListGroups()
			for _, x := range groups {
				if x.ID == g.ID {
					return
				}
			}
			t.Fatal("group was deleted despite the refusal")
		})
	}
}

func TestDeleteGroupRemovesUnreferencedGroupAndMembers(t *testing.T) {
	db := open(t)
	person := personOf(t, db, "giulia", RoleMember)
	g, err := db.CreateGroup("Book club")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddPersonToGroup(g.ID, person, "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteGroup(g.ID); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	groups, _ := db.ListGroups()
	for _, x := range groups {
		if x.ID == g.ID {
			t.Fatal("group still listed")
		}
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM person_group_members WHERE group_id = ?`, g.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("memberships left behind: n=%d err=%v", n, err)
	}
	if _, err := db.GetButlerMember("giulia"); err != nil {
		t.Fatalf("deleting a group touched the person: %v", err)
	}
}
