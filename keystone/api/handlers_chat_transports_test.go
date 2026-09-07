package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// An invite can only name a transport this deployment has an adapter
// for (ADR-W026 A3). A fresh deployment therefore offers none, which is
// the honest answer: there is no bot to send a code to.
func TestChatTransportsAreTheInstalledAdapters(t *testing.T) {
	h := newHarness(t)

	var none []chatTransport
	if resp := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/chat/transports", nil, &none); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(none) != 0 {
		t.Fatalf("transports with nothing installed = %+v, want none", none)
	}

	// Install the family Telegram adapter, the way the capability
	// engine records it: a resource owned by the capability.
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindVolume,
		Metadata: resource.Metadata{Name: "tg-state", Owner: "capability/chat-telegram-family", Node: "n1"},
		Volume:   &resource.VolumeSpec{Name: "tg-state"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.State.Commit("test", "seed", "install the family adapter"); err != nil {
		t.Fatal(err)
	}

	var got []chatTransport
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/chat/transports", nil, &got)
	if len(got) != 1 {
		t.Fatalf("transports = %+v, want one", got)
	}
	if got[0].Transport != "telegram" || got[0].Surface != "family" || !got[0].Installed {
		t.Fatalf("transport = %+v, want an installed family telegram", got[0])
	}
	// Installed but not yet credentialed: the adapter is running and
	// cannot reach Keystone, so a code issued for it is unredeemable.
	if got[0].Credentialed {
		t.Fatal("credentialed before the adapter was given a token")
	}

	if err := h.s.Principals.Create(chatAdapterPrincipalPrefix+"chat-telegram-family", runtimedb.RoleConcierge, ""); err != nil {
		t.Fatal(err)
	}
	got = nil
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/chat/transports", nil, &got)
	if len(got) != 1 || !got[0].Credentialed {
		t.Fatalf("transports after minting = %+v, want credentialed", got)
	}
}

// The list is an access-list read: the adapters themselves cannot see
// what other ways into the house exist.
func TestChatTransportsAreManageOnly(t *testing.T) {
	h := newHarness(t)
	for _, role := range []runtimedb.Role{runtimedb.RoleOperator, runtimedb.RoleButler, runtimedb.RoleConcierge} {
		if resp := h.do(role, http.MethodGet, "/api/v1/chat/transports", nil, nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", role, resp.StatusCode)
		}
	}
}

// An invite may only name a member who exists and a transport with a
// bot behind it. Both refusals happen where the operator is standing,
// not when the person they invited says nothing happened.
func TestChatClaimIsCheckedAgainstTheDeployment(t *testing.T) {
	h := newHarness(t)

	// Nothing installed at all: the refusal says so rather than minting
	// a code with no bot to send it to.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/chat/claims", map[string]any{
		"surface": "family", "identity": "sam", "transport": "telegram",
	}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no adapter: status = %d, want 400", resp.StatusCode)
	}

	h.ensureChatPrereqs(t, "family", "sam", "telegram")

	// A member who was never declared.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/chat/claims", map[string]any{
		"surface": "family", "identity": "smA", "transport": "telegram",
	}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown member: status = %d, want 400", resp.StatusCode)
	}

	// A transport this deployment has no adapter for.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/chat/claims", map[string]any{
		"surface": "family", "identity": "sam", "transport": "matrix",
	}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("uninstalled transport: status = %d, want 400", resp.StatusCode)
	}

	// The one it does have works.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/chat/claims", map[string]any{
		"surface": "family", "identity": "sam", "transport": "telegram",
	}, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("installed transport: status = %d, want 201", resp.StatusCode)
	}
}

// doToken issues a request with an arbitrary principal's token, for the
// adapters that are not one of the harness's fixed roles.
func (h *harness) doToken(token, method, path string, body any, out any) *http.Response {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			h.t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(method, h.srv.URL+path, &buf)
	if err != nil {
		h.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			h.t.Fatalf("%s %s: decoding body: %v", method, path, err)
		}
	}
	return resp
}

// adapterToken credentials one chat capability the way `wild chat
// adapter` does, and returns its token.
func (h *harness) adapterToken(t *testing.T, capability string) string {
	t.Helper()
	name := chatAdapterPrincipalPrefix + capability
	if _, err := h.s.DB.GetPrincipal(name); err != nil {
		if err := h.s.Principals.Create(name, runtimedb.RoleConcierge, ""); err != nil {
			t.Fatal(err)
		}
	}
	tok, err := h.s.Principals.Mint(name, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// An adapter is the only part of the deployment that knows its bot's
// handle — the token is write-only — so it reports it, and that is what
// turns a claim code into something a person can tap.
func TestAdapterRegistersItsBotAndTheInviteBecomesALink(t *testing.T) {
	h := newHarness(t)
	h.ensureChatPrereqs(t, "family", "sam", "telegram")
	token := h.adapterToken(t, "chat-telegram-family")

	if resp := h.doToken(token, http.MethodPost, "/api/v1/chat/adapters/self", map[string]any{
		"transport": "telegram", "handle": "@wildstone_house_bot", "displayName": "The House",
	}, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("register: status = %d, want 200", resp.StatusCode)
	}

	var got []chatTransport
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/chat/transports", nil, &got)
	if len(got) != 1 || got[0].Handle != "wildstone_house_bot" || !got[0].Online {
		t.Fatalf("transports = %+v, want an online bot with its handle", got)
	}

	claim := h.issueClaim(t, "family", "sam", "telegram")
	want := "https://t.me/wildstone_house_bot?start=" + claim.Code
	if claim.Link != want {
		t.Fatalf("link = %q, want %q", claim.Link, want)
	}
	if !strings.Contains(claim.Note, "carries the code") {
		t.Fatalf("note = %q, want it to say the link carries the code", claim.Note)
	}
}

// The handle is untrusted text from a process holding only converse,
// and it ends up in a URL an operator taps. Anything that could carry a
// scheme, a host or a query is refused rather than escaped.
func TestAdapterHandleIsBounded(t *testing.T) {
	h := newHarness(t)
	token := h.adapterToken(t, "chat-telegram-family")

	for _, handle := range []string{
		"evil.com/bot", "bot?start=x", "bot bot", "a", "@matrix:example.org",
		strings.Repeat("b", 33), "bot#fragment", "https://t.me/bot",
	} {
		if resp := h.doToken(token, http.MethodPost, "/api/v1/chat/adapters/self", map[string]any{
			"transport": "telegram", "handle": handle,
		}, nil); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("handle %q: status = %d, want 400", handle, resp.StatusCode)
		}
	}

	// It cannot register as a transport its capability is not, either:
	// the capability name decides, not the body.
	if resp := h.doToken(token, http.MethodPost, "/api/v1/chat/adapters/self", map[string]any{
		"transport": "matrix", "handle": "@bot:example.org",
	}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong transport: status = %d, want 400", resp.StatusCode)
	}

	// And nobody but an adapter may register at all: an admin token
	// holds converse, and is still not a bot.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/chat/adapters/self", map[string]any{
		"transport": "telegram", "handle": "wildstone_house_bot",
	}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("admin register: status = %d, want 403", resp.StatusCode)
	}
}
