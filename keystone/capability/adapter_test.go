package capability

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// Epic #326 S01: installing mail-roundcube bundles the adapter but runs
// none of it, and a placement renders one contained adapter that
// inherits the provider endpoint and no credential.

func loadMail(t *testing.T) *pkgspec.Package {
	t.Helper()
	pkg, err := pkgspec.Load("../../packages/mail-roundcube/package.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

var mailParams = map[string]string{
	"public_domain": "example.internal", "node": "node-a",
	"imap_host": "imap.example.internal", "imap_port": "993", "imap_encryption": "ssl",
	"smtp_host": "smtp.example.internal", "username_domain": "example.internal",
}

// Installing (and, identically, upgrading — an upgrade is this same
// expansion diffed against what runs) produces no adapter container, no
// member credential, and no adapter token. Nothing here can read a
// mailbox, and nothing Personal exists to be read with.
func TestInstallingMailRendersNoAdapter(t *testing.T) {
	pkg := loadMail(t)
	in, _ := pkg.AdapterFor("mail")
	out, err := NewEngine().Expand(pkg, mailParams, NewCatalog(pkg))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out {
		if strings.HasPrefix(r.Metadata.Owner, "integration/") {
			t.Errorf("install rendered %s/%s owned by %s", r.Kind, r.Metadata.Name, r.Metadata.Owner)
		}
		if r.Kind == resource.KindContainer {
			for _, arg := range r.Container.Command {
				if strings.Contains(arg, in.Adapter.Program) {
					t.Errorf("install rendered the adapter program in %s", r.Metadata.Name)
				}
			}
			for k := range r.Container.Env {
				if strings.HasPrefix(k, AdapterEnvSecretPrefix) || k == AdapterEnvKeystoneToken {
					t.Errorf("install gave %s an adapter credential %s", r.Metadata.Name, k)
				}
			}
		}
		if r.Kind == resource.KindSecret {
			for _, need := range in.Adapter.Needs.Secrets {
				if strings.Contains(r.Secret.Name, need.Name) {
					t.Errorf("install rendered the member credential %s", r.Secret.Name)
				}
			}
		}
	}
}

func TestExpandAdapterRendersOneContainedAdapter(t *testing.T) {
	pkg := loadMail(t)
	out, err := NewEngine().ExpandAdapter(pkg, mailParams, "mail", AdapterPlacement{
		Connection: "ada-mailbox", Node: "node-b", KeystoneURL: "http://192.0.2.10:9441",
		TokenSecret: "mail-adapter-ada-mailbox-token",
		Secrets: map[string]string{
			"mailbox-app-password":    "mail-ada-mailbox-app-password",
			"mailbox-fingerprint-key": "mail-ada-mailbox-fingerprint-key",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var c *resource.Resource
	secrets := map[string]bool{}
	for i := range out {
		r := &out[i]
		if r.Metadata.Owner != "integration/ada-mailbox" || r.Metadata.Node != "node-b" || r.Metadata.Package != "mail-roundcube" {
			t.Fatalf("%s/%s metadata = %+v", r.Kind, r.Metadata.Name, r.Metadata)
		}
		switch r.Kind {
		case resource.KindContainer:
			c = r
		case resource.KindSecret:
			if !r.Secret.Provided {
				t.Fatalf("secret %s would be generated; adapter credentials are provided, never invented", r.Secret.Name)
			}
			secrets[r.Secret.Name] = true
		}
	}
	if c == nil || c.Metadata.Name != "mail-adapter-ada-mailbox" {
		t.Fatalf("no adapter container in %+v", out)
	}
	if !secrets["mail-adapter-ada-mailbox-token"] || !secrets["mail-ada-mailbox-app-password"] || !secrets["mail-ada-mailbox-fingerprint-key"] || len(secrets) != 3 {
		t.Fatalf("secrets = %v", secrets)
	}
	spec := c.Container
	if strings.Join(spec.Command, " ") != "/opt/wildstone/bin/wild-mail-adapter" {
		t.Fatalf("command = %v", spec.Command)
	}
	want := map[string]string{
		"KEYSTONE_API": "http://192.0.2.10:9441", "KEYSTONE_TOKEN": "secret/mail-adapter-ada-mailbox-token",
		"WILD_CONNECTION": "ada-mailbox", "WILD_NODE": "node-b",
		"WILD_PACKAGE": "mail-roundcube", "WILD_PACKAGE_VERSION": pkg.Metadata.Version, "WILD_ADAPTER_CONTRACT": "1",
		"WILD_PARAM_IMAP_HOST": "imap.example.internal", "WILD_PARAM_IMAP_PORT": "993",
		"WILD_PARAM_IMAP_ENCRYPTION": "ssl", "WILD_PARAM_USERNAME_DOMAIN": "example.internal",
		"WILD_SECRET_MAILBOX_APP_PASSWORD":    "secret/mail-ada-mailbox-app-password",
		"WILD_SECRET_MAILBOX_FINGERPRINT_KEY": "secret/mail-ada-mailbox-fingerprint-key",
	}
	for k, v := range want {
		if spec.Env[k] != v {
			t.Errorf("env %s = %q, want %q", k, spec.Env[k], v)
		}
	}
	if len(spec.Env) != len(want) {
		t.Errorf("env carries more than the contract: %v", spec.Env)
	}
	// Not the SMTP side, not the calendar admin — only what was inherited.
	for k, v := range spec.Env {
		if strings.Contains(v, "smtp") || strings.Contains(strings.ToLower(k), "smtp") {
			t.Errorf("env %s=%s was not inherited", k, v)
		}
		if strings.HasPrefix(k, AdapterEnvSecretPrefix) && !strings.HasPrefix(v, "secret/") {
			t.Errorf("credential %s is a value, not a reference", k)
		}
	}
	if len(spec.Mounts) != 1 || !spec.Mounts[0].ReadOnly || spec.Mounts[0].Source != "/srv/wildstone/bin" {
		t.Fatalf("mounts = %+v", spec.Mounts)
	}
	// Hardened, which also means an older wardstoned is refused the spec.
	if spec.RequiredProtocolMinor() < resource.HardeningProtocolMinor {
		t.Fatalf("adapter needs protocol minor %d; it must need the hardening minor so an older agent is refused",
			spec.RequiredProtocolMinor())
	}
}

// With no credential named by the placement, the adapter still renders —
// it will answer not_configured — and carries no secret reference.
func TestExpandAdapterWithoutACredentialIsNotConfigured(t *testing.T) {
	out, err := NewEngine().ExpandAdapter(loadMail(t), mailParams, "mail", AdapterPlacement{
		Connection: "ada", Node: "node-a", KeystoneURL: "http://192.0.2.10:9441", TokenSecret: "mail-adapter-ada-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out {
		if r.Kind == resource.KindContainer {
			for k := range r.Container.Env {
				if strings.HasPrefix(k, AdapterEnvSecretPrefix) {
					t.Fatalf("env %s present with no credential placed", k)
				}
			}
		}
	}
}

func TestExpandAdapterRefusesWhatItCannotRender(t *testing.T) {
	pkg := loadMail(t)
	ok := AdapterPlacement{Connection: "ada", Node: "node-a", KeystoneURL: "http://192.0.2.10:9441", TokenSecret: "t"}
	if _, err := NewEngine().ExpandAdapter(pkg, mailParams, "calendar", ok); err == nil {
		t.Fatal("rendered an adapter the package does not declare")
	}
	bad := ok
	bad.Connection = "Ada Mailbox"
	if _, err := NewEngine().ExpandAdapter(pkg, mailParams, "mail", bad); err == nil {
		t.Fatal("rendered an adapter for a malformed connection name")
	}
	bad = ok
	bad.Node = ""
	if _, err := NewEngine().ExpandAdapter(pkg, mailParams, "mail", bad); err == nil {
		t.Fatal("rendered an adapter with no node")
	}
}
