package pkgtool

// Package-specific checks for the Matrix homeserver. Catalog-wide lint can
// prove that every named resource exists, but it cannot prove that the two
// sides of a credential, config path, or private-federation boundary agree.

import (
	"slices"
	"strings"
	"testing"

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
}

func TestMatrixOidcSecretTravelsByFile(t *testing.T) {
	config := matrixResource(t, resource.KindConfigFile, "matrix-private").ConfigFile.Content
	if strings.Contains(config, "secret/") {
		t.Fatal("Matrix's uid-991 config contains a secret reference and would be materialized 0600 root-only")
	}
	if !strings.Contains(config, "client_secret_path: /run/matrix/oidc-client-secret") {
		t.Fatal("Matrix config does not read the OIDC secret file")
	}
	if !strings.Contains(config, `localpart_template: '{{ user.preferred_username }}'`) ||
		!strings.Contains(config, `display_name_template: '{{ user.name }}'`) {
		t.Fatal("Synapse's Jinja OIDC mappings did not survive the outer package template")
	}

	job := matrixResource(t, resource.KindJob, "matrix-oidc-secret-file").Job.Run
	if job.Env["OIDC_CLIENT_SECRET"] == "" || !strings.HasPrefix(job.Env["OIDC_CLIENT_SECRET"], "secret/") {
		t.Fatalf("OIDC credential job does not receive a secret reference: %q", job.Env["OIDC_CLIENT_SECRET"])
	}
	if !mountsVolume(&job, "matrix-credentials", "/run/matrix") {
		t.Fatal("OIDC credential job does not write the matrix-credentials volume")
	}

	synapse := matrixResource(t, resource.KindContainer, "matrix-synapse").Container
	if !mountsVolume(synapse, "matrix-credentials", "/run/matrix") {
		t.Fatal("Synapse does not read the credential volume populated by the Job")
	}
	oidc := matrixResource(t, resource.KindOidcClient, "matrix-synapse").OidcClient
	if oidc.SecretRef != job.Env["OIDC_CLIENT_SECRET"] {
		t.Errorf("Authelia and the credential Job use different OIDC secrets: %q vs %q", oidc.SecretRef, job.Env["OIDC_CLIENT_SECRET"])
	}
	if len(oidc.RedirectUris) != 1 || !strings.HasSuffix(oidc.RedirectUris[0], "/_synapse/client/oidc/callback") {
		t.Errorf("unexpected Synapse OIDC callback: %v", oidc.RedirectUris)
	}
	oidcResource := matrixResource(t, resource.KindOidcClient, "matrix-synapse")
	jobResource := matrixResource(t, resource.KindJob, "matrix-oidc-secret-file")
	if oidcResource.Metadata.Node != jobResource.Metadata.Node {
		t.Errorf("OIDC secret is synthesized on node %q but its credential Job runs on %q",
			oidcResource.Metadata.Node, jobResource.Metadata.Node)
	}
}

func TestMatrixGenerationAndRuntimeUseTheSameData(t *testing.T) {
	generate := matrixResource(t, resource.KindJob, "matrix-synapse-generate").Job.Run
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
}

func TestMatrixPostgresCredentialAndBackupAreComplete(t *testing.T) {
	postgres := matrixResource(t, resource.KindContainer, "matrix-postgres").Container
	generate := matrixResource(t, resource.KindJob, "matrix-synapse-generate").Job.Run
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
	if slices.Contains(backup.Volumes, "matrix-credentials") {
		t.Error("Matrix backup includes the re-materializable OIDC credential volume")
	}
	if backup.Schedule != "daily" {
		t.Errorf("Matrix backup schedule is %q, want daily", backup.Schedule)
	}
}
