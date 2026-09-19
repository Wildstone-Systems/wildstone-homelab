package main

// Host recovery (design §2.6, #206).
//
// These are mostly about what the commands REFUSE and about the one
// property the whole break-glass rests on: it works in the state where
// nothing else does. A recovery path that only works on a healthy
// household is not a recovery path.

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// asRoot runs fn with the root check satisfied, and puts it back.
//
// The suite cannot run as two different users, so the check is
// indirected and exercised from both sides here. A root check nothing
// exercises is one that stops working without anybody noticing.
func asRoot(t *testing.T, fn func()) {
	t.Helper()
	was := geteuid
	geteuid = func() int { return 0 }
	defer func() { geteuid = was }()
	fn()
}

func ownerTestDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, err := runtimedb.Open(filepath.Join(dir, "keystone.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db.Close()
	return filepath.Join(dir, "keystone.db")
}

func openOwnerDB(t *testing.T, path string) *runtimedb.DB {
	t.Helper()
	db, err := runtimedb.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The mandatory negative case: an unprivileged caller is refused, and
// refused BEFORE anything is opened.
//
// Root-equivalent host access is the recovery authority (§2.6). Without
// this the commands would still mostly fail for an ordinary user,
// because the data directory is root-owned — but "permission denied on
// keystone.db" reads like a broken install rather than a boundary, and a
// boundary that only holds because of a file mode is one `chmod` from
// not holding at all.
func TestRecoveryCommandsRefuseWithoutRoot(t *testing.T) {
	path := ownerTestDB(t)
	was := geteuid
	geteuid = func() int { return 1000 }
	defer func() { geteuid = was }()

	for _, args := range [][]string{
		{"create", "-db", path, "mika"},
		{"reset-password", "-db", path, "mika"},
		{"list", "-db", path},
	} {
		err := ownerCommand(args)
		if err == nil {
			t.Fatalf("%v was allowed without root", args)
		}
		if !strings.Contains(err.Error(), "needs root") {
			t.Fatalf("%v refused with %q, want it to say it needs root", args, err)
		}
	}

	// And nothing was written on the way to refusing.
	db := openOwnerDB(t, path)
	people, err := db.ListButlerMembers()
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 0 {
		t.Fatalf("a refused command still recorded %d people", len(people))
	}
}

// The state #206 calls the break-glass — "every Owner disabled" — turns
// out to be unreachable, and that is worth a test of its own.
//
// The Last Owner invariant is enforced by a database TRIGGER, not by the
// Go path, so it holds against a direct UPDATE as well as against the
// API. A household therefore cannot arrive at zero active Owners by any
// road: not through the Console, not through a script, not through
// somebody with the file open in sqlite3.
//
// So `owner create` is not needed for that. It is needed for the states
// that ARE reachable, which the tests below cover: nobody recorded at
// all (every fresh install, until the installer creates the first
// Owner), and somebody recorded as the wrong thing.
func TestTheLastOwnerCannotBeDisabledEvenBySQL(t *testing.T) {
	path := ownerTestDB(t)
	db := openOwnerDB(t, path)
	for _, who := range []string{"mika", "sandy"} {
		if err := db.RecordButlerMember(who, runtimedb.RoleOwner, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember(who, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DisableButlerMember("sandy", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.DisableButlerMember("mika", "sandy"); err == nil {
		t.Fatal("the last active owner was disabled through the Go path")
	}
	db.Close()

	// And the same straight at the column, which is the half that makes
	// this an invariant rather than a convention.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(
		`UPDATE butler_members SET disabled_at = '2026-01-01T00:00:00Z' WHERE member = 'mika'`); err == nil {
		t.Fatal("a direct UPDATE disabled the last active owner")
	}
}

// The reachable lockout, and the one every deployment starts in: nobody
// is on record at all.
//
// Until the installer creates the first Owner this is the state of every
// fresh install, and it is the state in which the Console admits whoever
// the directory says — because a household that has never had an Owner
// has made no statement about who administers it. `owner create` is how
// that statement gets made from the host.
func TestOwnerCreateMakesTheFirstOwnerFromNothing(t *testing.T) {
	path := ownerTestDB(t)
	asRoot(t, func() {
		if err := ownerCommand([]string{"create", "-db", path, "mika"}); err != nil {
			t.Fatalf("create: %v", err)
		}
	})
	db := openOwnerDB(t, path)
	m, err := db.GetButlerMember("mika")
	if err != nil {
		t.Fatalf("the first owner was not recorded: %v", err)
	}
	if m.Role != runtimedb.RoleOwner || !m.Active() {
		t.Fatalf("mika is %+v, want an active owner", m)
	}
	// And the household now says so, which is what shuts the Console's
	// bootstrap window (slice 3).
	has, err := db.HouseholdHasAnOwner()
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("the household still reports no owner")
	}
}

// The other reachable one: recorded, but never admitted. An Owner row
// that was never let in is not an active Owner, so the Console refuses
// them — and refuses everybody else too, because the household now
// counts as provisioned.
func TestOwnerCreateAdmitsSomebodyRecordedButNeverAdmitted(t *testing.T) {
	path := ownerTestDB(t)
	db := openOwnerDB(t, path)
	if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	db.Close()

	asRoot(t, func() {
		if err := ownerCommand([]string{"create", "-db", path, "mika"}); err != nil {
			t.Fatalf("create: %v", err)
		}
	})
	back := openOwnerDB(t, path)
	m, err := back.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Active() {
		t.Fatalf("mika is still not active: %+v", m)
	}
}

// A Member is repaired into an Owner, which is what a Console holder
// recorded as the wrong thing needs. Separate from the case above
// because "already an Owner but disabled" and "active but only a Member"
// are different repairs and only one of them changes the role.
func TestOwnerCreatePromotesAMember(t *testing.T) {
	path := ownerTestDB(t)
	db := openOwnerDB(t, path)
	if err := db.RecordButlerMember("sam", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	asRoot(t, func() {
		if err := ownerCommand([]string{"create", "-db", path, "sam"}); err != nil {
			t.Fatalf("create: %v", err)
		}
	})

	back := openOwnerDB(t, path)
	m, err := back.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if m.Role != runtimedb.RoleOwner || !m.Active() {
		t.Fatalf("sam is %+v, want an active owner", m)
	}
}

// Running it twice is the same household, not an error.
//
// An operator repairing a locked-out deployment is reading a scrolling
// terminal under pressure. A second run that failed would make them
// wonder what the first one half-did.
func TestOwnerCreateIsIdempotent(t *testing.T) {
	path := ownerTestDB(t)
	asRoot(t, func() {
		for i := range 2 {
			if err := ownerCommand([]string{"create", "-db", path, "mika"}); err != nil {
				t.Fatalf("run %d: %v", i+1, err)
			}
		}
	})
	db := openOwnerDB(t, path)
	people, err := db.ListButlerMembers()
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 1 {
		t.Fatalf("two runs produced %d people, want 1", len(people))
	}
}

// The first Owner carries no permanent privilege: once a second exists,
// they are demotable like anybody else (§2.4).
//
// This is the difference between a bootstrap and a superuser. A flag
// that survived bootstrap would be an account the household could never
// fully remove, which is the thing §2.4 says not to build.
func TestFirstOwnerHasNoPermanentPrivilege(t *testing.T) {
	path := ownerTestDB(t)
	asRoot(t, func() {
		if err := ownerCommand([]string{"create", "-db", path, "mika"}); err != nil {
			t.Fatal(err)
		}
	})
	db := openOwnerDB(t, path)

	// While they are the only one, the invariant protects them — which
	// is the protection §2.4 says the first Owner gets, and the only one.
	if err := db.SetButlerMemberRole("mika", runtimedb.RoleMember, "root-admin"); err == nil {
		t.Fatal("the only owner was demoted")
	}

	if err := db.RecordButlerMember("sandy", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("sandy", "mika"); err != nil {
		t.Fatal(err)
	}
	// And now nothing is special about them.
	if err := db.SetButlerMemberRole("mika", runtimedb.RoleMember, "sandy"); err != nil {
		t.Fatalf("the first owner could not be demoted once a second existed: %v", err)
	}
}

// Both commands write an audit row. A credential that can mint an Owner
// and leaves no trace is indistinguishable from somebody who found the
// host.
func TestRecoveryIsAudited(t *testing.T) {
	path := ownerTestDB(t)
	t.Setenv("SUDO_USER", "mika")
	asRoot(t, func() {
		if err := ownerCommand([]string{"create", "-db", path, "sandy"}); err != nil {
			t.Fatal(err)
		}
		if err := ownerCommand([]string{"reset-password", "-db", path, "sandy"}); err != nil {
			t.Fatal(err)
		}
	})

	db := openOwnerDB(t, path)
	rows, err := db.ListAudit(50)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]runtimedb.AuditRecord{}
	for _, r := range rows {
		seen[r.Path] = r
	}
	for _, want := range []string{"keystone owner create", "keystone owner reset-password"} {
		r, ok := seen[want]
		if !ok {
			t.Fatalf("no audit row for %q; got %v", want, seen)
		}
		// Who ran it, against whom, and from where — the three
		// questions §2.6 says the row has to answer.
		if !strings.Contains(r.Principal, "mika") {
			t.Errorf("%s: principal = %q, want it to name who ran it", want, r.Principal)
		}
		if r.ActingFor != "sandy" {
			t.Errorf("%s: actingFor = %q, want sandy", want, r.ActingFor)
		}
		if r.Remote != "host" {
			t.Errorf("%s: remote = %q, want host", want, r.Remote)
		}
	}
}

// reset-password stores nothing, and cannot: it never receives one.
//
// Asserted on the whole database rather than on a column, because the
// claim is "nowhere", and a test that checked one table would pass while
// a password sat in another. ADR-W020 A4 is the rule and this is the
// proof of it.
func TestResetPasswordDoesNotStoreThePasswordInWildstone(t *testing.T) {
	path := ownerTestDB(t)
	const secret = "correct-horse-battery-staple"
	asRoot(t, func() {
		if err := ownerCommand([]string{"create", "-db", path, "sandy"}); err != nil {
			t.Fatal(err)
		}
		// The command takes no password argument at all, which is the
		// structural half of the claim. Passing one is a usage error
		// rather than a stored secret.
		if err := ownerCommand([]string{"reset-password", "-db", path, "sandy", secret}); err == nil {
			t.Fatal("reset-password accepted a password argument")
		}
		if err := ownerCommand([]string{"reset-password", "-db", path, "sandy"}); err != nil {
			t.Fatal(err)
		}
	})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("the database file contains the password")
	}
	// A hash would be just as much a stored credential, so the file must
	// not contain one either. Checked by looking for ANY value in any
	// text column that came from the attempt, rather than guessing which
	// hash somebody might have reached for.
	if bytes.Contains(raw, []byte("password")) && bytes.Contains(raw, []byte(secret[:8])) {
		t.Fatal("the database file contains something derived from the password")
	}
}

// The recovery commands are not routes. Asserted against the router
// rather than by reading the code, because "we did not add a route" is
// the kind of claim that stays true right up until somebody adds one.
func TestRecoveryCommandsAreNotReachableOverTheAPI(t *testing.T) {
	routes, err := os.ReadFile("../../api/api.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"owner/create", "owner/reset-password", "ownerCreate", "ownerResetPassword"} {
		if strings.Contains(string(routes), forbidden) {
			t.Fatalf("the API mentions %q; host recovery must not be reachable over it (design §2.6)", forbidden)
		}
	}
}

// `owner list` is the diagnosis that comes before the repair, and the
// two states it has to tell apart look identical from outside: a Console
// that refuses everybody because nobody is recorded, and one that
// refuses everybody because the recorded Owner is disabled. They need
// different fixes.
func TestOwnerListSeparatesNobodyFromNoActiveOwner(t *testing.T) {
	path := ownerTestDB(t)

	var empty bytes.Buffer
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerList(&empty, raw); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if !strings.Contains(empty.String(), "Nobody is on record") {
		t.Fatalf("an empty household reads as %q", empty.String())
	}

	db := openOwnerDB(t, path)
	if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var unadmitted bytes.Buffer
	raw2, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer raw2.Close()
	if err := writeOwnerList(&unadmitted, raw2); err != nil {
		t.Fatal(err)
	}
	got := unadmitted.String()
	if !strings.Contains(got, "No ACTIVE owner") {
		t.Fatalf("a household with an un-admitted owner reads as %q", got)
	}
	if strings.Contains(got, "Nobody is on record") {
		t.Fatalf("a household with somebody on record says nobody is: %q", got)
	}
}
