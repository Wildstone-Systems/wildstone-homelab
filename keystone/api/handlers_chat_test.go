package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// ensureChatPrereqs declares what an invite requires before one can be
// issued: an installed adapter for that surface, and — on the family
// surface — the person the code names, who is an `LdapUser` in the
// Statefile (ADR-W020 A3). A test that skips this is testing a code
// nobody could ever redeem.
func (h *harness) ensureChatPrereqs(t *testing.T, surface, identity, transport string) {
	t.Helper()
	if transport == "" {
		transport = "telegram"
	}
	current, err := h.s.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	owner := "capability/chat-" + transport + "-" + surface
	haveAdapter, haveMember := false, false
	for _, r := range current {
		if r.Metadata.Owner == owner {
			haveAdapter = true
		}
		if r.Kind == resource.KindLdapUser && r.LdapUser != nil && r.LdapUser.Username == identity {
			haveMember = true
		}
	}
	// Credentialing is part of the prerequisite, not decoration: a code
	// may only name an adapter that can actually redeem one.
	h.adapterToken(t, "chat-"+transport+"-"+surface)

	changed := false
	if !haveAdapter {
		name := "chat-" + transport + "-" + surface + "-state"
		if err := h.s.State.Put(resource.Resource{
			ApiVersion: resource.ApiVersion, Kind: resource.KindVolume,
			Metadata: resource.Metadata{Name: name, Owner: owner, Node: "n1"},
			Volume:   &resource.VolumeSpec{Name: name},
		}); err != nil {
			t.Fatal(err)
		}
		changed = true
	}
	// The Person the Connection admits (ADR-W033 B9, design §8.3). An
	// admin-surface Connection is an Owner Agent's reach, so the Person
	// it admits is an Owner; a family one is an ordinary Member.
	//
	// This is the fixture the slice changed. It used to name a Keystone
	// principal on the admin surface — `op`, `root-admin` — because
	// chatclaim.go required one. A credential is not a Person, and a
	// chat account reaching an Owner Agent as one had nobody behind it.
	role := runtimedb.RoleMember
	if surface == "admin" {
		role = runtimedb.RoleOwner
	}
	m, err := h.s.DB.GetButlerMember(identity)
	if err != nil {
		if err := h.s.DB.RecordButlerMember(identity, role, "", ""); err != nil {
			t.Fatalf("recording %q: %v", identity, err)
		}
		m, err = h.s.DB.GetButlerMember(identity)
		if err != nil {
			t.Fatalf("reading %q back: %v", identity, err)
		}
	}
	// RecordButlerMember never changes a role — crossing the Owner
	// boundary is its own audited act — so an existing Member named for
	// an admin-surface Connection is promoted through the path that
	// records it.
	if role == runtimedb.RoleOwner && m.Role != runtimedb.RoleOwner {
		if err := h.s.DB.SetButlerMemberRole(identity, runtimedb.RoleOwner, "root-admin"); err != nil {
			t.Fatalf("promoting %q: %v", identity, err)
		}
	}
	if !m.Active() {
		if err := h.s.DB.AdmitButlerMember(identity, "root-admin"); err != nil {
			t.Fatalf("admitting %q: %v", identity, err)
		}
	}
	if surface == "family" && !haveMember {
		if err := h.s.State.Put(resource.Resource{
			ApiVersion: resource.ApiVersion, Kind: resource.KindLdapUser,
			Metadata: resource.Metadata{Name: identity, Owner: "system", Node: "n1"},
			LdapUser: &resource.LdapUserSpec{
				Username: identity, DisplayName: identity, Email: identity + "@example.com",
			},
		}); err != nil {
			t.Fatal(err)
		}
		changed = true
	}
	if changed {
		if err := h.s.State.Commit("test", "seed", "chat prerequisites"); err != nil {
			t.Fatal(err)
		}
	}
}

// issueClaim mints a code through the API as an admin, the way an
// operator does.
func (h *harness) issueClaim(t *testing.T, surface, identity, transport string) claimResponse {
	t.Helper()
	h.ensureChatPrereqs(t, surface, identity, transport)
	var out claimResponse
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/claims", map[string]any{
		"surface": surface, "identity": identity, "transport": transport,
	}, &out)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue claim: status = %d, want 201", resp.StatusCode)
	}
	return out
}

