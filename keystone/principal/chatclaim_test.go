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

// agent gives the household one Agent with a bot, and returns the
// Connection an invite is issued against.
//
// A claim names a Connection since 0045, not a surface: a surface is a
// class of Agents, and an invite to a class admits somebody to every
// member of it.
func agent(t *testing.T, s *Store, name string, surface runtimedb.ButlerSurface) runtimedb.Connection {
	t.Helper()
	if err := s.DB.UpsertModel(runtimedb.Model{
		Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Readiness: runtimedb.ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	profile := runtimedb.ProfileHousehold
	if surface == runtimedb.ButlerSurfaceAdmin {
		profile = runtimedb.ProfileOperator
	}
	capability := "chat-telegram-" + string(surface)
	if _, err := s.DB.GetButler(name); err != nil {
		if _, err := s.DB.CreateButler(runtimedb.Butler{
			Name: name, Directive: "look after the house", Surface: surface,
			Profile: profile, Model: "kitchen-box",
		}, "root-admin"); err != nil {
			t.Fatalf("CreateButler(%s): %v", name, err)
		}
		if _, err := s.DB.AddButlerAddress(name, "telegram", capability, "", "root-admin"); err != nil {
			t.Fatalf("addressing %s: %v", name, err)
		}
	}
	at, err := s.DB.ChatConnectionAt("telegram", capability)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// An owner-agent Connection admits an Owner.
func TestAnAdminClaimNamesAnOwner(t *testing.T) {
	s := store(t)
	person(t, s, "mika", runtimedb.RoleOwner)
	if _, _, err := s.IssueChatClaim(agent(t, s, "alfred", runtimedb.ButlerSurfaceAdmin), "mika", "root-admin", 0); err != nil {
		t.Fatalf("an owner cannot be issued an admin claim: %v", err)
	}
}

// And refuses a Member, however they are named. This is the case the
// old code could not express at all: it required a machine PRINCIPAL
// here, so the question "is this person an Owner" was never asked.
func TestAnAdminClaimRefusesAMember(t *testing.T) {
	s := store(t)
	person(t, s, "sam", runtimedb.RoleMember)
	_, _, err := s.IssueChatClaim(agent(t, s, "alfred", runtimedb.ButlerSurfaceAdmin), "sam", "root-admin", 0)
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
	if _, _, err := s.IssueChatClaim(agent(t, s, "alfred", runtimedb.ButlerSurfaceAdmin), "op", "root-admin", 0); err == nil {
		t.Fatal("a chat account was bound to a machine principal (ADR-W033 B9)")
	}
}

// Family Connections admit Members, so the Owner rule does not lock the
// household out of its own agents.
func TestAFamilyClaimNamesAMember(t *testing.T) {
	s := store(t)
	person(t, s, "sam", runtimedb.RoleMember)
	if _, _, err := s.IssueChatClaim(agent(t, s, "sandy", runtimedb.ButlerSurfaceFamily), "sam", "root-admin", 0); err != nil {
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
	if _, _, err := s.IssueChatClaim(agent(t, s, "sandy", runtimedb.ButlerSurfaceFamily), "staged", "root-admin", 0); err == nil {
		t.Fatal("a staged, un-admitted person was given a connection")
	}
	if _, _, err := s.IssueChatClaim(agent(t, s, "sandy", runtimedb.ButlerSurfaceFamily), "nobody", "root-admin", time.Minute); err == nil {
		t.Fatal("a connection was issued for somebody who does not exist")
	}
}
