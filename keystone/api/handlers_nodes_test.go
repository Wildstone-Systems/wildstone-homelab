package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
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
	if minted != 2 {
		t.Fatalf("mint events = %d, want 2", minted)
	}
}
