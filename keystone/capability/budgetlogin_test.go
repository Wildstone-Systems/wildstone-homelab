package capability

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/ingressorigin"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// budget-actual ships two login postures behind one param, and only one
// of them is reachable from the catalog walk in pkgtool — that walk
// renders every package with its DEFAULTS, so the OIDC branch of the
// container's env block is never expanded there. A branch nothing
// renders is a branch nothing checks: the first person to set
// budget_login=oidc would be the one to discover a missing .Resolved
// key or a mis-indented conditional, at install time, on their own
// deployment.
//
// The shipped package is the subject on purpose (as in
// dashboardtitle_test.go): a fixture restating the template would keep
// passing while packages/budget-actual/package.yaml drifted.

func expandBudget(t *testing.T, login string, current []resource.Resource) []resource.Resource {
	t.Helper()
	pkg, err := pkgspec.Load(filepath.Join(repoRoot(t), "packages", "budget-actual", "package.yaml"))
	if err != nil {
		t.Fatalf("loading packages/budget-actual/package.yaml: %v", err)
	}
	identity, err := pkgspec.Load(filepath.Join(repoRoot(t), "packages", "identity", "package.yaml"))
	if err != nil {
		t.Fatalf("loading packages/identity/package.yaml: %v", err)
	}
	params := map[string]string{"public_domain": "example.com", "node": "n1"}
	if login != "" {
		params["budget_login"] = login
	}
	cat := NewCatalog(pkg, identity)
	if current != nil {
		cat = cat.WithCurrent(current)
	}
	out, err := NewEngine().Expand(pkg, params, cat)
	if err != nil {
		t.Fatalf("Expand(budget_login=%q): %v", login, err)
	}
	return out
}

// expandBudget2 is expandBudget with the params spelled out, for the
// cases that vary something other than the login mode.
func expandBudget2(t *testing.T, params map[string]string, current []resource.Resource) []resource.Resource {
	t.Helper()
	pkg, err := pkgspec.Load(filepath.Join(repoRoot(t), "packages", "budget-actual", "package.yaml"))
	if err != nil {
		t.Fatalf("loading packages/budget-actual/package.yaml: %v", err)
	}
	identity, err := pkgspec.Load(filepath.Join(repoRoot(t), "packages", "identity", "package.yaml"))
	if err != nil {
		t.Fatalf("loading packages/identity/package.yaml: %v", err)
	}
	merged := map[string]string{"public_domain": "example.com", "node": "n1"}
	for k, v := range params {
		merged[k] = v
	}
	cat := NewCatalog(pkg, identity)
	if current != nil {
		cat = cat.WithCurrent(current)
	}
	out, err := NewEngine().Expand(pkg, merged, cat)
	if err != nil {
		t.Fatalf("Expand(%v): %v", params, err)
	}
	return out
}

func budgetContainerEnv(t *testing.T, res []resource.Resource) map[string]string {
	t.Helper()
	for _, r := range res {
		if r.Kind == resource.KindContainer && r.Container != nil && r.Metadata.Name == "actual" {
			return r.Container.Env
		}
	}
	t.Fatal("no Container/actual in the expanded package")
	return nil
}

// The default is the posture the package shipped with, and it must stay
// that way: budget_login is opt-in precisely because the first OIDC
// sign-in takes permanent ownership of the Actual server. A default that
// drifted to "oidc" would hand that to whoever visited first.
func TestBudgetDefaultsToPasswordAndSetsNoOpenIDEnv(t *testing.T) {
	for _, login := range []string{"", "password"} {
		env := budgetContainerEnv(t, expandBudget(t, login, nil))
		for name := range env {
			if strings.HasPrefix(name, "ACTUAL_OPENID_") {
				t.Errorf("budget_login=%q set %s; the password posture must configure no OpenID at all", login, name)
			}
		}
		if env["ACTUAL_PORT"] != "5006" {
			t.Errorf("budget_login=%q lost ACTUAL_PORT: got %q", login, env["ACTUAL_PORT"])
		}
	}
}

// The opt-in branch, which nothing else renders.
func TestBudgetOidcModeConfiguresActualAgainstAuthelia(t *testing.T) {
	res := expandBudget(t, "oidc", nil)
	env := budgetContainerEnv(t, res)

	for key, want := range map[string]string{
		"ACTUAL_OPENID_DISCOVERY_URL":   "https://auth.example.com/.well-known/openid-configuration",
		"ACTUAL_OPENID_CLIENT_ID":       "actual",
		"ACTUAL_OPENID_AUTH_METHOD":     "openid",
		"ACTUAL_OPENID_SERVER_HOSTNAME": "https://budget.example.com",
	} {
		if env[key] != want {
			t.Errorf("%s\n got %q\nwant %q", key, env[key], want)
		}
	}
	// The secret reaches the container as a reference, never a value:
	// env carries "secret/<name>" and the agent substitutes it.
	if ref := env["ACTUAL_OPENID_CLIENT_SECRET"]; !strings.HasPrefix(ref, "secret/") {
		t.Errorf("ACTUAL_OPENID_CLIENT_SECRET is %q, not a secret/ reference", ref)
	}
	// Enforcement is a one-way door in Actual — it has no config to undo
	// it — so the package must never set it. Losing the server password
	// also locks out the apps and any bank-sync tooling, which sign in
	// to the server directly rather than through a browser.
	if v, set := env["ACTUAL_OPENID_ENFORCE"]; set {
		t.Errorf("ACTUAL_OPENID_ENFORCE is set to %q: the server password is the way back in", v)
	}
}

