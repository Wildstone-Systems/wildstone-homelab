package butlerlive

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
)

// Slice 11 (#212): §10 layer 1 ∩ layer 3. A Connection is a ceiling on
// what may be done over it, and a ceiling is not a grant.

// openPermits is the most permissive Connection rule there can be: it
// refuses nothing. It stands in for a Connection somebody configured
// wide open, which is the case a widening bug would hide behind.
func openPermits(string) (bool, string) { return true, "" }

// refusesPerson is a resolver whose LAYER 1 says no — nobody has
// admitted this person — while its layer 2 would happily allow the
// tool. The split is the point: a widening bug shows up as layer 2's
// yes reaching the caller past layer 1's no.
type refusesPerson struct{ asked []string }

func (r *refusesPerson) AuthorizeTool(q butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	r.asked = append(r.asked, q.Tool)
	return butler.ToolDecision{Allowed: true, Reason: "layer 2 has no objection"}, nil
}

func (r *refusesPerson) AdmitPerson(q butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	return butler.ToolDecision{
		Allowed: false,
		Reason:  q.Person + " is not admitted to the butler",
	}, nil
}

// admitsPerson is the same shape with layer 1 saying yes, for the
// over-correction direction.
type admitsPerson struct{}

func (admitsPerson) AuthorizeTool(butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	return butler.ToolDecision{Allowed: true, Reason: "granted"}, nil
}

func (admitsPerson) AdmitPerson(butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	return butler.ToolDecision{Allowed: true}, nil
}

// TestConnectionScopeNeverWidensPersonAuthority — a permissive
// Connection does not grant what the Person lacks (#212; §10 layer 1 ∩
// layer 3).
//
// WHAT COULD GO WRONG HERE. connectionCeiling is a decorator, and once
// it is bound it IS the authorizer the seam sees. It therefore has to
// carry AdmitPerson, and a decorator that carries a method has two
// ways to get it wrong: answer the question itself, or answer it from
// its own rule. Either would make the Connection a place a Person
// could be admitted — and a Connection nobody narrowed refuses
// nothing, so the widest configuration would become the most
// permissive authority rather than merely the least restrictive
// ceiling.
//
// The test pins the only correct behaviour: forward, and let layer 1
// answer.
func TestConnectionScopeNeverWidensPersonAuthority(t *testing.T) {
	q := butler.ToolAuthorityQuery{
		Caller: butler.CallerPersonFacing, Person: "sam",
		Connection: portal.Connection, Tool: "pantry.read_list", Structural: true,
	}

	// A Connection that refuses nothing at all.
	inner := &refusesPerson{}
	got, err := WithConnection(inner, openPermits).(butler.PersonAuthorizer).AdmitPerson(q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Fatal("a wide-open connection admitted a person the authority refuses (§10 layer 1)")
	}
	if !strings.Contains(got.Reason, "not admitted") {
		t.Fatalf("reason = %q, want layer 1's own answer to survive the decorator", got.Reason)
	}
	// And the ceiling did not answer layer 1 out of its own tool rule:
	// a decorator that decided this itself would never have needed to
	// ask, and would also never have asked about the tool.
	if len(inner.asked) != 0 {
		t.Errorf("the ceiling consulted layer 2 while answering layer 1: %v", inner.asked)
	}

	// The same with the REAL household rule rather than an open one,
	// for a tool that rule permits. The Connection has no objection,
	// and the Person is still refused.
	got, err = WithConnection(inner, portal.Permits).(butler.PersonAuthorizer).AdmitPerson(q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Fatal("the household connection admitted a person the authority refuses")
	}

	// THE OVER-CORRECTION DIRECTION. All of the above passes against a
	// decorator whose AdmitPerson returns a flat no, which would refuse
	// every person on every connection and take the whole household
	// surface away. Layer 1's yes must survive too.
	got, err = WithConnection(admitsPerson{}, portal.Permits).(butler.PersonAuthorizer).AdmitPerson(q)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed {
		t.Fatalf("an admitted person was refused by the ceiling: %q", got.Reason)
	}
}

// A resolver that cannot answer layer 1 is not a resolver that admits
// everybody.
//
// The decorator forwards to an inner authorizer that may or may not
// implement PersonAuthorizer. When it does not, nothing on this
// deployment can say whether this person may act — which §10.1 answers
// with no. The case exists because the tempting implementation of a
// forwarder is to pass through what it can and allow what it cannot.
func TestACeilingOverAnAuthorityThatCannotJudgePeopleRefuses(t *testing.T) {
	// `yes` (connection_test.go) is a bare ToolAuthorizer: it answers
	// tools and knows nothing about people.
	a := WithConnection(&yes{}, portal.Permits).(butler.PersonAuthorizer)
	got, err := a.AdmitPerson(butler.ToolAuthorityQuery{
		Caller: butler.CallerPersonFacing, Person: "sam",
		Connection: portal.Connection, Tool: "pantry.read_list", Structural: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Fatal("a person was admitted by an authority with no opinion about people")
	}
	if !strings.Contains(got.Reason, "sam") {
		t.Errorf("reason = %q, want it to name who was refused", got.Reason)
	}
}

// A person-facing call with nothing underneath is refused even for a
// tool the Connection permits — and an administrative one is not.
//
// This is the scoping slice 10 established, asserted through the
// ceiling rather than through the seam, because once a ceiling is
// bound it answers in the seam's place. A decorator that fell open
// here would be the permissive default surviving inside the one
// component whose whole job is to narrow.
func TestTheCeilingKeepsTheTwoCallerPathsApart(t *testing.T) {
	a := WithConnection(nil, portal.Permits)
	person, err := a.AuthorizeTool(butler.ToolAuthorityQuery{
		Caller: butler.CallerPersonFacing, Person: "sam",
		Connection: portal.Connection, Tool: "pantry.read_list", Structural: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if person.Allowed {
		t.Fatal("a person-facing call with no authority underneath was allowed")
	}
	admin, err := a.AuthorizeTool(butler.ToolAuthorityQuery{
		Caller: butler.CallerAdministrative, Tool: "pantry.read_list", Structural: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !admin.Allowed {
		t.Fatalf("an administrative check on an unconfigured deployment was refused: %q", admin.Reason)
	}
}
