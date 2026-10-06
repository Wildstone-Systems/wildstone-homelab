package runtimedb

// The Person core: stable identity, the role/preset split, and the Last
// Owner invariant (design §2.2, §2.3, §2.5, §18.1).
//
// The negative cases carry this file. A Last-Owner rule tested only by
// promoting a second Owner and watching the first demote successfully
// is not tested at all — it would pass against an implementation that
// never refused anything.

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// soleOwner returns a database whose only active Owner is "mika",
// with "sam" an ordinary Member alongside.
func soleOwner(t *testing.T) *DB {
	t.Helper()
	db := open(t)
	admitted(t, db, "mika", RoleOwner)
	admitted(t, db, "sam", RoleMember)
	return db
}

func roleOf(t *testing.T, db *DB, member string) string {
	t.Helper()
	m, err := db.GetButlerMember(member)
	if err != nil {
		t.Fatalf("GetButlerMember(%s): %v", member, err)
	}
	return m.Role
}

func TestLastOwnerCannotBeDemoted(t *testing.T) {
	db := soleOwner(t)
	err := db.SetButlerMemberRole("mika", RoleMember, "root-admin")
	if !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demoting the last owner: got %v, want ErrLastOwner", err)
	}
	if got := roleOf(t, db, "mika"); got != RoleOwner {
		t.Fatalf("role after refused demotion = %q, want owner", got)
	}
	// The refusal must not have written an audit row either: nothing
	// happened, so nothing is recorded as having happened.
	m, err := db.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	changes, err := db.ListButlerRoleChanges(m.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("a refused demotion wrote %d audit rows", len(changes))
	}
}

// Disable and offboard strip Owner authority as completely as a
// demotion does, so the invariant covers each lifecycle operation.
func TestLastOwnerCannotBeDisabledOrSoftDeleted(t *testing.T) {
	for _, op := range []struct {
		name string
		run  func(db *DB) error
	}{
		{"disable", func(db *DB) error { return db.DisableButlerMember("mika", "root-admin") }},
		{"offboard", func(db *DB) error { return db.OffboardButlerMember("mika", "root-admin") }},
	} {
		t.Run(op.name, func(t *testing.T) {
			db := soleOwner(t)
			if err := op.run(db); !errors.Is(err, ErrLastOwner) {
				t.Fatalf("%s of the last owner: got %v, want ErrLastOwner", op.name, err)
			}
			m, err := db.GetButlerMember("mika")
			if err != nil {
				t.Fatal(err)
			}
			if !m.Active() || m.Role != RoleOwner {
				t.Fatalf("last owner after refused %s = %+v", op.name, m)
			}
		})
	}
}

func TestSecondOwnerReleasesTheInvariant(t *testing.T) {
	db := soleOwner(t)
	// While sam is a Member, mika is stuck.
	if err := db.SetButlerMemberRole("mika", RoleMember, "root-admin"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("precondition: got %v, want ErrLastOwner", err)
	}
	if err := db.SetButlerMemberRole("sam", RoleOwner, "root-admin"); err != nil {
		t.Fatalf("promoting a second owner: %v", err)
	}
	if err := db.SetButlerMemberRole("mika", RoleMember, "root-admin"); err != nil {
		t.Fatalf("demoting with a second owner present: %v", err)
	}
	if got := roleOf(t, db, "mika"); got != RoleMember {
		t.Fatalf("role after demotion = %q, want member", got)
	}
	// And now sam is the one who cannot leave.
	if err := db.SetButlerMemberRole("sam", RoleMember, "root-admin"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("the invariant did not follow the remaining owner: %v", err)
	}
}

