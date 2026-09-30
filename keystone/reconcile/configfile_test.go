package reconcile

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func configFile(name, owner, filename, content string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindConfigFile,
		Metadata:   resource.Metadata{Name: name, Owner: owner, Node: "node-a"},
		ConfigFile: &resource.ConfigFileSpec{Filename: filename, Content: content},
	}
}

// TestConfigFilePathIsDerivedFromTheCapability is the property that makes
// this kind safe to hand to packages. The directory is not supplied — it
// comes from the owner — so a package cannot address another capability's
// subtree, by mistake or otherwise. It is also what makes the derived
// path line up with the per-capability read-only mount each container
// gets: a file lands where its own app can read it, and nowhere else.
func TestConfigFilePathIsDerivedFromTheCapability(t *testing.T) {
	got, err := configFilePath("/etc/wildstone",
		configFile("identity-config", "capability/identity", "configuration.yml", "x: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "/etc/wildstone/identity/configuration.yml"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

// A ConfigFile owned by "system" has no capability subtree to land in.
// Every other kind tolerates a system owner; this one has nowhere to put
// the file, so it is refused rather than defaulting somewhere.
func TestConfigFileRefusesANonCapabilityOwner(t *testing.T) {
	_, err := configFilePath("/etc/wildstone",
		configFile("orphan", "system", "configuration.yml", "x: 1\n"))
	if err == nil {
		t.Fatal("a system-owned ConfigFile was given a path")
	}
	if !strings.Contains(err.Error(), "capability/") {
		t.Errorf("error does not explain the requirement: %v", err)
	}
}

// Validation is what keeps a filename from walking out of the directory
// the derivation put it in.
func TestConfigFileFilenameMustBeOneSegment(t *testing.T) {
	for _, bad := range []string{
		"../../etc/passwd",
		"sub/dir.yml",
		"/absolute.yml",
		".hidden",
		"",
	} {
		r := configFile("cfg", "capability/identity", bad, "x: 1\n")
		if err := r.Validate(); err == nil {
			t.Errorf("filename %q was accepted", bad)
		}
	}
	// And the shapes real config files actually have are accepted.
	for _, good := range []string{"configuration.yml", "LDAP-Auth.xml", "immich.json", "config"} {
		r := configFile("cfg", "capability/identity", good, "x: 1\n")
		if err := r.Validate(); err != nil {
			t.Errorf("filename %q was refused: %v", good, err)
		}
	}
}

// Materialization is DERIVED from the content, not declared by the
// package. An author who forgot a flag would ship a config with the
// literal text "secret/authelia-oidc" where a secret belongs — an app
// that starts and rejects every login, with nothing in any log to say
// why. Deriving it means that cannot happen.
func TestMaterializationIsDerivedFromContent(t *testing.T) {
	for _, tc := range []struct {
		content string
		want    bool
	}{
		{"client_secret: 'pbkdf2:secret/immich-oidc'\n", true},
		{"password: secret/lldap-admin\n", true},
		{"z2m:mosquitto:secret/mqtt-z2m\n", true},
		{"jwks:\n  - key: |\n      secret/authelia-oidc-jwks\n", true},
		{"log_level: debug\n", false},
		{"note: this file has no references\n", false},
	} {
		if got := configFileNeedsMaterialization(tc.content); got != tc.want {
			t.Errorf("content %q: materialize = %v, want %v", tc.content, got, tc.want)
		}
	}
}

// An empty content is how a delete is expressed on the wire, so declaring
// one is refused — otherwise a package could declare a file that is
// indistinguishable from one being removed.
func TestEmptyContentIsRefused(t *testing.T) {
	r := configFile("cfg", "capability/identity", "configuration.yml", "")
	if err := r.Validate(); err == nil {
		t.Fatal("an empty ConfigFile was accepted")
	}
}
