package runtimedb

import (
	"errors"
	"strings"
	"testing"
)

// Slice 11 (#212), design §8.1 and §8.3: a Connection carries its own
// admission policy, and a policy that cannot be read admits nobody.
//
// WHAT THESE ARE FOR. The rule they pin is not "an Owner Agent admits
// Owners" — that was true before and has its own test. It is that
// every way of failing to say so lands on DENY. The implementation
// they replaced answered with `surface == 'admin'`, where a value that
// was not one of the two meant "household", so the unreadable case and
// the widest case were the same answer. Each test below is one way of
// being unreadable.
//
// They are written against Connection.Admits directly, in-memory,
// rather than through a database, because that is where the property
// lives and because several of these states cannot be reached through
// a write path at all — which is the point: the guard must hold for a
// row nobody can currently write, since "nobody can write it" is a
// fact about today's schema and not about this function.

// ownerAgentConn is an Owner Agent's Connection with a well-formed
// policy: the state every test below deviates from by one field.
func ownerAgentConn() Connection {
	return Connection{
		Seq: 1, Butler: "alfred", Transport: "telegram", ExternalID: "chat-telegram-admin",
		Surface: ButlerSurfaceAdmin, AgentClass: AgentClassOwner,
		Admission: AdmitOwners, AdmissionSubject: AdmissionSubjectPerson,
	}
}

// memberAgentConn is the same for a Member Agent.
func memberAgentConn() Connection {
	return Connection{
		Seq: 2, Butler: "sandy", Transport: "telegram", ExternalID: "chat-telegram-family",
		Surface: ButlerSurfaceFamily, AgentClass: AgentClassMember,
		Admission: AdmitHousehold, AdmissionSubject: AdmissionSubjectPerson,
	}
}

// TestMalformedAdmissionConfigDeniesMemberToOwnerAgent is #212's
// fail-closed negative case, in every shape a policy can be malformed.
//
// Each case is a Member asking to reach an Owner Agent, which is the
// escalation §8.3 forbids. A correct implementation refuses all of
// them for the same reason — it cannot establish that this Person is
// admitted — and a subtly wrong one refuses the tidy cases and admits
// the broken ones.
func TestMalformedAdmissionConfigDeniesMemberToOwnerAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		with func(Connection) Connection
	}{
		{"no policy row at all", func(c Connection) Connection {
			// What a LEFT JOIN produces for a Connection whose
			// admission row is missing. It is the state the migration
			// and its trigger exist to prevent, and therefore exactly
			// the state whose refusal nothing else would catch.
			c.Admission = AdmissionUnresolved
			return c
		}},
		{"an admits value this build does not know", func(c Connection) Connection {
			c.Admission = Admission("everyone")
			return c
		}},
		{"an admits value that is only nearly right", func(c Connection) Connection {
			// The case a Contains or a TrimSpace would wave through.
			c.Admission = Admission(" owners ")
			return c
		}},
		{"a policy that claims to admit the household", func(c Connection) Connection {
			// 0063's trigger refuses this write; the Go check refuses
			// the row anyway. The Agent class is the ceiling, and a
			// policy cannot rise above it — so even a row written
			// around the trigger, restored from a backup or produced
			// by a future migration cannot be the way in.
			c.Admission = AdmitHousehold
			return c
		}},
		{"no subject kind", func(c Connection) Connection {
			c.AdmissionSubject = AdmissionSubjectUnresolved
			return c
		}},
		{"a group subject", func(c Connection) Connection {
			// §8.3's seam. Legal to store, admits nobody in MVP.
			c.AdmissionSubject = AdmissionSubjectGroup
			return c
		}},
		{"an unreadable surface", func(c Connection) Connection {
			// THE ORIGINAL DEFECT, preserved as a case. `surface ==
			// 'admin'` is false here, and false used to mean the
			// household may talk. AgentClass still says owner, and
			// OwnerAgent takes the narrower of the two.
			c.Surface = ButlerSurface("ADMIN")
			return c
		}},
		{"an unreadable agent class", func(c Connection) Connection {
			// The mirror of the case above: the other column is the
			// one that still reads.
			c.AgentClass = "0wner"
			return c
		}},
		{"an unreadable surface and a policy admitting the household",
			func(c Connection) Connection {
				// THE ONLY CASE THE AGENT-CLASS HALF OF OwnerAgent
				// CATCHES, and it escaped an earlier version of this
				// table. With `surface` unreadable AND the policy row
				// saying 'household', the policy branch is happy and
				// the surface comparison is false — so an OwnerAgent
				// that consulted `surface` alone would admit a Member
				// to an Owner Agent, and every other case here would
				// still pass. That is what a pair of columns read as
				// an OR is for, and it is worth a case of its own
				// precisely because no other case fails without it.
				c.Surface = ButlerSurface("ADMIN")
				c.Admission = AdmitHousehold
				return c
			}},
		{"an unreadable agent class and a policy admitting the household",
			func(c Connection) Connection {
				// The mirror, for the surface half.
				c.AgentClass = "0wner"
				c.Admission = AdmitHousehold
				return c
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.with(ownerAgentConn())
			if ok, reason := c.Admits(RoleMember); ok {
				t.Fatalf("a member reached an owner agent with %s (design §8.3)", tc.name)
			} else if reason == "" {
				// A denial with no reason is a denial nobody can act
				// on: the operator cannot tell a narrowed bot from a
				// demoted person from a missing row.
				t.Fatal("refused with no reason given")
			}
		})
	}
}