// The Go write path gives the readable error; the schema is what
// actually holds. This bypasses the write path entirely, the way a
// console form or a repair script written next year would.
func TestLastOwnerInvariantIsEnforcedBySchema(t *testing.T) {
	for _, raw := range []struct {
		name  string
		query string
	}{
		{"demote", `UPDATE butler_members SET role = 'member' WHERE member = 'mika'`},
		{"disable", `UPDATE butler_members SET disabled_at = 't', admitted = 0 WHERE member = 'mika'`},
		{"offboard", `UPDATE butler_members SET offboarded_at = 't', admitted = 0 WHERE member = 'mika'`},
		{"unadmit", `UPDATE butler_members SET admitted = 0 WHERE member = 'mika'`},
		{"delete", `DELETE FROM butler_members WHERE member = 'mika'`},
	} {
		t.Run(raw.name, func(t *testing.T) {
			db := soleOwner(t)
			_, err := db.sql.Exec(raw.query)
			if err == nil {
				t.Fatalf("a raw %s stripped the last owner", raw.name)
			}
			if !strings.Contains(err.Error(), "last active owner") {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := roleOf(t, db, "mika"); got != RoleOwner {
				t.Fatalf("role after refused raw %s = %q", raw.name, got)
			}
			m, err := db.GetButlerMember("mika")
			if err != nil {
				t.Fatal(err)
			}
			if !m.Active() {
				t.Fatalf("last owner deactivated by raw %s: %+v", raw.name, m)
			}
		})
	}
}

// Two Owners demoted at the same time must not both succeed. Whichever
// transaction commits second finds itself demoting the last one.
func TestConcurrentDemotionOfTwoOwnersLeavesOne(t *testing.T) {
	db := open(t)
	admitted(t, db, "mika", RoleOwner)
	admitted(t, db, "dana", RoleOwner)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i, who := range []string{"mika", "dana"} {
		wg.Add(1)
		go func(i int, who string) {
			defer wg.Done()
			<-start
			errs[i] = db.SetButlerMemberRole(who, RoleMember, "root-admin")
		}(i, who)
	}
	close(start)
	wg.Wait()

	var failed, succeeded int
	for _, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		failed++
		if !errors.Is(err, ErrLastOwner) {
			t.Fatalf("the losing demotion failed for the wrong reason: %v", err)
		}
	}
	if succeeded != 1 || failed != 1 {
		t.Fatalf("concurrent demotions: %d succeeded, %d refused; want exactly one of each (%v)", succeeded, failed, errs)
	}
	var owners int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM butler_members
		 WHERE role = 'owner' AND admitted = 1 AND disabled_at = '' AND offboarded_at = ''`).
		Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if owners != 1 {
		t.Fatalf("active owners after concurrent demotion = %d, want 1", owners)
	}
}

// A database written at the 0021 shape migrates to the split columns,
// and the legacy profile maps as specified.
func TestMigrationMapsLegacyProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	old, err := openThrough(t, path, "migrations/0021_butler_identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	// One row of each legacy profile, at the old schema.
	if _, err := old.sql.Exec(
		`INSERT INTO butler_members (member, profile, admitted, generation, created_at, guardian) VALUES
		   ('mika', 'operator',  1, 3, '2026-01-01T00:00:00Z', ''),
		   ('sam',   'household', 1, 1, '2026-01-02T00:00:00Z', ''),
		   ('kid',   'child',     0, 1, '2026-01-03T00:00:00Z', 'mika')`); err != nil {
		t.Fatalf("seeding legacy rows: %v", err)
	}
	// A link, so the migration has a foreign key to preserve.
	if _, err := old.sql.Exec(
		`INSERT INTO butler_identity_links (link_type, external_id, member, generation, created_at, created_by)
		 VALUES ('api-principal', 'mika-api', 'mika', 3, '2026-01-01T00:00:00Z', 'root-admin')`); err != nil {
		t.Fatalf("seeding a link: %v", err)
	}
	old.Close()

	db, err := Open(path) // applies 0022..0039
	if err != nil {
		t.Fatalf("Open (applies 0039): %v", err)
	}
	defer db.Close()

	want := map[string]struct{ role, profile string }{
		"mika": {RoleOwner, ""},
		"sam":   {RoleMember, ""},
		"kid":   {RoleMember, ProfileChild},
	}
	seen := map[string]bool{}
	for member, expect := range want {
		m, err := db.GetButlerMember(member)
		if err != nil {
			t.Fatalf("GetButlerMember(%s): %v", member, err)
		}
		if m.Role != expect.role || m.Profile != expect.profile {
			t.Errorf("%s: role=%q profile=%q, want role=%q profile=%q",
				member, m.Role, m.Profile, expect.role, expect.profile)
		}
		if m.PersonID == "" {
			t.Errorf("%s: person_id was not populated", member)
		}
		if seen[m.PersonID] {
			t.Errorf("%s: person_id %q is not distinct", member, m.PersonID)
		}
		seen[m.PersonID] = true
	}
	// Everything else about the rows survived the rebuild.
	mika, err := db.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	if mika.Generation != 3 || !mika.Admitted {
		t.Errorf("mika lost state in the rebuild: %+v", mika)
	}
	kid, err := db.GetButlerMember("kid")
	if err != nil {
		t.Fatal(err)
	}
	if kid.Guardian != "mika" || kid.Active() {
		t.Errorf("kid lost state in the rebuild: %+v", kid)
	}
	// The foreign key still points somewhere real.
	resolved, err := db.ResolveSurfaceIdentity(LinkAPIPrincipal, "mika-api")
	if err != nil {
		t.Fatalf("a link written before the migration stopped resolving: %v", err)
	}
	if resolved.Member != "mika" {
		t.Errorf("link resolved to %q, want mika", resolved.Member)
	}
	// And the migration left no orphans behind.
	orphans, err := db.sql.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer orphans.Close()
	if orphans.Next() {
		t.Error("the migration left orphaned foreign key rows")
	}
}

// A child is a constrained Member, not a third role (§2.3). Nothing —
// not the store, not the schema — may produce role = 'child'.
func TestChildPresetIsAConstrainedMemberNotARole(t *testing.T) {
	db := open(t)
	if err := db.RecordButlerMember("mika", RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordButlerMember("kid", RoleMember, ProfileChild, "mika"); err != nil {
		t.Fatalf("recording a child: %v", err)
	}
	kid, err := db.GetButlerMember("kid")
	if err != nil {
		t.Fatal(err)
	}
	if kid.Role != RoleMember || kid.Profile != ProfileChild {
		t.Fatalf("child = role %q profile %q, want role member profile child", kid.Role, kid.Profile)
	}
	// The store refuses 'child' as a role.
	if err := db.RecordButlerMember("kid2", ProfileChild, "", "mika"); err == nil {
		t.Error("a person was recorded with role 'child'")
	}
	if err := db.SetButlerMemberRole("kid", ProfileChild, "root-admin"); err == nil {
		t.Error("a role was set to 'child'")
	}
	// A child cannot be promoted to Owner: the preset constrains a
	// Member, and an Owner is not one.
	if err := db.SetButlerMemberRole("kid", RoleOwner, "root-admin"); err == nil {
		t.Error("a child was promoted to owner")
	}
	// And the schema refuses both directly.
	if _, err := db.sql.Exec(
		`INSERT INTO butler_members (person_id, member, role, profile, admitted, generation, created_at, guardian)
		 VALUES ('psn_x', 'kid3', 'child', NULL, 0, 1, 't', '')`); err == nil {
		t.Error("the schema accepted role = 'child'")
	}
	if _, err := db.sql.Exec(
		`INSERT INTO butler_members (person_id, member, role, profile, admitted, generation, created_at, guardian)
		 VALUES ('psn_y', 'kid4', 'owner', 'child', 0, 1, 't', 'mika')`); err == nil {
		t.Error("the schema accepted an owner carrying the child preset")
	}
}

func TestHandleRenamePreservesPersonID(t *testing.T) {
	db := open(t)
	admitted(t, db, "mika", RoleOwner)
	if err := db.LinkSurfaceIdentity(LinkAPIPrincipal, "mika-api", "mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	conv, err := db.StartButlerConversation("mika", "nodes")
	if err != nil {
		t.Fatal(err)
	}
	before, err := db.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}

	if err := db.RenameButlerMemberHandle("mika", "mikhail"); err != nil {
		t.Fatalf("RenameButlerMemberHandle: %v", err)
	}

	after, err := db.GetButlerMember("mikhail")
	if err != nil {
		t.Fatalf("the renamed person did not resolve: %v", err)
	}
	if after.PersonID != before.PersonID {
		t.Fatalf("person_id moved on rename: %q -> %q", before.PersonID, after.PersonID)
	}
	if after.Role != RoleOwner || after.Generation != before.Generation {
		t.Errorf("rename changed more than the handle: %+v", after)
	}
	if _, err := db.GetButlerMember("mika"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old handle still resolves: %v", err)
	}
	// The references that are foreign keys still resolve.
	resolved, err := db.ResolveSurfaceIdentity(LinkAPIPrincipal, "mika-api")
	if err != nil {
		t.Fatalf("the identity link stopped resolving after a rename: %v", err)
	}
	if resolved.PersonID != before.PersonID {
		t.Errorf("the link resolved to a different person: %+v", resolved)
	}
	got, err := db.GetButlerConversation(conv)
	if err != nil {
		t.Fatalf("the conversation stopped resolving after a rename: %v", err)
	}
	if got.Member != "mikhail" {
		t.Errorf("conversation member = %q, want mikhail", got.Member)
	}
	// The invariant followed the person, not the handle.
	if err := db.SetButlerMemberRole("mikhail", RoleMember, "root-admin"); !errors.Is(err, ErrLastOwner) {
		t.Errorf("the last-owner invariant lost track of a renamed owner: %v", err)
	}
}

func TestRoleChangeIsAudited(t *testing.T) {
	db := soleOwner(t)
	sam, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerMemberRole("sam", RoleOwner, "mika"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if err := db.SetButlerMemberRole("sam", RoleMember, "root-admin"); err != nil {
		t.Fatalf("demote: %v", err)
	}
	changes, err := db.ListButlerRoleChanges(sam.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("role changes = %d, want 2: %+v", len(changes), changes)
	}
	want := []struct{ actor, old, new string }{
		{"mika", RoleMember, RoleOwner},
		{"root-admin", RoleOwner, RoleMember},
	}
	for i, w := range want {
		got := changes[i]
		if got.Actor != w.actor || got.OldRole != w.old || got.NewRole != w.new {
			t.Errorf("change %d = actor %q %s->%s, want actor %q %s->%s",
				i, got.Actor, got.OldRole, got.NewRole, w.actor, w.old, w.new)
		}
		if got.PersonID != sam.PersonID || got.Member != "sam" {
			t.Errorf("change %d names the wrong subject: %+v", i, got)
		}
		if got.At.IsZero() {
			t.Errorf("change %d has no timestamp", i)
		}
	}
	// A role change needs an actor, and a no-op writes nothing.
	if err := db.SetButlerMemberRole("sam", RoleOwner, ""); err == nil {
		t.Error("a role change was accepted with no actor")
	}
	if err := db.SetButlerMemberRole("sam", RoleMember, "root-admin"); err != nil {
		t.Fatalf("no-op role change: %v", err)
	}
	after, err := db.ListButlerRoleChanges(sam.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Errorf("a no-op role change wrote an audit row: %+v", after)
	}
	// The trail is append-only, which takes both halves: erasing a row
	// and rewriting it leave the trail in the same state, so refusing
	// only the rewrite would not make it append-only.
	if _, err := db.sql.Exec(`UPDATE butler_role_changes SET actor = 'someone-else'`); err == nil {
		t.Error("a role change record was rewritten")
	}
	if _, err := db.sql.Exec(`DELETE FROM butler_role_changes`); err == nil {
		t.Error("role change records were deleted")
	}
	if _, err := db.sql.Exec(
		`DELETE FROM butler_role_changes WHERE person_id = ?`, sam.PersonID); err == nil {
		t.Error("a single role change record was deleted")
	}
	// Still all there after both attempts.
	surviving, err := db.ListButlerRoleChanges(sam.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(surviving) != 2 {
		t.Errorf("audit rows after refused edits = %d, want 2", len(surviving))
	}
}

// Staging a person must not be a second way to cross the Owner
// boundary. RecordButlerMember is reachable by anyone who can manage
// members, and if it could promote, every audit of "who made this
// person an Owner" would have a hole in it.
func TestRecordingAPersonCannotChangeTheirRole(t *testing.T) {
	db := open(t)
	admitted(t, db, "sam", RoleMember)
	before, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordButlerMember("sam", RoleOwner, "", ""); err != nil {
		t.Fatalf("re-recording: %v", err)
	}
	after, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if after.Role != RoleMember {
		t.Errorf("a staging call promoted sam to %q", after.Role)
	}
	// And the identity is the same person throughout.
	if after.PersonID != before.PersonID {
		t.Errorf("person_id changed on re-record: %q -> %q", before.PersonID, after.PersonID)
	}
	changes, err := db.ListButlerRoleChanges(before.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("a staging call wrote %d role-change rows", len(changes))
	}
}

// The invariant counts ACTIVE Owners, so it must not fire for an Owner
// who is already disabled: demoting them costs the household nothing. A
// rule that refused here would strand every inactive Owner record.
func TestInactiveOwnerIsNotTheLastOwner(t *testing.T) {
	db := open(t)
	admitted(t, db, "mika", RoleOwner)
	admitted(t, db, "dana", RoleOwner)
	if err := db.DisableButlerMember("dana", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerMemberRole("dana", RoleMember, "root-admin"); err != nil {
		t.Fatalf("demoting an already-disabled owner: %v", err)
	}
	// mika is now genuinely the last one.
	if err := db.SetButlerMemberRole("mika", RoleMember, "root-admin"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("got %v, want ErrLastOwner", err)
	}
}

// Bootstrap is the installer's job (§2.4). The migration must not
// invent an Owner for a household that has none, and the invariant is
// about not LOSING the last one rather than about always having one.
func TestMigrationDoesNotFabricateAnOwner(t *testing.T) {
	db := open(t)
	admitted(t, db, "sam", RoleMember)
	var owners int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM butler_members WHERE role = 'owner'`).Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if owners != 0 {
		t.Fatalf("the schema fabricated %d owners", owners)
	}
}