// The happy path: an operator issues a code, the adapter redeems it, and
// the account is bound to the identity the CODE named (ADR-W026 A4).
func TestChatClaimRedeemBinds(t *testing.T) {
	h := newHarness(t)
	claim := h.issueClaim(t, "family", "sam", "telegram")
	if claim.Code == "" || !strings.Contains(claim.Note, "works once") {
		t.Fatalf("claim = %+v", claim)
	}

	var binding runtimedb.ChatBinding
	resp := h.do(runtimedb.RoleConcierge, "POST", "/api/v1/chat/bindings/redeem", map[string]any{
		"code": claim.Code, "transport": "telegram", "account": "12345", "displayName": "Sam",
	}, &binding)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("redeem: status = %d, want 201", resp.StatusCode)
	}
	if binding.Identity != "sam" || binding.Surface != runtimedb.ButlerSurfaceFamily {
		t.Fatalf("binding = %+v, want sam on the family surface", binding)
	}
	if binding.CreatedBy != "root-admin" {
		t.Fatalf("binding created by %q, want the issuing operator", binding.CreatedBy)
	}

	// Single use: the same code cannot bind a second account.
	resp = h.do(runtimedb.RoleConcierge, "POST", "/api/v1/chat/bindings/redeem", map[string]any{
		"code": claim.Code, "transport": "telegram", "account": "99999",
	}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("second redeem: status = %d, want 403", resp.StatusCode)
	}
}

// A code carries its surface and identity; nothing the redeemer sends
// can change either. This is the property that keeps a family code from
// ever producing an admin binding.
func TestChatClaimCannotBeRedirected(t *testing.T) {
	h := newHarness(t)
	claim := h.issueClaim(t, "family", "sam", "")

	var binding runtimedb.ChatBinding
	resp := h.do(runtimedb.RoleConcierge, "POST", "/api/v1/chat/bindings/redeem", map[string]any{
		"code": claim.Code, "transport": "telegram", "account": "12345",
		// Everything below is ignored: the redeem request has no field
		// for a surface or an identity, and these are simply unknown.
		"surface": "admin", "identity": "root-admin",
	}, &binding)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("redeem: status = %d", resp.StatusCode)
	}
	if binding.Surface != runtimedb.ButlerSurfaceFamily || binding.Identity != "sam" {
		t.Fatalf("binding = %+v, want the code's own surface and identity", binding)
	}
}

// A code issued for one transport does not work on another, and an
// unknown code fails the same way a spent one does.
func TestChatClaimRefusalsAreIndistinguishable(t *testing.T) {
	h := newHarness(t)
	claim := h.issueClaim(t, "family", "sam", "telegram")

	wrong := h.do(runtimedb.RoleConcierge, "POST", "/api/v1/chat/bindings/redeem", map[string]any{
		"code": claim.Code, "transport": "discord", "account": "12345",
	}, nil)
	unknown := h.do(runtimedb.RoleConcierge, "POST", "/api/v1/chat/bindings/redeem", map[string]any{
		"code": "ZZZZ-ZZZZ-ZZZZ", "transport": "telegram", "account": "12345",
	}, nil)
	if wrong.StatusCode != http.StatusForbidden || unknown.StatusCode != http.StatusForbidden {
		t.Fatalf("statuses = %d and %d, want both 403", wrong.StatusCode, unknown.StatusCode)
	}
}

