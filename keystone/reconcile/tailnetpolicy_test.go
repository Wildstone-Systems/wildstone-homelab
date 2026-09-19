package reconcile

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// Under the tailnet posture a password is the whole sign-in.
//
// The failure this pins: the Console's admin_tailscale Route is
// two_factor, every Route shares the tailnet hostname, and the strictest
// policy wins per hostname — so installing the Console put the family's
// homepage and every app behind a second factor whose enrolment code
// the file notifier writes somewhere only root can read.
func TestTheTailnetPostureSignsInWithAPassword(t *testing.T) {
	console := tailnetRoute("console", "console.example.com")
	console.Route.Classification = "admin_tailscale"
	desired := []resource.Resource{
		tailnetIngress("wildstone.tailnet.ts.net"),
		tailnetRoute("home", "home.example.com"),
		console,
	}
	out := string(renderAutheliaAccess(desired))
	if strings.Contains(out, "two_factor") {
		t.Errorf("the tailnet posture still demands a second factor:\n%s", out)
	}
	if got := AccessPolicies(desired)["wildstone.tailnet.ts.net"]; got != "one_factor" {
		t.Errorf("tailnet host policy = %q, want one_factor", got)
	}
}

// Only two_factor is relaxed. A deny is a deny under every posture, and
// a Route nobody classified still gets the strongest policy it can have
// there — which on the tailnet is a password, not an open door.
func TestTheTailnetPostureRelaxesNothingButTheSecondFactor(t *testing.T) {
	tailnet := ingressorigin.Of([]resource.Resource{tailnetIngress("wildstone.tailnet.ts.net")})
	for _, tc := range []struct{ in, want string }{
		{"two_factor", "one_factor"},
		{"one_factor", "one_factor"},
		{"deny", "deny"},
		{"bypass", "bypass"},
	} {
		if got := tailnetPolicy(tc.in, tailnet); got != tc.want {
			t.Errorf("tailnetPolicy(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Hostname postures keep what each package declared: they can tell one
// surface from another, so the reasons for relaxing do not apply.
func TestHostnamePosturesKeepTheSecondFactor(t *testing.T) {
	console := tailnetRoute("console", "console.example.com")
	console.Route.Classification = "admin_tailscale"
	desired := []resource.Resource{ingressRes("internal"), console}
	if got := AccessPolicies(desired)["console.example.com"]; got != "two_factor" {
		t.Errorf("hostname posture policy = %q, want two_factor", got)
	}
	client := oidcClient("vault", func(c *resource.OidcClientSpec) { c.AuthPolicy = "two_factor" })
	out := string(renderAutheliaClients([]resource.Resource{ingressRes("internal"), client}))
	if !strings.Contains(clientBlock(t, out, "vault"), "authorization_policy: 'two_factor'") {
		t.Errorf("a hostname posture dropped a client's second factor:\n%s", out)
	}
}

// The OIDC clients agree with the access rules. Were they to differ, an
// app would ask for a second factor the page in front of it did not —
// or, the other way round, admit somebody the page would have stopped.
// Group-gated clients carry the policy in their named rule, so both
// shapes are checked.
func TestTailnetClientsSignInWithAPassword(t *testing.T) {
	ungated := oidcClient("vault", func(c *resource.OidcClientSpec) { c.AuthPolicy = "two_factor" })
	gated := oidcClient("paper", func(c *resource.OidcClientSpec) {
		c.AuthPolicy = "two_factor"
		c.RequireGroups = []string{"paper_users"}
	})
	desired := []resource.Resource{tailnetIngress("wildstone.tailnet.ts.net"), ungated, gated}
	out := string(renderAutheliaClients(desired))
	if strings.Contains(out, "two_factor") {
		t.Errorf("a client still demands a second factor under the tailnet posture:\n%s", out)
	}
	// Desired state itself is untouched: the relaxation is a rendering of
	// the posture, not an edit to what the package declared.
	if ungated.OidcClient.AuthPolicy != "two_factor" {
		t.Error("rendering edited the client's declared policy")
	}
}
