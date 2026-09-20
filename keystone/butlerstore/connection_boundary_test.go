package butlerstore

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Slice 11 (#212) at the boundary: Ledger.ConnectionCheck stopped being
// an existence check.
//
// §10.1 says a Task "is re-evaluated at every authorization boundary...
// If the Person, session, Connection or grant behind it later loses
// authority, the Task loses it too at its next boundary." Slice 10
// wired the leg and checked only that the Connection still EXISTED,
// and said so in a comment, because there was no rule yet about what a
// Connection admits. Migration 0063 is that rule.
//
// WHAT A DOOR-ONLY CHECK CANNOT SEE. Nothing about a running task
// changes when its Person is demoted or its bot is narrowed. Both are
// changes to the admission the task inherited, and a check that only
// runs on the way in has already run.

// TestABoundaryRefusesWorkItsConnectionNoLongerAdmits is the negative
// case, in the two ways a Connection can stop carrying work that was
// admitted over it: it narrows past the Person, or it goes away.
//
// Demotion is the same loss approached from the other side — the
// policy holds still and the role moves — and it is covered where it
// can be seen end to end, in TestDemotingAnOwnerClosesTheirAdminConnection.
func TestABoundaryRefusesWorkItsConnectionNoLongerAdmits(t *testing.T) {
	t.Run("the connection is narrowed past the person's role", func(t *testing.T) {
		f := newDerivedFixture(t)
		// Narrow the Connection the task arrived over to Owners. mika
		// is a Member, so the work they started is no longer admitted
		// there — and nothing about the work, the person, the session
		// or the address has changed. A door-only check sees none of
		// this, because the door is behind it.
		narrowToOwners(t, f.db, f.connRef)

		err := f.ledger.Authorize(f.taskID)
		if err == nil {
			t.Fatal("a task continued over a connection that no longer admits its person (§10.1)")
		}
		if !strings.Contains(err.Error(), "no longer admits") {
			t.Fatalf("the refusal does not say admission was lost: %v", err)
		}
		// The refusal names the CONNECTION, because that is the thing
		// somebody would have to change. Naming the task or the tool
		// sends them to the wrong screen.
		if !strings.Contains(err.Error(), fixtureConnection) {
			t.Fatalf("the refusal does not name the connection: %v", err)
		}
	})

	t.Run("the connection was withdrawn entirely", func(t *testing.T) {
		f := newDerivedFixture(t)
		if err := f.db.RevokeButlerAddressFor(
			"alfred", "matrix", "!room:example.org", "root-admin"); err != nil {
			t.Fatal(err)
		}
		err := f.ledger.Authorize(f.taskID)
		if err == nil {
			t.Fatal("a task continued over a connection that was withdrawn")
		}
		// A DIFFERENT refusal from the one above, and deliberately so:
		// "the bot is gone" and "the bot no longer admits you" send
		// whoever reads it to different places.
		if !strings.Contains(err.Error(), "is gone") {
			t.Fatalf("a withdrawn connection reported as something else: %v", err)
		}
	})
}

// The two legs a Person can fail to supply, checked against
// ConnectionCheck directly.
//
// DIRECTLY, because neither state is reachable through Authorize: a
// task records its owner by person_id and resolves the handle on read
// (slice 2), so a task whose owner is blank or unresolvable cannot be
// built through the write path. That is exactly why the branches are
// worth pinning. "No write path can produce this" is a fact about
// today's schema — about a restored backup, a future migration, or a
// caller assembling an OriginConnection by hand it says nothing, and
// §10.1 answers a leg it cannot establish with no rather than with
// "not applicable".
func TestAConnectionCheckWithNoResolvablePersonRefuses(t *testing.T) {
	f := newDerivedFixture(t)
	check := f.ledger.ConnectionCheck()

	// The control: the real fixture state passes, so the refusals
	// below are about the owner and not about the Connection.
	if err := check(OriginConnection{
		Name: fixtureConnection, Ref: f.connRef, Owner: "mika"}); err != nil {
		t.Fatalf("the fixture's own connection refused its own owner: %v", err)
	}

	// An empty owner refuses, and refuses IN ITS OWN WORDS. Both this
	// and the unresolvable case below end in a denial either way —
	// GetButlerMember("") finds nobody, so deleting the empty-string
	// branch would still deny — which is why the assertion is on the
	// reason. "nothing records who it belongs to" and "'' is not a
	// person on record" describe different faults, and only the first
	// is true of a task with no owner.
	err := check(OriginConnection{Name: fixtureConnection, Ref: f.connRef, Owner: ""})
	if err == nil {
		t.Fatal("a task with no owner recorded was admitted")
	}
	if !strings.Contains(err.Error(), "nothing records who it belongs to") {
		t.Fatalf("a task with no owner was refused as something else: %v", err)
	}
	err = check(OriginConnection{
		Name: fixtureConnection, Ref: f.connRef, Owner: "nobody-by-that-name"})
	if err == nil {
		t.Fatal("a task owned by nobody on record was admitted")
	}
	if !strings.Contains(err.Error(), "nobody-by-that-name") {
		t.Fatalf("the refusal does not name who could not be resolved: %v", err)
	}
}

// The over-correction direction, and it is the test that makes the two
// above mean anything.
//
// Every assertion there passes against a ConnectionCheck that returns
// an error unconditionally — which would refuse every in-flight task in
// the house at its next boundary, including the ones that are perfectly
// fine. The fixture already proves an untouched task authorizes; these
// are the two states that LOOK like a loss and are not.
func TestABoundaryStillCarriesWorkItsConnectionAdmits(t *testing.T) {
	t.Run("a member agent's connection admitting the household", func(t *testing.T) {
		f := newDerivedFixture(t)
		// The fixture's own state, re-authorized: no narrowing, no
		// demotion, and the task carries on.
		if err := f.ledger.Authorize(f.taskID); err != nil {
			t.Fatalf("an unchanged task was refused at its boundary: %v", err)
		}
	})

	t.Run("a narrowing the person still satisfies", func(t *testing.T) {
		f := newDerivedFixture(t)
		// Promote mika first, THEN narrow. The Connection admits
		// Owners and mika is one, so this is a configuration change
		// that does not touch this task — and a check that refused on
		// "the policy changed" rather than on "this person is not
		// admitted" would stop it anyway.
		if err := f.db.SetButlerMemberRole("mika", runtimedb.RoleOwner, "root-admin"); err != nil {
			t.Fatal(err)
		}
		narrowToOwners(t, f.db, f.connRef)
		if err := f.ledger.Authorize(f.taskID); err != nil {
			t.Fatalf("an owner's task was refused by a narrowing they satisfy: %v", err)
		}
	})
}

// narrowToOwners sets a Connection's admission policy to Owners only,
// the way an operator narrowing a bot by hand would.
func narrowToOwners(t *testing.T, db *runtimedb.DB, connection int64) {
	t.Helper()
	if _, err := db.ExecForTest(
		`UPDATE connection_admission SET admits = 'owners' WHERE connection = ?`,
		connection); err != nil {
		t.Fatalf("narrowing connection %d: %v", connection, err)
	}
}
