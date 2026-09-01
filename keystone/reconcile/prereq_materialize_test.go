package reconcile

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// The materialize flag on a prereq write, and the rule nothing was
// checking.
//
// Keystone decides whether to ASK for secret materialization; the agent
// decides whether to ACCEPT the request, and refuses one with nothing to
// materialize. Both halves are right. What was missing was anything that
// held them to the same answer — the flag was a constant `true` on the
// clients file, and the test below used to assert that constant while
// passing the zero-client case that makes it false.
//
// The result was that the FIRST install anyone performs failed. Identity
// is always installed before anything that consumes it, so it always has
// zero OIDC clients, so the renderer emits a comment-only file, so the
// agent refuses the write:
//
//	prereq write /etc/wildstone/authelia/clients.yml: ERROR_CODE_INVALID:
//	materialize_secrets set but the content carries no secret reference

// TestPrereqMaterializeMatchesItsContent asserts the PAIRING rather than
// any particular file's flag. A prereq is written with materialization
// exactly when its content carries a reference — no file is special.
func TestPrereqMaterializeMatchesItsContent(t *testing.T) {
	cases := map[string][]resource.Resource{
		"a fresh install, no consumers yet": nil,
		"one app registered": {
			oidcClientResource("photos", "photos", "secret/photos-oidc"),
		},
	}
	for name, desired := range cases {
		t.Run(name, func(t *testing.T) {
			for _, f := range nodePrereqFiles(testManagedPrefix, desired) {
				want := configFileNeedsMaterialization(string(f.want))
				if f.materialize != want {
					t.Errorf("%s: materialize=%v, but the content carries a secret reference: %v",
						f.path, f.materialize, want)
				}
			}
		})
	}
}

// TestZeroClientsPrereqIsWritable keeps the specific VM failure as its
// own case. This is not an edge case: zero OidcClients is what every
// install starts as.
func TestZeroClientsPrereqIsWritable(t *testing.T) {
	for _, f := range nodePrereqFiles(testManagedPrefix, nil) {
		if f.path != autheliaClientsPath(testManagedPrefix) {
			continue
		}
		if f.materialize {
			t.Fatal("the clients file asks for materialization with no OidcClient declared. " +
				"The agent refuses that write, so plan-1 of a fresh install fails and the " +
				"reconcile loop retries it forever")
		}
		return
	}
	t.Fatal("the clients file is not among the node prereqs")
}

// TestKeystoneAgreesWithTheAgentOnSecretReferences is the cross-package
// check whose absence let this ship. The two rules live in modules that
// cannot import each other, so nothing compared them. This pins
// Keystone's side against the shapes the agent's rule accepts and
// rejects; change one without the other and this fails.
func TestKeystoneAgreesWithTheAgentOnSecretReferences(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"the zero-client clients file", string(renderAutheliaClients(nil)), false},
		{"a rendered client secret", "client_secret: 'pbkdf2:secret/photos-oidc'\n", true},
		{"a plain reference", "password: secret/lldap-admin-password\n", true},
		{"ordinary config", "theme: light\nlog:\n  level: info\n", false},
		{"empty", "", false},
		{"the rendered group list", string(renderLdapGroups(nil)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := configFileNeedsMaterialization(tc.content); got != tc.want {
				t.Errorf("configFileNeedsMaterialization = %v, want %v, for:\n%s", got, tc.want, tc.content)
			}
		})
	}
}