// TestMalformedAdmissionConfigAdmitsNobodyAtAMemberAgent is the same
// table where NOTHING ELSE CAN ANSWER.
//
// WHY IT EXISTS AS A SEPARATE TEST. Every case above is a Member at an
// OWNER Agent, and the Agent-class ceiling denies those before the
// policy is consulted at all. That makes them a weak test of the
// policy: two deliberate mutations — "an unresolved policy admits" and
// "a group subject admits" — passed the whole owner-agent table,
// because the layer above had already said no and the test could not
// tell which layer answered. #211 recorded exactly this failure from
// slice 9 ("three because a lower layer caught them and the test could
// not tell which layer answered").
//
// At a MEMBER Agent there is no ceiling above to catch anything. The
// policy is the only thing standing between a malformed row and an
// admitted Person, so these cases fail if and only if it is doing its
// job.
func TestMalformedAdmissionConfigAdmitsNobodyAtAMemberAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		with func(Connection) Connection
	}{
		{"no policy row at all", func(c Connection) Connection {
			c.Admission = AdmissionUnresolved
			c.AdmissionSubject = AdmissionSubjectUnresolved
			return c
		}},
		{"a readable subject and an unreadable policy", func(c Connection) Connection {
			// The case a deleted row does NOT produce: both columns
			// vanish together there, so the subject check answers
			// first and the policy branch is never reached. A row from
			// a schema version this binary predates has exactly this
			// shape, and only the policy branch refuses it.
			c.Admission = Admission("everyone")
			return c
		}},
		{"a readable subject and no policy", func(c Connection) Connection {
			c.Admission = AdmissionUnresolved
			return c
		}},
		{"a group subject and a readable policy", func(c Connection) Connection {
			// §8.3's seam at the one Agent class that would otherwise
			// admit them. Knowing who is speaking is not knowing who
			// is listening.
			c.AdmissionSubject = AdmissionSubjectGroup
			return c
		}},
		{"an unreadable subject and a readable policy", func(c Connection) Connection {
			c.AdmissionSubject = AdmissionSubject("people")
			return c
		}},
		{"an agent class this build cannot read", func(c Connection) Connection {
			// THE CASE A REVIEW OF #289 FOUND, and the one the
			// owner-agent table could not reach. `surface = 'family'`
			// is perfectly valid; only the CLASS is unreadable. So
			// OwnerAgent said no -- neither column literally says
			// owner -- and control fell through to a policy saying
			// 'household', which admitted a Member.
			//
			// That is the fail-open shape this slice removes, one
			// field along from where it was removed. A class a newer
			// schema adds is at least as likely to sit ABOVE member as
			// below, and the runtime cannot know which.
			c.AgentClass = "guest"
			return c
		}},
		{"a surface this build cannot read", func(c Connection) Connection {
			// The mirror. 'lounge' is not admin, so the old reading
			// treated it as the household's.
			c.Surface = ButlerSurface("lounge")
			return c
		}},
		{"neither classification field readable", func(c Connection) Connection {
			c.Surface = ButlerSurface("lounge")
			c.AgentClass = "guest"
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.with(memberAgentConn())
			// BOTH roles, because a member agent admits both when it is
			// well formed, and a guard that only refused Members would
			// leave the Owner case open.
			for _, role := range []string{RoleOwner, RoleMember} {
				if ok, _ := c.Admits(role); ok {
					t.Fatalf("a %s was admitted to a member agent with %s", role, tc.name)
				}
			}
		})
	}
}

