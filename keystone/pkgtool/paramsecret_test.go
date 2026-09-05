package pkgtool

import "testing"

// TestSecretishParamNames pins ADR-W021 A2's lint rule and, as much as
// the rule itself, its narrowness. A param's value is recorded in the
// plan's Origin and reaches the audit trail, so a package asking for a
// password through a param creates a second path for secret material
// beside the one designed for it.
//
// The negative cases are the load-bearing half: every one of them is a
// real param name from a shipped package, and a broader match would
// reject them and make the rule useless.
func TestSecretishParamNames(t *testing.T) {
	for _, name := range []string{
		"admin_password", "PASSWORD", "oidc_secret", "api_key", "apikey",
		"tunnel_token", "private_key", "db_credential",
	} {
		if secretishParamWord(name) == "" {
			t.Errorf("%q should be rejected as secret material", name)
		}
	}
	for _, name := range []string{
		"public_domain", "node", "tunnel_id", "ldap_base_dn", "admin_listen_addr",
		"listen_port", "media_path", "timezone", "docs_subdomain", "identity_node",
	} {
		if w := secretishParamWord(name); w != "" {
			t.Errorf("%q is a real shipped param and was rejected on %q", name, w)
		}
	}
}