// The registration exists ONLY in oidc mode, and the redirect must be
// the path Actual actually listens on: a client whose redirect_uri
// disagrees fails the exchange at the provider, after the person has
// already authenticated.
func TestBudgetRegistersItsOidcClientOnlyInOidcMode(t *testing.T) {
	for _, r := range expandBudget(t, "password", nil) {
		if r.Kind == resource.KindOidcClient {
			t.Fatalf("password mode registered OidcClient %q: a client the app was never told about is "+
				"half a login, held by Authelia and redeemed by nobody", r.Metadata.Name)
		}
	}

	var found *resource.OidcClientSpec
	var secretNames []string
	for _, r := range expandBudget(t, "oidc", nil) {
		switch {
		case r.Kind == resource.KindOidcClient && r.OidcClient != nil:
			found = r.OidcClient
		case r.Kind == resource.KindSecret && r.Secret != nil:
			secretNames = append(secretNames, r.Secret.Name)
		}
	}
	if found == nil {
		t.Fatal("oidc mode rendered no OidcClient")
	}
	want := "https://budget.example.com/openid/callback"
	if len(found.RedirectUris) != 1 || found.RedirectUris[0] != want {
		t.Errorf("redirectUris\n got %v\nwant [%s]", found.RedirectUris, want)
	}
	// The engine synthesises the client secret FROM the OidcClient. If
	// that stops happening the node refuses to write the merged clients
	// file, which is a failure far from here.
	name := strings.TrimPrefix(found.SecretRef, "secret/")
	var have bool
	for _, s := range secretNames {
		if s == name {
			have = true
		}
	}
	if !have {
		t.Errorf("no Secret %q for the client secret; got %v", name, secretNames)
	}
	// And it goes away with the client: a generated secret left on a
	// node for a client nobody registered is an orphan no uninstall
	// accounts for.
	for _, r := range expandBudget(t, "password", nil) {
		if r.Kind == resource.KindSecret && r.Secret != nil && r.Secret.Name == name {
			t.Errorf("password mode still declares the client secret %q", name)
		}
	}
}

// Under the tailnet posture the browser never sees budget.example.com,
// and ACTUAL_OPENID_SERVER_HOSTNAME is compared against what it DID see.
// This is the value that would have been wrong had the package
// concatenated params, and the symptom is a redirect error with no
// password fallback if enforcement were ever turned on.
func TestBudgetOidcHostnameFollowsTheTailnetPosture(t *testing.T) {
	current := append(tailnetState(), resource.Resource{
		Kind:     resource.KindRoute,
		Metadata: resource.Metadata{Name: "budget", Node: "n1"},
		Route: &resource.RouteSpec{Host: "budget.example.com", Upstream: "container/actual:5006",
			Auth: "none", Classification: "family_private"},
	})
	env := budgetContainerEnv(t, expandBudget(t, "oidc", current))
	if got := env["ACTUAL_OPENID_SERVER_HOSTNAME"]; !strings.HasPrefix(got, "https://wildstone.tailnet.ts.net") {
		t.Errorf("ACTUAL_OPENID_SERVER_HOSTNAME is %q, which is not where the tailnet posture serves this route", got)
	}
	if got := env["ACTUAL_OPENID_DISCOVERY_URL"]; !strings.HasPrefix(got, "https://wildstone.tailnet.ts.net") {
		t.Errorf("ACTUAL_OPENID_DISCOVERY_URL is %q, which is not where the tailnet posture serves Authelia", got)
	}
}

// A custom budget_subdomain is the case the pinned default hides. The
// pin in keystone/ingressorigin follows the DEFAULT label, so
// "money.example.com" is an unpinned Route — and budget-actual in oidc
// mode is the first shipped package to need a self-origin for one.
//
// What must hold is not that the port is any particular number, but
// that the number expansion PREDICTS is the number the assignment later
// HANDS OUT. The two run at completely different times: .Resolved feeds
// the templates that declare the Route, so it necessarily runs while
// that Route is still absent from desired state. A prediction that
// disagreed would be internally consistent on both sides and wrong
// between them (F70) — Actual building its callback for one port while
// Caddy serves it on another, which reads as a redirect error and names
// nothing.
func TestBudgetOidcHostnameAgreesWithTheEventualPortForACustomLabel(t *testing.T) {
	const host = "money.example.com"
	res := expandBudget2(t, map[string]string{"budget_login": "oidc", "budget_subdomain": "money"}, tailnetState())

	predicted := budgetContainerEnv(t, res)["ACTUAL_OPENID_SERVER_HOSTNAME"]
	if strings.Contains(predicted, host) {
		t.Fatalf("ACTUAL_OPENID_SERVER_HOSTNAME is %q — the declared host, which the tailnet posture does not serve", predicted)
	}

	// Now the Route this very expansion declared IS in desired state,
	// which is the state the reconciler assigns ports from.
	desired := tailnetState()
	for _, r := range res {
		if r.Kind == resource.KindRoute {
			desired = append(desired, r)
		}
	}
	if assigned := ingressorigin.Of(desired).OriginFor(host); assigned != predicted {
		t.Errorf("expansion predicted %q for an unpinned label, but the assignment hands out %q\n"+
			"Actual would build its callback for one and Caddy would serve it on the other", predicted, assigned)
	}
}
