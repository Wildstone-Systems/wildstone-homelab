package api

// The Console's forward-auth path (ADR-W032).
//
// This is the one authentication path that constructs an administrator
// out of two request headers, so the tests here are mostly about what it
// REFUSES. Each of the three conditions is exercised alone, because a
// path that is safe only when all three happen to hold is a path one
// refactor away from not being safe at all.

import (
	"net/http"
	"testing"
)

// forwarded issues a request the way the identity proxy would: no bearer
// token, a Remote-User, and whatever group list the case is testing.
func (h *harness) forwarded(method, path, user, groups string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, nil)
	if err != nil {
		h.t.Fatalf("NewRequest: %v", err)
	}
	if user != "" {
		req.Header.Set(TrustedIngressHeader, user)
	}
	if groups != "" {
		req.Header.Set(ConsoleGroupsHeader, groups)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	resp.Body.Close()
	return resp
}

// The group is the permission. A directory account in it reaches the
// Console with no token minted for them by hand, which is the whole
// point: membership is managed where every other service's is.
func TestTheConsoleGroupIsWhatGrantsTheConsole(t *testing.T) {
	h := newHarness(t)
	h.s.TrustedIngress = []string{"127.0.0.1", "::1"}

	if res := h.forwarded(http.MethodGet, "/api/v1/butlers", "giulia", ConsoleGroup); res.StatusCode != http.StatusOK {
		t.Fatalf("a person in %s was refused the Console: %d", ConsoleGroup, res.StatusCode)
	}
	// And it is real authority, not a read-only impression of one: the
	// Butlers screen is almost entirely manage-verb endpoints, so a role
	// without manage would render and then refuse every control.
	if res := h.forwarded(http.MethodDelete, "/api/v1/butlers/nobody", "giulia", ConsoleGroup); res.StatusCode == http.StatusForbidden {
		t.Fatal("the forwarded identity holds no manage verb, so the Console would render controls that all 403")
	}
}

// Every refusal, one condition at a time.
func TestTheConsoleForwardAuthRefusesEverythingElse(t *testing.T) {
	h := newHarness(t)
	h.s.TrustedIngress = []string{"127.0.0.1", "::1"}

	for _, c := range []struct {
		what, user, groups string
		trusted            bool
	}{
		{what: "a signed-in person who is not in the group", user: "mika", groups: "media_users", trusted: true},
		{what: "no group header at all", user: "mika", groups: "", trusted: true},
		{what: "no user header", user: "", groups: ConsoleGroup, trusted: true},
		// The peer check is the one that matters most: everything else
		// here is a header the caller controls.
		{what: "the right headers from an untrusted peer", user: "mika", groups: ConsoleGroup, trusted: false},
	} {
		if !c.trusted {
			h.s.TrustedIngress = nil
		} else {
			h.s.TrustedIngress = []string{"127.0.0.1", "::1"}
		}
		if res := h.forwarded(http.MethodGet, "/api/v1/butlers", c.user, c.groups); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", c.what, res.StatusCode)
		}
	}
}

// An unconfigured trust boundary fails closed.
//
// This is the deployment state on a host that has not put the Console
// behind the proxy yet, and it must be "this path does not exist" rather
// than "this path trusts anybody".
func TestTheConsoleForwardAuthIsOffUntilTheProxyIsNamed(t *testing.T) {
	h := newHarness(t)
	if len(h.s.TrustedIngress) != 0 {
		t.Fatal("TrustedIngress is set by default; the header path would be live on a fresh install")
	}
	if res := h.forwarded(http.MethodGet, "/api/v1/butlers", "attacker", ConsoleGroup); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("with no trusted ingress configured the header path answered %d, want 401", res.StatusCode)
	}
}

// A group name is matched exactly.
//
// Group names are the directory's namespace and somebody will eventually
// add a neighbouring one. A prefix or substring match would hand
// "console_users_readonly" everything "console_users" has.
func TestTheConsoleGroupIsMatchedExactly(t *testing.T) {
	h := newHarness(t)
	h.s.TrustedIngress = []string{"127.0.0.1", "::1"}

	for _, groups := range []string{"console_users_readonly", "not_console_users", "console_user", "console_userss"} {
		if res := h.forwarded(http.MethodGet, "/api/v1/butlers", "mika", groups); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("group %q was accepted as %q: %d", groups, ConsoleGroup, res.StatusCode)
		}
	}
	// A list, as Authelia actually sends it.
	if res := h.forwarded(http.MethodGet, "/api/v1/butlers", "mika", "media_users, console_users ,dev"); res.StatusCode != http.StatusOK {
		t.Fatalf("a comma-separated list containing the group was refused: %d", res.StatusCode)
	}
}

// A token always decides when one is offered, even a bad one.
//
// Otherwise a revoked or expired credential quietly becomes whatever the
// headers claim — an escalation arriving through the failure path, which
// is the shape of bug that survives review because it looks like a
// fallback.
func TestABadTokenDoesNotFallThroughToTheHeaders(t *testing.T) {
	h := newHarness(t)
	h.s.TrustedIngress = []string{"127.0.0.1", "::1"}

	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/butlers", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer this-token-was-revoked")
	req.Header.Set(TrustedIngressHeader, "giulia")
	req.Header.Set(ConsoleGroupsHeader, ConsoleGroup)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a rejected token fell through to the forwarded headers: %d", res.StatusCode)
	}
}

// The audit trail names the person, not a shared administrator.
//
// "who could have seen this" is the question the whole Butler design
// exists to answer, and it is worth nothing if every forwarded request
// is filed under one name.
func TestAForwardedRequestIsAuditedUnderTheirOwnName(t *testing.T) {
	h := newHarness(t)
	h.s.TrustedIngress = []string{"127.0.0.1", "::1"}

	if res := h.forwarded(http.MethodGet, "/api/v1/butlers", "giulia", ConsoleGroup); res.StatusCode != http.StatusOK {
		t.Fatalf("setup: %d", res.StatusCode)
	}
	records, err := h.s.DB.ListAudit(20)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		if rec.Path == "/api/v1/butlers" {
			if rec.Principal != "giulia" && rec.ActingFor != "giulia" {
				t.Fatalf("audited as principal=%q actingFor=%q, want giulia", rec.Principal, rec.ActingFor)
			}
			return
		}
	}
	t.Fatal("the forwarded request was not audited at all")
}
