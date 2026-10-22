package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestMintJoinToken(t *testing.T) {
	h := newHarness(t)

	// Unwired: 503 even for the admin.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/join-tokens", nil, nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unwired = %d, want 503", resp.StatusCode)
	}

	fakeToken := strings.Repeat("ab", 32) // 64 hex — the shape redaction eats
	var gotTTL time.Duration
	h.s.MintJoin = func(ttl time.Duration) (string, error) {
		gotTTL = ttl
		return fakeToken, nil
	}
	h.s.CAFingerprint = strings.Repeat("cd", 32)

	// manage verb: operator and butler are refused.
	for _, role := range []runtimedb.Role{runtimedb.RoleOperator, runtimedb.RoleButler} {
		if resp := h.do(role, http.MethodPost, "/api/v1/join-tokens", nil, nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("role %s = %d, want 403", role, resp.StatusCode)
		}
	}

	// The admin gets the token back VERBATIM (the ADR-W014 carve-out: a
	// redacted pairing code would be useless) plus the fingerprint and a
	// ready-to-run install command carrying both.
	var out joinTokenResponse
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/join-tokens", nil, &out); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint = %d, want 201", resp.StatusCode)
	}
	if out.JoinToken != fakeToken {
		t.Fatalf("joinToken = %q — redaction ate the pairing code", out.JoinToken)
	}
	if out.CAFingerprint != h.s.CAFingerprint || out.ExpiresInSec != int64(defaultJoinTTL.Seconds()) {
		t.Fatalf("response = %+v", out)
	}
	if gotTTL != defaultJoinTTL {
		t.Fatalf("minted ttl = %s, want default %s", gotTTL, defaultJoinTTL)
	}
	if !strings.Contains(out.Install, "--join-token "+fakeToken) ||
		!strings.Contains(out.Install, "--ca-fingerprint "+h.s.CAFingerprint) {
		t.Fatalf("install command = %q, want token and fingerprint baked in", out.Install)
	}
	// With no advertised address the command must show a placeholder AND
	// say so — a silent loopback address is a node that never connects.
	if !strings.Contains(out.Install, addrPlaceholder) {
		t.Fatalf("install command = %q, want the address placeholder", out.Install)
	}
	if !strings.Contains(strings.Join(out.InstallNotes, "\n"), addrPlaceholder) {
		t.Fatalf("notes = %q, want the placeholder explained", out.InstallNotes)
	}
	// The private-repo form is what actually works while this repo is
	// private: install.sh fetched with an authenticated client, not piped
	// from an anonymous curl that 404s.
	if strings.Contains(out.InstallPrivateRepo, "curl") ||
		!strings.Contains(out.InstallPrivateRepo, "gh release download") ||
		!strings.Contains(out.InstallPrivateRepo, "--join-token "+fakeToken) {
		t.Fatalf("private install = %q", out.InstallPrivateRepo)
	}

	// An advertised gateway address is filled in, leaving nothing to edit.
	h.s.GatewayAddr = "keystone.example:9440"
	var addressed joinTokenResponse
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/join-tokens", nil, &addressed); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint with address = %d, want 201", resp.StatusCode)
	}
	if !strings.Contains(addressed.Install, "--keystone keystone.example:9440") ||
		strings.Contains(addressed.Install, addrPlaceholder) {
		t.Fatalf("install command = %q, want the advertised address", addressed.Install)
	}
	h.s.GatewayAddr = ""

	// Custom TTL honored; over the cap refused.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/join-tokens", map[string]int64{"ttlSeconds": 3600}, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("custom ttl = %d", resp.StatusCode)
	}
	if gotTTL != time.Hour {
		t.Fatalf("minted ttl = %s, want 1h", gotTTL)
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/join-tokens", map[string]int64{"ttlSeconds": 999999}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("over-cap ttl accepted")
	}

	// Provenance: the mint is on the event stream, name-level only.
	events, err := h.s.DB.EventsAfter(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	minted := 0
	for _, ev := range events {
		if ev.Type == "node.join-token-minted" {
			minted++
			if strings.Contains(ev.Message, fakeToken) {
				t.Fatal("event carries the join token value")
			}
		}
	}
	// One event per successful mint: the default-TTL mint, the
	// advertised-address mint, and the custom-TTL mint. The over-cap
	// attempt is refused before it mints, so it leaves no event.
	if minted != 3 {
		t.Fatalf("mint events = %d, want 3", minted)
	}
}