// person_id is the identity, so the schema refuses to move it. A
// comment saying "stable" is a convention; a trigger is an identity.
//
// The concrete cost of a rewrite is the audit trail: butler_role_changes
// is keyed on person_id, so changing it would strand the history under
// an id nothing resolves to. Every table slice 2 repoints onto person_id
// would detach the same way.
func TestPersonIDCannotBeRewritten(t *testing.T) {
	db := soleOwner(t)
	before, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerMemberRole("sam", RoleOwner, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(
		`UPDATE butler_members SET person_id = 'psn_rewritten' WHERE member = 'sam'`); err == nil {
		t.Fatal("person_id was rewritten")
	} else if !strings.Contains(err.Error(), "stable identity") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	after, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if after.PersonID != before.PersonID {
		t.Fatalf("person_id moved: %q -> %q", before.PersonID, after.PersonID)
	}
	// The audit trail still belongs to somebody.
	trail, err := db.ListButlerRoleChanges(after.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(trail) != 1 {
		t.Errorf("role history under the current person_id = %d rows, want 1", len(trail))
	}

	// The supported operations still work: a rename moves the handle and
	// not the identity, and an ordinary update that leaves person_id
	// alone is not caught by the guard.
	if err := db.RenameButlerMemberHandle("sam", "samantha"); err != nil {
		t.Fatalf("rename after the guard: %v", err)
	}
	renamed, err := db.GetButlerMember("samantha")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.PersonID != before.PersonID {
		t.Errorf("rename moved person_id: %q -> %q", before.PersonID, renamed.PersonID)
	}
	if err := db.SetButlerMemberRole("samantha", RoleMember, "mika"); err != nil {
		t.Errorf("role change after the guard: %v", err)
	}
	// A no-op write of the same value is not a change and must pass.
	if _, err := db.sql.Exec(
		`UPDATE butler_members SET person_id = person_id WHERE member = 'samantha'`); err != nil {
		t.Errorf("a no-op person_id write was refused: %v", err)
	}
}