// TestAWellFormedOwnerConnectionStillAdmitsItsOwner is the
// over-correction half, and it is not optional.
//
// Every test above passes against `func Admits(string) (bool, string) {
// return false, "no" }`. This is what makes them mean something: the
// well-formed case, which must still work, in both directions.
func TestAWellFormedOwnerConnectionStillAdmitsItsOwner(t *testing.T) {
	if ok, reason := ownerAgentConn().Admits(RoleOwner); !ok {
		t.Fatalf("an owner is refused at their own agent: %s", reason)
	}
	for _, role := range []string{RoleOwner, RoleMember} {
		if ok, reason := memberAgentConn().Admits(role); !ok {
			t.Fatalf("a %s is refused at a member agent admitting the household: %s", role, reason)
		}
	}
}

// A Member Agent narrowed by hand to Owners is the configuration §8.1
// asks for and the derivation could not express: nothing about the
// Agent changes, and the Connection admits Owners only.
func TestAMemberAgentConnectionCanBeNarrowedToOwners(t *testing.T) {
	c := memberAgentConn()
	c.Admission = AdmitOwners
	if ok, _ := c.Admits(RoleOwner); !ok {
		t.Fatal("an owner is refused at a member agent narrowed to owners")
	}
	if ok, _ := c.Admits(RoleMember); ok {
		t.Fatal("a narrowed connection still admits the household")
	}
}

// The widest policy there is, is still not a reason to admit a role
// this build cannot name.
//
// It is the same class of bug as the surface comparison, one level
// down: "household" is easy to implement as "anybody", and anybody
// includes an empty string and a role from a schema version this
// binary predates.
func TestTheHouseholdPolicyStillRequiresAKnownRole(t *testing.T) {
	for _, role := range []string{"", "owner ", "OWNER", "guest", "admin", "child"} {
		if ok, _ := memberAgentConn().Admits(role); ok {
			t.Fatalf("a connection admitting the household admitted the role %q", role)
		}
	}
}

