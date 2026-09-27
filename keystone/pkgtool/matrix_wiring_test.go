package pkgtool

// Package-specific checks for the Matrix homeserver. Catalog-wide lint can
// prove that every named resource exists, but it cannot prove that the two
// sides of a credential, config path, or private-federation boundary agree.

import (
	"slices"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func matrixPackage(t *testing.T) []resource.Resource {
	t.Helper()
	rendered, ok := renderShippedPackages(t)["matrix-synapse"]
	if !ok {
		t.Fatal("matrix-synapse is not in the catalog")
	}
	return rendered
}

func matrixResource(t *testing.T, kind resource.Kind, name string) resource.Resource {
	t.Helper()
	for _, r := range matrixPackage(t) {
		if r.Kind == kind && r.Metadata.Name == name {
			return r
		}
	}
	t.Fatalf("matrix-synapse renders no %s/%s", kind, name)
	return resource.Resource{}
}

func TestMatrixIsPrivateAndNonFederatedByDefault(t *testing.T) {
	config := matrixResource(t, resource.KindConfigFile, "matrix-private").ConfigFile.Content
	for _, want := range []string{
		"names: [client]",
		"federation_domain_whitelist: []",
		"allow_profile_lookup_over_federation: false",
		"allow_public_rooms_over_federation: false",
		"enable_registration: false",
		"allow_guest_access: false",
		"password_config:\n  enabled: false",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("private Matrix config is missing %q", want)
		}
	}
	if strings.Contains(config, "names: [client, federation]") || strings.Contains(config, "names: [federation]") {
		t.Error("private Matrix listener exposes the federation resource")
	}

	route := matrixResource(t, resource.KindRoute, "matrix").Route
	if route.Auth != "app_oidc" || route.Classification != "family_private" {
		t.Errorf("Matrix route is auth=%q classification=%q; want app_oidc on family_private", route.Auth, route.Classification)
	}
	if route.Upstream != "container/matrix-synapse:8008" {
		t.Errorf("Matrix route points at %q, want the Synapse client listener", route.Upstream)
	}
	if route.Host != "matrix.example.internal" {
		t.Errorf("Matrix route host is %q; it must retain the pinned matrix label", route.Host)
	}
}

func TestMatrixOidcSecretTravelsByFile(t *testing.T) {
	config := matrixResource(t, resource.KindConfigFile, "matrix-private").ConfigFile.Content
	if strings.Contains(config, "secret/") {
		t.Fatal("Matrix's uid-991 config contains a secret reference and would be materialized 0600 root-only")
	}
	if !strings.Contains(config, "client_secret_path: /tmp/matrix-oidc-client-secret") {
		t.Fatal("Matrix config does not read the OIDC secret file")
	}
	if !strings.Contains(config, `localpart_template: '{{ user.preferred_username }}'`) ||
		!strings.Contains(config, `display_name_template: '{{ user.name }}'`) {
		t.Fatal("Synapse's Jinja OIDC mappings did not survive the outer package template")
	}

	synapse := matrixResource(t, resource.KindContainer, "matrix-synapse").Container
	secretRef := synapse.Env["MATRIX_OIDC_CLIENT_SECRET"]
	if secretRef == "" || !strings.HasPrefix(secretRef, "secret/") {
		t.Fatalf("Synapse does not directly receive its OIDC secret reference: %q", secretRef)
	}
	if synapse.Env["PYTHONPATH"] != "/etc/wildstone/matrix-homeserver" {
		t.Fatalf("Synapse cannot import its managed startup module: PYTHONPATH=%q", synapse.Env["PYTHONPATH"])
	}
	if !slices.Contains(synapse.Command, "matrix_oidc_start") {
		t.Fatalf("Synapse bypasses the OIDC startup module: %v", synapse.Command)
	}
	startup := matrixResource(t, resource.KindConfigFile, "matrix-oidc-start").ConfigFile
	for _, want := range []string{
		`os.environ.pop("MATRIX_OIDC_CLIENT_SECRET", "")`,
		`Path("/tmp/matrix-oidc-client-secret")`,
		`os.execv(`,
		`"synapse.app.homeserver"`,
	} {
		if !strings.Contains(startup.Content, want) {
			t.Errorf("Matrix startup module is missing %q", want)
		}
	}
	oidc := matrixResource(t, resource.KindOidcClient, "matrix-synapse").OidcClient
	if oidc.SecretRef != secretRef {
		t.Errorf("Authelia and Synapse use different OIDC secrets: %q vs %q", oidc.SecretRef, secretRef)
	}
	if len(oidc.RedirectUris) != 1 || !strings.HasSuffix(oidc.RedirectUris[0], "/_synapse/client/oidc/callback") {
		t.Errorf("unexpected Synapse OIDC callback: %v", oidc.RedirectUris)
	}
	oidcResource := matrixResource(t, resource.KindOidcClient, "matrix-synapse")
	synapseResource := matrixResource(t, resource.KindContainer, "matrix-synapse")
	if oidcResource.Metadata.Node != synapseResource.Metadata.Node {
		t.Errorf("OIDC secret is synthesized on node %q but Synapse runs on %q",
			oidcResource.Metadata.Node, synapseResource.Metadata.Node)
	}
}

