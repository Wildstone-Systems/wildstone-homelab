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
	h.agentAt(t, surface, transport)
}

// agentFor names the Agent these fixtures give each surface a bot on.
//
// Since ADR-W033 B7 a Connection belongs to one Agent, so a fixture
// cannot bind "the admin surface" any more — there has to be an Agent
// answering there. The catalog ships one bot per (transport, surface),
// so one Agent per surface is what a real deployment can have today.
func agentFor(surface string) string {
	if surface == "admin" {
		return "alfred"
	}
	return "sandy"
}

// agentAt gives that Agent its bot, and returns the Connection.
func (h *harness) agentAt(t *testing.T, surface, transport string) runtimedb.Connection {
	t.Helper()
	if transport == "" {
		transport = "telegram"
	}
	butler := agentFor(surface)
	capability := chatCapabilityPrefix + transport + "-" + surface
	if _, err := h.s.DB.GetButler(butler); err != nil {
		if err := h.s.DB.UpsertModel(runtimedb.Model{
			Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama",
			ProviderModel: "llama", Readiness: runtimedb.ModelReady, Enabled: true,
		}, "root-admin"); err != nil {
			t.Fatal(err)
		}
		profile := runtimedb.AgentClassMember
		if surface == "admin" {
			profile = runtimedb.AgentClassOwner
		}
		if _, err := h.s.DB.CreateButler(runtimedb.Butler{
			Name: butler, Directive: "look after the house",
			Surface: runtimedb.ButlerSurface(surface), AgentClass: profile, Model: "kitchen-box",
		}, "root-admin"); err != nil {
			t.Fatalf("CreateButler(%s): %v", butler, err)
		}
	}
	if _, err := h.s.DB.ChatConnectionAt(transport, capability); err != nil {
		if _, err := h.s.DB.AddButlerAddress(butler, transport, capability, "", "root-admin"); err != nil {
			t.Fatalf("addressing %s at %s: %v", butler, capability, err)
		}
		// Continuation tests use an explicitly classified destination. Real
		// newly created Connections still default to unknown retention, which
		// now denies loading prior content until the operator classifies it.
		if _, err := h.s.DB.ExecForTest(`UPDATE connection_disclosure SET retention='provider'
            WHERE connection=(SELECT seq FROM butler_addresses WHERE transport=? AND external_id=?)`, transport, capability); err != nil {
			t.Fatal(err)
		}
	}
	at, err := h.s.DB.ChatConnectionAt(transport, capability)
	if err != nil {
		t.Fatalf("resolving the connection at %s: %v", capability, err)
	}
	return at
}

// issueClaim mints a code through the API as an admin, the way an
// operator does.
func (h *harness) issueClaim(t *testing.T, surface, identity, transport string) claimResponse {
	t.Helper()
	h.ensureChatPrereqs(t, surface, identity, transport)
	var out claimResponse
	if transport == "" {
		transport = "telegram"
	}
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/claims", map[string]any{
		"butler": agentFor(surface), "identity": identity, "transport": transport,
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
	if binding.Identity != "sam" || binding.Butler != agentFor("family") {
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
		"identity": "root-admin",
	}, &binding)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("redeem: status = %d", resp.StatusCode)
	}
	if binding.Butler != agentFor("family") || binding.Identity != "sam" {
		t.Fatalf("binding = %+v, want the code's own surface and identity", binding)
	}
}

// A code issued for one Agent does not work at another's bot, and an
// unknown code fails the same way. One refusal for every reason: a bot
// relaying codes for strangers must not be able to tell them apart.
func TestChatClaimRefusalsAreIndistinguishable(t *testing.T) {
	h := newHarness(t)
	h.agentAt(t, "family", "telegram")
	// A code for the OTHER Agent, presented at this one's bot.
	claim := h.issueClaim(t, "admin", "mika", "telegram")

	wrong := h.do(runtimedb.RoleConcierge, "POST", "/api/v1/chat/bindings/redeem", map[string]any{
		"code": claim.Code, "account": "12345",
	}, nil)
	unknown := h.do(runtimedb.RoleConcierge, "POST", "/api/v1/chat/bindings/redeem", map[string]any{
		"code": "ZZZZ-ZZZZ-ZZZZ", "account": "12345",
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
		"identity": "no-such-principal",
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
			"identity": "sam",
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

	if _, err := h.s.DB.LookupChatBinding(h.agentAt(t, "family", "telegram"), "12345"); err != nil {
		t.Fatalf("lookup before revoke: %v", err)
	}
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/bindings/"+itoa(binding.ID)+"/revoke", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: status = %d", resp.StatusCode)
	}
	if _, err := h.s.DB.LookupChatBinding(h.agentAt(t, "family", "telegram"), "12345"); err == nil {
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

// A claim code for another Agent is refused WITHOUT being burned: a
// code meant for one bot sent to another by mistake stays usable on the
// right one (ADR-W026 A4, ADR-W033 B7).
func TestClaimOnTheWrongBotIsRefusedNotBurned(t *testing.T) {
	h := newHarness(t)
	claim := h.issueClaim(t, "family", "sam", "telegram")
	// The other Agent's bot has to exist for it to be the wrong one.
	h.agentAt(t, "admin", "telegram")

	// The other Agent's bot sees the code.
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
	if binding.Identity != "sam" || binding.Butler != agentFor("family") {
		t.Fatalf("binding = %+v", binding)
	}
}

// A human redeeming through the Console or `wild` is pinned to no bot:
// they can redeem on behalf of whoever is standing next to them, for
// whichever Agent the code was issued against, which is what a claim
// code is for.
func TestHumanRedeemIsNotPinnedToABot(t *testing.T) {
	h := newHarness(t)
	for _, surface := range []string{"family", "admin"} {
		identity := "sam"
		if surface == "admin" {
			identity = "mika"
		}
		claim := h.issueClaim(t, surface, identity, "telegram")
		var binding runtimedb.ChatBinding
		// The human names the Agent, because they have no bot to arrive
		// through — which is the whole of what "not pinned" means now.
		if resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/bindings/redeem", map[string]any{
			"code": claim.Code, "butler": agentFor(surface),
			"transport": "telegram", "account": "acct-" + surface,
		}, &binding); resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s: human redeem status = %d, want 201", surface, resp.StatusCode)
		}
		if binding.Butler != agentFor(surface) {
			t.Fatalf("binding butler = %q, want %q", binding.Butler, agentFor(surface))
		}
	}
}

