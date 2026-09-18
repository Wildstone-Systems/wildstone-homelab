package runtimedb

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Connection admission (ADR-W033 A2, B9, B10; design §8.3).
//
// A Connection admits a Person. Before migration 0041 an admin-surface
// binding named a Keystone PRINCIPAL — `op`, `root-admin` — because
// principal/chatclaim.go required one, so a chat account reached an
// Owner Agent as a credential with nobody behind it. These tests pin
// what replaced that, and the negative cases are the point: the
// configuration check alone would leave every one of them open.

// bind puts a live binding in place the way a redeemed claim does.
func bind(t *testing.T, db *DB, handle, surface, account string) ChatBinding {
	t.Helper()
	hash := "hash-" + handle + "-" + surface + "-" + account
	if err := db.InsertChatClaimCode(hash, ChatClaimCode{
		Surface:   ButlerSurface(surface),
		Identity:  handle,
		CreatedBy: "root-admin",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("issuing a code for %q: %v", handle, err)
	}
	b, err := db.RedeemChatClaimCode(hash, "telegram", account, handle, "conv-1", ButlerSurface(surface))
	if err != nil {
		t.Fatalf("redeeming for %q: %v", handle, err)
	}
	return b
}

// An owner keeps their admin-surface Connection; the binding names them
// by identity, so the handle it reads back is whatever they carry now.
func TestARenamedPersonKeepsTheirConnection(t *testing.T) {
	db := open(t)
	record(t, db, "mika")
	if err := db.SetButlerMemberRole("mika", RoleOwner, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	bind(t, db, "mika", "admin", "12345")

	if err := db.RenameButlerMemberHandle("mika", "micah"); err != nil {
		t.Fatalf("renaming: %v", err)
	}
	b, err := db.LookupChatBinding("telegram", "12345", ButlerSurfaceAdmin)
	if err != nil {
		t.Fatalf("the connection was lost in a rename: %v", err)
	}
	if b.Identity != "micah" {
		t.Fatalf("binding identity = %q, want the handle they carry now", b.Identity)
	}
}

// The runtime half of B9, and the reason the configuration half is not
// enough on its own: "Owner-Agent Connections admit Owners only.
// Configuration and runtime both enforce this."
//
// Demotion is immediate (A6). A binding issued while somebody was an
// Owner must stop resolving the moment they are not — otherwise the
// Console rule that a demoted Owner loses the control plane holds
// everywhere except the transport they were already talking on.
func TestDemotingAnOwnerClosesTheirAdminConnection(t *testing.T) {
	db := open(t)
	record(t, db, "mika", "sam")
	for _, h := range []string{"mika", "sam"} {
		if err := db.SetButlerMemberRole(h, RoleOwner, "root-admin"); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember(h, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	bind(t, db, "sam", "admin", "12345")
	if _, err := db.LookupChatBinding("telegram", "12345", ButlerSurfaceAdmin); err != nil {
		t.Fatalf("the fixture never had a working binding: %v", err)
	}

	// mika is the other active Owner, so the Last Owner invariant does
	// not stand in the way of demoting sam.
	if err := db.SetButlerMemberRole("sam", RoleMember, "root-admin"); err != nil {
		t.Fatalf("demoting: %v", err)
	}
	if _, err := db.LookupChatBinding("telegram", "12345", ButlerSurfaceAdmin); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a demoted owner still reaches an owner agent over chat: err = %v", err)
	}
}

// The same for the other two ways a Person stops being admitted, since
// a rule that only catches demotion is not the rule it claims to be.
func TestAnOffboardedPersonReachesNoConnection(t *testing.T) {
	db := open(t)
	record(t, db, "sam")
	if err := db.AdmitButlerMember("sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	bind(t, db, "sam", "family", "12345")
	if _, err := db.LookupChatBinding("telegram", "12345", ButlerSurfaceFamily); err != nil {
		t.Fatalf("the fixture never had a working binding: %v", err)
	}
	if err := db.OffboardButlerMember("sam", "root-admin"); err != nil {
		t.Fatalf("offboarding: %v", err)
	}
	if _, err := db.LookupChatBinding("telegram", "12345", ButlerSurfaceFamily); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an offboarded person still reaches their agent over chat: err = %v", err)
	}
}

// A family-surface Connection is unaffected by the Owner rule: it
// admits a Member, which is what the great majority of them are. The
// converse matters because the obvious over-correction — admit Owners
// everywhere — would lock the household out of its own agents.
func TestAFamilyConnectionAdmitsAMember(t *testing.T) {
	db := open(t)
	record(t, db, "sam")
	if err := db.AdmitButlerMember("sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	bind(t, db, "sam", "family", "12345")
	b, err := db.LookupChatBinding("telegram", "12345", ButlerSurfaceFamily)
	if err != nil {
		t.Fatalf("a member cannot hold a family connection: %v", err)
	}
	if b.Identity != "sam" {
		t.Fatalf("binding identity = %q, want sam", b.Identity)
	}
}

// A claim code for somebody who is not on record is refused where it is
// written, not where it is redeemed.
func TestAClaimCodeNamesAPersonOnRecord(t *testing.T) {
	db := open(t)
	err := db.InsertChatClaimCode("hash-ghost", ChatClaimCode{
		Surface: ButlerSurfaceFamily, Identity: "ghost", CreatedBy: "root-admin",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if !errors.Is(err, ErrNoSuchPerson) {
		t.Fatalf("a code was issued for nobody: err = %v", err)
	}
}

// Migration 0041 refuses a deployment still admitting a machine
// principal, rather than migrating it into something that looks like a
// person.
//
// This is the case the whole slice exists for. Under the superseded
// model, `IssueChatClaim` REQUIRED a Keystone principal for an
// admin-surface code, so a deployment that bound the operator's own
// Telegram account has exactly this row — and it is not a row the
// migration can resolve, because `op` is a credential and no Person is
// behind it. Refusing leaves the database at 0040 with the binding
// intact, so the account can claim again against the Owner it belongs
// to.
func TestMigrationRefusesAConnectionAdmittingAPrincipal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	old, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.migrateThrough("migrations/0040_person_id_references.sql"); err != nil {
		t.Fatalf("migrating through 0040: %v", err)
	}
	// Exactly what the old claim path wrote: an admin-surface binding
	// naming a principal, with no Person behind it.
	if _, err := old.sql.Exec(
		`INSERT INTO chat_bindings (transport, account, surface, identity, created_at, created_by)
		 VALUES ('telegram', '12345', 'admin', 'op', '2026-01-01T00:00:00Z', 'root-admin')`); err != nil {
		t.Fatalf("seeding a legacy binding: %v", err)
	}
	applied, err := old.AppliedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	before := len(applied)
	old.Close()

	db, err := Open(path)
	if err == nil {
		db.Close()
		t.Fatal("0041 migrated a connection that admits a machine principal")
	}
	if !strings.Contains(err.Error(), "op") {
		t.Errorf("the refusal does not name the binding to fix: %v", err)
	}

	// And it left the database where it was, so the operator can deal
	// with the binding and run again.
	after, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	nowApplied, err := after.AppliedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(nowApplied) != before {
		t.Fatalf("migrations applied = %d, want %d: a refused migration committed something", len(nowApplied), before)
	}
	var n int
	if err := after.sql.QueryRow(
		`SELECT COUNT(*) FROM chat_bindings WHERE identity = 'op'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the legacy binding count is %d, want it left intact for the operator to re-claim", n)
	}
}

// The converse, so the refusal cannot pass by refusing everything: a
// binding that names a Person migrates, and keeps pointing at them.
func TestMigrationCarriesAConnectionThatNamesAPerson(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	old, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.migrateThrough("migrations/0040_person_id_references.sql"); err != nil {
		t.Fatalf("migrating through 0040: %v", err)
	}
	if _, err := old.sql.Exec(
		`INSERT INTO butler_members (person_id, member, role, admitted, generation, created_at)
		 VALUES ('psn_sam', 'sam', 'member', 1, 1, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.sql.Exec(
		`INSERT INTO chat_bindings (transport, account, surface, identity, created_at, created_by)
		 VALUES ('telegram', '12345', 'family', 'sam', '2026-01-01T00:00:00Z', 'root-admin')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("0041 refused a connection that names a person: %v", err)
	}
	defer db.Close()
	b, err := db.LookupChatBinding("telegram", "12345", ButlerSurfaceFamily)
	if err != nil {
		t.Fatalf("the migrated binding does not resolve: %v", err)
	}
	if b.Identity != "sam" {
		t.Fatalf("binding identity = %q, want sam", b.Identity)
	}
}
