package principal

import (
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// The configuration half of ADR-W033 B9: "Owner-Agent Connections admit
// Owners only. Configuration and runtime both enforce this."
//
// A claim code is configuration — it is written now and redeemed later
// — and an operator should learn about a mistake here rather than from
// a bot that answers nobody. The runtime half is
// LookupChatBinding, tested in runtimedb.

func person(t *testing.T, s *Store, handle, role string) {
	t.Helper()
	if err := s.DB.RecordButlerMember(handle, role, "", ""); err != nil {
		t.Fatalf("recording %q: %v", handle, err)
	}
	if err := s.DB.AdmitButlerMember(handle, "root-admin"); err != nil {
		t.Fatalf("admitting %q: %v", handle, err)
	}
}

// An owner-agent Connection admits an Owner.
func TestAnAdminClaimNamesAnOwner(t *testing.T) {
	s := store(t)
	person(t, s, "mika", runtimedb.RoleOwner)
	if _, _, err := s.IssueChatClaim(runtimedb.ButlerSurfaceAdmin, "mika", "telegram", "root-admin", 0); err != nil {
		t.Fatalf("an owner cannot be issued an admin claim: %v", err)
	}
}

// And refuses a Member, however they are named. This is the case the
// old code could not express at all: it required a machine PRINCIPAL
// here, so the question "is this person an Owner" was never asked.
func TestAnAdminClaimRefusesAMember(t *testing.T) {
	s := store(t)
	person(t, s, "sam", runtimedb.RoleMember)
	_, _, err := s.IssueChatClaim(runtimedb.ButlerSurfaceAdmin, "sam", "telegram", "root-admin", 0)
	if err == nil {
		t.Fatal("a member was admitted to an owner agent's connection")
	}
	if !strings.Contains(err.Error(), "owners only") {
		t.Fatalf("refusal = %v, want one that says why", err)
	}
}

// A machine principal is not a Person, so it cannot be admitted to
// anything — which is the whole correction. `op` exists as a principal
// in every deployment, and under the superseded model this was the
// SUPPORTED way to bind the operator's own chat account.
func TestAPrincipalIsNotAdmittedToAConnection(t *testing.T) {
	s := store(t)
	if err := s.DB.CreatePrincipal("op", runtimedb.RoleOperator, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.IssueChatClaim(runtimedb.ButlerSurfaceAdmin, "op", "telegram", "root-admin", 0); err == nil {
		t.Fatal("a chat account was bound to a machine principal (ADR-W033 B9)")
	}
}

// Family Connections admit Members, so the Owner rule does not lock the
// household out of its own agents.
func TestAFamilyClaimNamesAMember(t *testing.T) {
	s := store(t)
	person(t, s, "sam", runtimedb.RoleMember)
	if _, _, err := s.IssueChatClaim(runtimedb.ButlerSurfaceFamily, "sam", "telegram", "root-admin", 0); err != nil {
		t.Fatalf("a member cannot be issued a family claim: %v", err)
	}
}

// Somebody who is not admitted is refused on either surface: B9 says
// unknown or unbound external identities are refused before model
// inference, and an un-admitted Person is not somebody an Agent may
// work for (ADR-W030 A2.1).
func TestAClaimRefusesSomebodyNotAdmitted(t *testing.T) {
	s := store(t)
	if err := s.DB.RecordButlerMember("staged", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.IssueChatClaim(runtimedb.ButlerSurfaceFamily, "staged", "telegram", "root-admin", 0); err == nil {
		t.Fatal("a staged, un-admitted person was given a connection")
	}
	if _, _, err := s.IssueChatClaim(runtimedb.ButlerSurfaceFamily, "nobody", "telegram", "root-admin", time.Minute); err == nil {
		t.Fatal("a connection was issued for somebody who does not exist")
	}
}
