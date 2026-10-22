package capability

import (
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// Epic #326 S02: a placement carries the connection's own non-secret
// environment (the Person's login and authorized filters), and that
// channel can neither shadow the contract nor carry a credential.
func TestExpandAdapterRendersPlacementEnv(t *testing.T) {
	pkg := loadMail(t)
	base := AdapterPlacement{
		Connection: "ada-mailbox", Node: "node-b", KeystoneURL: "http://192.0.2.10:9441",
		TokenSecret: "mail-adapter-ada-mailbox-token",
		Secrets:     map[string]string{"mailbox-app-password": "pc.ada-mailbox.v1"},
	}
	pl := base
	pl.Env = map[string]string{
		"WILD_MAILBOX_USERNAME": "ada@example.internal",
		"WILD_MAILBOX_FILTERS":  `{"folders":["INBOX"]}`,
	}
	out, err := NewEngine().ExpandAdapter(pkg, mailParams, "mail", pl)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out {
		if r.Kind == resource.KindContainer {
			if r.Container.Env["WILD_MAILBOX_USERNAME"] != "ada@example.internal" ||
				r.Container.Env["WILD_MAILBOX_FILTERS"] != `{"folders":["INBOX"]}` {
				t.Fatalf("env = %v", r.Container.Env)
			}
		}
	}
	for name, env := range map[string]map[string]string{
		"shadows the connection":  {"WILD_CONNECTION": "other"},
		"claims the param prefix": {"WILD_PARAM_IMAP_HOST": "imap.attacker.example"},
		"claims a secret need":    {"WILD_SECRET_MAILBOX_APP_PASSWORD": "x"},
		"carries a secret ref":    {"WILD_MAILBOX_USERNAME": "secret/pc.bo-mailbox.v1"},
		"outside WILD_":           {"HTTPS_PROXY": "http://192.0.2.99"},
	} {
		pl := base
		pl.Env = env
		if _, err := NewEngine().ExpandAdapter(pkg, mailParams, "mail", pl); err == nil ||
			!strings.Contains(err.Error(), "placement env") {
			t.Errorf("%s: rendered (%v)", name, err)
		}
	}
}