// An admin-surface code must name a real principal: the operator finds
// out about the typo when they issue the code, not when their bot
// silently answers nobody.
func TestChatClaimAdminSurfaceRequiresPrincipal(t *testing.T) {
	h := newHarness(t)
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/claims", map[string]any{
		"surface": "admin", "identity": "no-such-principal",
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// Issuing and revoking are `manage`; an operator or butler token cannot
// hand out access to the homelab (ADR-W026 A4).
func TestChatClaimIssuingRequiresManage(t *testing.T) {
	h := newHarness(t)
	for _, role := range []runtimedb.Role{runtimedb.RoleOperator, runtimedb.RoleButler, runtimedb.RoleValet} {
		resp := h.do(role, "POST", "/api/v1/chat/claims", map[string]any{
			"surface": "family", "identity": "sam",
		}, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s issuing a claim: status = %d, want 403", role, resp.StatusCode)
		}
	}
}

// Revoking takes effect immediately: the binding stops resolving, and
// the account can be bound again with a fresh code.
func TestChatBindingRevokeAndRebind(t *testing.T) {
	h := newHarness(t)
	claim := h.issueClaim(t, "family", "sam", "telegram")
	var binding runtimedb.ChatBinding
	h.do(runtimedb.RoleConcierge, "POST", "/api/v1/chat/bindings/redeem", map[string]any{
		"code": claim.Code, "transport": "telegram", "account": "12345",
	}, &binding)

	if _, err := h.s.DB.LookupChatBinding("telegram", "12345", runtimedb.ButlerSurfaceFamily); err != nil {
		t.Fatalf("lookup before revoke: %v", err)
	}
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/bindings/"+itoa(binding.ID)+"/revoke", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: status = %d", resp.StatusCode)
	}
	if _, err := h.s.DB.LookupChatBinding("telegram", "12345", runtimedb.ButlerSurfaceFamily); err == nil {
		t.Fatalf("a revoked binding still resolves")
	}
	// Revoking twice is a 404, not a silent success.
	if resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/bindings/"+itoa(binding.ID)+"/revoke", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second revoke: status = %d, want 404", resp.StatusCode)
	}

	// A fresh code rebinds the same account.
	second := h.issueClaim(t, "family", "sam", "telegram")
	var rebound runtimedb.ChatBinding
	if resp := h.do(runtimedb.RoleConcierge, "POST", "/api/v1/chat/bindings/redeem", map[string]any{
		"code": second.Code, "transport": "telegram", "account": "12345",
	}, &rebound); resp.StatusCode != http.StatusCreated {
		t.Fatalf("rebind: status = %d", resp.StatusCode)
	}
	if rebound.ID == binding.ID {
		t.Fatalf("rebinding reused the revoked row")
	}

	// The default listing shows live bindings only; ?revoked=true shows
	// the history.
	var live []runtimedb.ChatBinding
	h.do(runtimedb.RoleAdmin, "GET", "/api/v1/chat/bindings", nil, &live)
	if len(live) != 1 || live[0].ID != rebound.ID {
		t.Fatalf("live bindings = %+v", live)
	}
	var all []runtimedb.ChatBinding
	h.do(runtimedb.RoleAdmin, "GET", "/api/v1/chat/bindings?revoked=true", nil, &all)
	if len(all) != 2 {
		t.Fatalf("full history = %d rows, want 2", len(all))
	}
}

// A concierge holds `converse` and nothing else: it can redeem, and it
// cannot read state, propose, approve or list bindings (ADR-W026 A3).
func TestConciergeHoldsConverseOnly(t *testing.T) {
	for _, verb := range []principal.Verb{
		principal.VerbRead, principal.VerbPropose, principal.VerbApprove,
		principal.VerbManage, principal.VerbEmit,
	} {
		if principal.Allowed(runtimedb.RoleConcierge, verb) {
			t.Fatalf("concierge holds %q — it must hold converse and nothing else", verb)
		}
	}
	if !principal.Allowed(runtimedb.RoleConcierge, principal.VerbConverse) {
		t.Fatalf("concierge does not hold converse")
	}
	// And no role gains approve over chat by holding converse
	// (ADR-W026 A5): the butler role an admin adapter runs as still
	// cannot approve.
	if principal.Allowed(runtimedb.RoleButler, principal.VerbApprove) {
		t.Fatalf("the butler role holds approve — no chat transport may reach an approval")
	}
}