// TestListNodes covers the inventory route (#30): durable rows joined with
// live sessions, with liveness never coming from stored state.
func TestListNodes(t *testing.T) {
	h := newHarness(t)

	type nodesResp struct {
		Nodes []struct {
			ID             string     `json:"id"`
			Connected      bool       `json:"connected"`
			AgentVersion   string     `json:"agentVersion"`
			ProtocolMinor  uint32     `json:"protocolMinor"`
			ConnectedSince *time.Time `json:"connectedSince"`
		} `json:"nodes"`
	}

	// Empty fleet: an empty list, not a 404.
	var out nodesResp
	if resp := h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/nodes", nil, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("empty inventory = %d, want 200", resp.StatusCode)
	}
	if len(out.Nodes) != 0 {
		t.Fatalf("fresh install listed %d nodes", len(out.Nodes))
	}

	if err := h.s.DB.RecordNodeJoin("vm-1"); err != nil {
		t.Fatalf("RecordNodeJoin: %v", err)
	}
	if err := h.s.DB.RecordNodeSeen("vm-1", "v0.1.0-alpha", 7); err != nil {
		t.Fatalf("RecordNodeSeen: %v", err)
	}
	if err := h.s.DB.RecordNodeJoin("vm-2"); err != nil {
		t.Fatalf("RecordNodeJoin: %v", err)
	}

	// No gateway wired: both nodes are listed, both disconnected. The
	// inventory is still the useful half of the answer.
	out = nodesResp{}
	h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/nodes", nil, &out)
	if len(out.Nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(out.Nodes))
	}
	for _, n := range out.Nodes {
		if n.Connected {
			t.Errorf("node %s reported connected with no gateway wired", n.ID)
		}
	}

	// With a session for vm-1 only, liveness follows the gateway — and the
	// connected node's version comes from the live Hello, not the row.
	since := time.Now().UTC().Add(-90 * time.Second)
	h.s.LiveNodes = func() []LiveNode {
		return []LiveNode{{NodeID: "vm-1", AgentVersion: "v0.2.0", ProtocolMinor: 8, Since: since}}
	}
	out = nodesResp{}
	h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/nodes", nil, &out)
	if len(out.Nodes) != 2 || out.Nodes[0].ID != "vm-1" || out.Nodes[1].ID != "vm-2" {
		t.Fatalf("want id-ordered vm-1, vm-2; got %+v", out.Nodes)
	}
	if !out.Nodes[0].Connected || out.Nodes[1].Connected {
		t.Errorf("liveness wrong: vm-1=%v vm-2=%v", out.Nodes[0].Connected, out.Nodes[1].Connected)
	}
	if out.Nodes[0].AgentVersion != "v0.2.0" || out.Nodes[0].ProtocolMinor != 8 {
		t.Errorf("connected node should report the live Hello, got %+v", out.Nodes[0])
	}
	if out.Nodes[0].ConnectedSince == nil || !out.Nodes[0].ConnectedSince.Equal(since) {
		t.Errorf("connectedSince = %v, want %v", out.Nodes[0].ConnectedSince, since)
	}
	if out.Nodes[1].ConnectedSince != nil {
		t.Errorf("disconnected node carries connectedSince %v", out.Nodes[1].ConnectedSince)
	}

	// A session with no durable row still shows up: better than pretending
	// the fleet is smaller than it is.
	h.s.LiveNodes = func() []LiveNode {
		return []LiveNode{{NodeID: "vm-3", AgentVersion: "v0.2.0", ProtocolMinor: 8, Since: since}}
	}
	out = nodesResp{}
	h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/nodes", nil, &out)
	if len(out.Nodes) != 3 || out.Nodes[2].ID != "vm-3" || !out.Nodes[2].Connected {
		t.Fatalf("unrecorded live session missing from inventory: %+v", out.Nodes)
	}

	// read verb: the Butler can see the fleet, and authn is still required.
	if resp := h.do(runtimedb.RoleButler, http.MethodGet, "/api/v1/nodes", nil, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("butler read = %d, want 200", resp.StatusCode)
	}
	if resp := h.do("", http.MethodGet, "/api/v1/nodes", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d, want 401", resp.StatusCode)
	}
}