func TestMatrixGenerationAndRuntimeUseTheSameData(t *testing.T) {
	generate := matrixResource(t, resource.KindJob, "matrix-config-generate").Job.Run
	synapse := matrixResource(t, resource.KindContainer, "matrix-synapse").Container
	if generate.Image != synapse.Image {
		t.Errorf("config generator image %q differs from runtime %q", generate.Image, synapse.Image)
	}
	if !slices.Equal(generate.Command, []string{"migrate_config"}) {
		t.Errorf("generator command is %v, want official migrate_config mode", generate.Command)
	}
	if !mountsVolume(&generate, "matrix-data", "/data") || !mountsVolume(synapse, "matrix-data", "/data") {
		t.Fatal("generator and Synapse do not share matrix-data at /data")
	}
	if !slices.Contains(synapse.Command, "--config-path=/data/homeserver.yaml") ||
		!slices.Contains(synapse.Command, "--config-path=/etc/wildstone/matrix-homeserver/private.yaml") {
		t.Errorf("Synapse does not load generated base then managed override: %v", synapse.Command)
	}

	// Job and Container intentionally share a planner tier because most Jobs
	// provision an already-running service. Matrix's generator is the opposite:
	// the runtime cannot start until this Job has created homeserver.yaml. Pin
	// the real plan order so a rename cannot recreate the first-install loop.
	generatedAt, runtimeAt := -1, -1
	for i, step := range plan.Diff(matrixPackage(t), nil).Steps {
		switch step.Name {
		case "matrix-config-generate":
			generatedAt = i
		case "matrix-synapse":
			runtimeAt = i
		}
	}
	if generatedAt < 0 || runtimeAt < 0 || generatedAt >= runtimeAt {
		t.Errorf("fresh-install plan puts config generation at %d and Synapse runtime at %d; generation must be first", generatedAt, runtimeAt)
	}
}

func TestMatrixPostgresCredentialAndBackupAreComplete(t *testing.T) {
	postgres := matrixResource(t, resource.KindContainer, "matrix-postgres").Container
	generate := matrixResource(t, resource.KindJob, "matrix-config-generate").Job.Run
	if postgres.Env["POSTGRES_PASSWORD"] != generate.Env["POSTGRES_PASSWORD"] {
		t.Errorf("Postgres and Synapse generator use different password references: %q vs %q",
			postgres.Env["POSTGRES_PASSWORD"], generate.Env["POSTGRES_PASSWORD"])
	}
	if generate.Env["POSTGRES_HOST"] != "matrix-postgres" {
		t.Errorf("Synapse generator points at database host %q", generate.Env["POSTGRES_HOST"])
	}

	backup := matrixResource(t, resource.KindBackupPlan, "matrix").BackupPlan
	for _, volume := range []string{"matrix-data", "matrix-pgdata"} {
		if !slices.Contains(backup.Volumes, volume) {
			t.Errorf("Matrix backup omits durable volume %q", volume)
		}
	}
	if backup.Schedule != "daily" {
		t.Errorf("Matrix backup schedule is %q, want daily", backup.Schedule)
	}
}