// The chat verb must NOT be on the butler role. ADR-W024 A5 makes every
// external agent — Hermes, Codex, anything else — a butler-role
// principal, so a chat verb there would appear on credentials minted
// long before chat existed and hand each of them the operator's private
// queue (ADR-W026 A3).
func TestExternalAgentsGainNothingFromChat(t *testing.T) {
	if principal.Allowed(runtimedb.RoleButler, principal.VerbConverse) {
		t.Fatalf("the butler role holds converse — every external agent token would gain chat access on upgrade")
	}
	// The adapters hold converse and nothing else.
	for _, role := range []runtimedb.Role{runtimedb.RoleConcierge, runtimedb.RoleValet} {
		if !principal.Allowed(role, principal.VerbConverse) {
			t.Fatalf("%s does not hold converse", role)
		}
		for _, verb := range []principal.Verb{
			principal.VerbRead, principal.VerbPropose, principal.VerbApprove,
			principal.VerbManage, principal.VerbEmit,
		} {
			if principal.Allowed(role, verb) {
				t.Fatalf("%s holds %q — an adapter relays and nothing else", role, verb)
			}
		}
	}
}

// An external agent's token cannot reach the chat plane at all.
func TestButlerRoleCannotReachTheChatPlane(t *testing.T) {
	h := newHarness(t)
	for _, call := range []struct{ method, path string }{
		{"POST", "/api/v1/chat/messages"},
		{"GET", "/api/v1/chat/outbound"},
		{"POST", "/api/v1/chat/outbound/1/ack"},
		{"POST", "/api/v1/chat/bindings/redeem"},
	} {
		resp := h.do(runtimedb.RoleButler, call.method, call.path, map[string]any{
			"transport": "telegram", "account": "12345", "text": "hello",
		}, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s as an external agent: status = %d, want 403", call.method, call.path, resp.StatusCode)
		}
	}
}

// A claim code for the other surface is refused WITHOUT being burned: a
// household code sent to the operator's bot by mistake stays usable on
// the right one (ADR-W026 A4).
func TestClaimOnTheWrongBotIsRefusedNotBurned(t *testing.T) {
	h := newHarness(t)
	claim := h.issueClaim(t, "family", "sam", "telegram")

	// The admin bot (valet) sees a family code.
	resp := h.do(runtimedb.RoleValet, "POST", "/api/v1/chat/bindings/redeem", map[string]any{
		"code": claim.Code, "transport": "telegram", "account": "12345",
	}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong-bot redeem: status = %d, want 403", resp.StatusCode)
	}
	// No binding was created on either surface.
	if bindings, err := h.s.DB.ListChatBindings(true); err != nil || len(bindings) != 0 {
		t.Fatalf("bindings = %+v (err %v), want none", bindings, err)
	}
	// And the code still works on the bot it was meant for.
	var binding runtimedb.ChatBinding
	if resp := h.do(runtimedb.RoleConcierge, "POST", "/api/v1/chat/bindings/redeem", map[string]any{
		"code": claim.Code, "transport": "telegram", "account": "12345",
	}, &binding); resp.StatusCode != http.StatusCreated {
		t.Fatalf("redeem on the right bot: status = %d, want 201 — the code was burned by the wrong one", resp.StatusCode)
	}
	if binding.Identity != "sam" || binding.Surface != runtimedb.ButlerSurfaceFamily {
		t.Fatalf("binding = %+v", binding)
	}
}

// A human redeeming through the Console or `wild` is pinned to no
// surface: they can redeem on behalf of whoever is standing next to
// them, which is what a claim code is for.
func TestHumanRedeemIsNotSurfacePinned(t *testing.T) {
	h := newHarness(t)
	for _, surface := range []string{"family", "admin"} {
		identity := "sam"
		if surface == "admin" {
			identity = "root-admin"
		}
		claim := h.issueClaim(t, surface, identity, "telegram")
		var binding runtimedb.ChatBinding
		if resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/bindings/redeem", map[string]any{
			"code": claim.Code, "transport": "telegram", "account": "acct-" + surface,
		}, &binding); resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s: human redeem status = %d, want 201", surface, resp.StatusCode)
		}
		if string(binding.Surface) != surface {
			t.Fatalf("binding surface = %q, want %q", binding.Surface, surface)
		}
	}
}