// An Agent with two bots on one transport can be invited to either.
//
// B7 allows exactly this, and until the request carried a way to say
// which, allowing it made BOTH unreachable: every claim for that Agent
// and transport was ambiguous, and the refusal named no way forward, so
// giving an Owner Agent a second Telegram bot silently ended invites to
// its first one.
func TestAnAgentWithTwoBotsOnOneTransportIsInvitedToOne(t *testing.T) {
	h := newHarness(t)
	h.ensureChatPrereqs(t, "admin", "mika", "telegram")
	first := h.agentAt(t, "admin", "telegram")
	// A second bot for the same Agent, which is what B7 permits. It is
	// installed and credentialed like the first, because a code may
	// only name a bot that exists and can actually redeem it.
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindVolume,
		Metadata: resource.Metadata{
			Name: "chat-telegram-spare-state", Owner: "capability/chat-telegram-spare", Node: "n1",
		},
		Volume: &resource.VolumeSpec{Name: "chat-telegram-spare-state"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.AddButlerAddress(first.Butler, "telegram", "chat-telegram-spare", "", "root-admin"); err != nil {
		t.Fatalf("second address: %v", err)
	}
	h.adapterToken(t, "chat-telegram-spare")

	// Without a selector it is refused — and the refusal says which
	// ids would work, because a caller who cannot see them cannot
	// answer it.
	var refused struct {
		Error string `json:"error"`
	}
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/claims", map[string]any{
		"butler": first.Butler, "identity": "mika", "transport": "telegram",
	}, &refused)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ambiguous claim: status = %d, want 400", resp.StatusCode)
	}
	for _, want := range []string{first.ExternalID, "chat-telegram-spare"} {
		if !strings.Contains(refused.Error, want) {
			t.Fatalf("the refusal does not name %q: %s", want, refused.Error)
		}
	}

	// Naming one issues the code for that bot and no other.
	var out claimResponse
	resp = h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/claims", map[string]any{
		"butler": first.Butler, "identity": "mika", "transport": "telegram",
		"connection": "chat-telegram-spare",
	}, &out)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("named claim: status = %d, want 201", resp.StatusCode)
	}
	if out.Code == "" {
		t.Fatal("no code came back")
	}

	// And a bot that is not that Agent's is refused rather than
	// quietly deciding the operator meant its owner. The refusal has to
	// SAY that is why: this request is refusable on several grounds at
	// once — a family bot does not admit an owner-only identity either
	// — and a status code alone cannot tell which one answered.
	other := h.agentAt(t, "family", "telegram")
	resp = h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/claims", map[string]any{
		"butler": first.Butler, "identity": "mika", "transport": "telegram",
		"connection": other.ExternalID,
	}, &refused)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("another agent's bot: status = %d, want 400", resp.StatusCode)
	}
	if want := other.ExternalID + " is not one of " + first.Butler + "'s bots"; refused.Error != want {
		t.Fatalf("refused with %q, want %q", refused.Error, want)
	}
}

// A code is refused for a bot whose capability is no longer installed.
//
// Both halves of "this bot can take a code" are separately reachable and
// the check needs both. An address is not revoked when its capability is
// removed, and the adapter's principal is not deleted either — so a
// credential check on its own calls a bot that is not running ready, and
// the operator hands somebody an invite that can never be redeemed. That
// is the failure this check exists to prevent: it does not look like a
// failure, it looks like the person ignoring you.
func TestACodeIsRefusedForAnUninstalledBot(t *testing.T) {
	h := newHarness(t)
	h.ensureChatPrereqs(t, "family", "sam", "telegram")
	at := h.agentAt(t, "family", "telegram")

	// It works while the capability is installed.
	var out claimResponse
	if resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/claims", map[string]any{
		"butler": at.Butler, "identity": "sam", "transport": "telegram",
	}, &out); resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	// Remove the capability, leaving the address and the credential —
	// which is what removing one actually does.
	current, err := h.s.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range current {
		if r.Metadata.Owner == "capability/"+at.ExternalID {
			if err := h.s.State.Delete(r.Kind, r.Metadata.Name); err != nil {
				t.Fatalf("removing %s: %v", r.Metadata.Name, err)
			}
		}
	}

	var refused struct {
		Error string `json:"error"`
	}
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/claims", map[string]any{
		"butler": at.Butler, "identity": "sam", "transport": "telegram",
	}, &refused)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(refused.Error, "is not installed") {
		t.Fatalf("refused with %q, want it to say the bot is not installed", refused.Error)
	}
}
