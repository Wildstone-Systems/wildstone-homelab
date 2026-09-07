package api

import (
	"net/http"
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
