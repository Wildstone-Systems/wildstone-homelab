package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// issueClaim mints a code through the API as an admin, the way an
// operator does.
func (h *harness) issueClaim(t *testing.T, surface, identity, transport string) claimResponse {
	t.Helper()
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