// The database half of the same rule. 0063's triggers give every new
// Connection a policy and refuse one wider than its Agent, so the
// fail-closed branches above are defence rather than routine.
func TestANewConnectionGetsAPolicyAndCannotBeWidened(t *testing.T) {
	db := householdWith(t, "mika")
	if err := db.SetButlerMemberRole("mika", RoleOwner, "root-admin"); err != nil {
		t.Fatal(err)
	}
	owner := connect(t, db, "alfred", "telegram", ButlerSurfaceAdmin)
	member := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)

	// The trigger wrote one for each, derived from the Agent class.
	if owner.Admission != AdmitOwners || owner.AdmissionSubject != AdmissionSubjectPerson {
		t.Fatalf("a new owner-agent connection resolved as %+v", owner)
	}
	if member.Admission != AdmitHousehold {
		t.Fatalf("a new member-agent connection resolved as %+v", member)
	}

	// And it cannot be widened past the Agent behind it.
	if _, err := db.sql.Exec(
		`UPDATE connection_admission SET admits = 'household' WHERE connection = ?`,
		owner.Seq); err == nil {
		t.Fatal("an owner agent's connection was widened to admit the household (design §8.3)")
	} else if !strings.Contains(err.Error(), "owners only") {
		t.Fatalf("widening failed for some other reason: %v", err)
	}

	// A second policy for one Connection would be two answers to one
	// question, resolved by whichever the query happened to read.
	if _, err := db.sql.Exec(
		`INSERT INTO connection_admission (connection, admits, subject_kind, set_at, set_by)
		 VALUES (?, 'owners', 'person', '2026-09-20T00:00:00.000Z', 'test')`,
		member.Seq); err == nil {
		t.Fatal("one connection took two admission policies")
	}
}

// A Connection whose policy row is deleted out from under it resolves,
// and refuses. It does NOT come back as ErrNoConnection: the address
// exists, and reporting it as missing sends whoever investigates
// looking for a revocation that never happened.
func TestAConnectionWithNoPolicyResolvesAndAdmitsNobody(t *testing.T) {
	db := householdWith(t, "mika", "sam")
	at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)
	bind(t, db, at, "sam", "12345")
	if _, err := db.LookupChatBinding(at, "12345"); err != nil {
		t.Fatalf("the fixture never had a working binding: %v", err)
	}

	if _, err := db.sql.Exec(`DELETE FROM connection_admission WHERE connection = ?`, at.Seq); err != nil {
		t.Fatal(err)
	}
	fresh, err := db.ChatConnectionAt(at.Transport, at.ExternalID)
	if err != nil {
		t.Fatalf("a connection with no policy failed to resolve: %v", err)
	}
	if fresh.Admission != AdmissionUnresolved {
		t.Fatalf("a deleted policy read back as %q", fresh.Admission)
	}
	_, err = db.LookupChatBinding(fresh, "12345")
	if !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("a connection with no policy admitted somebody: err = %v", err)
	}
	if !strings.Contains(err.Error(), "admits nobody") {
		t.Fatalf("the refusal does not say the policy is missing: %v", err)
	}
}

// parseAdmission and parseAdmissionSubject read a stored value as one
// of a closed set, and everything else as the unresolved member.
//
// Tested directly as well as through Admits, because the struct-level
// tests above set the fields by hand and never exercise the parse. A
// default branch that returned the permissive member would pass all of
// them and fail here.
func TestAnUnrecognisedStoredPolicyParsesAsUnresolved(t *testing.T) {
	for _, raw := range []string{"", "household ", "Household", "HOUSEHOLD", "owner",
		"everyone", "all", "anybody", "0", "null", "person"} {
		if got := parseAdmission(raw); got != AdmissionUnresolved {
			t.Errorf("parseAdmission(%q) = %q, want unresolved", raw, got)
		}
	}
	if parseAdmission("owners") != AdmitOwners || parseAdmission("household") != AdmitHousehold {
		t.Fatal("the two values that ARE the vocabulary no longer parse")
	}
	for _, raw := range []string{"", "Person", "people", "persons", "groups", "owners"} {
		if got := parseAdmissionSubject(raw); got != AdmissionSubjectUnresolved {
			t.Errorf("parseAdmissionSubject(%q) = %q, want unresolved", raw, got)
		}
	}
	if parseAdmissionSubject("person") != AdmissionSubjectPerson ||
		parseAdmissionSubject("group") != AdmissionSubjectGroup {
		t.Fatal("the two subject kinds that ARE the vocabulary no longer parse")
	}
}
