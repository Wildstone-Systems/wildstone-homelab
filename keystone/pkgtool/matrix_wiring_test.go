package pkgtool

// Package-specific checks for the Matrix homeserver. Catalog-wide lint can
// prove that every named resource exists, but it cannot prove that the two
// sides of a credential, config path, or private-federation boundary agree.

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

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
		`config["database"]["args"]["host"] = "/run/postgresql"`,
		`config["database"]["args"].pop("password", None)`,
		`yaml.safe_dump(config, destination, sort_keys=False)`,
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
	for _, claim := range []string{"preferred_username", "name", "email"} {
		if !slices.Contains(oidc.IdTokenClaims, claim) {
			t.Errorf("Synapse maps OIDC claim %q but Authelia does not put it in the ID token: %v", claim, oidc.IdTokenClaims)
		}
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

func TestMatrixPostgresSocketAndBackupAreComplete(t *testing.T) {
	postgres := matrixResource(t, resource.KindContainer, "matrix-postgres").Container
	generate := matrixResource(t, resource.KindJob, "matrix-config-generate").Job.Run
	synapse := matrixResource(t, resource.KindContainer, "matrix-synapse").Container
	if _, ok := postgres.Env["POSTGRES_PASSWORD"]; ok {
		t.Error("Postgres declares a rotatable password even though its initialized role would retain the old value")
	}
	if _, ok := synapse.Env["POSTGRES_PASSWORD"]; ok {
		t.Error("Synapse declares a database password instead of using the private Unix socket")
	}
	if generate.Env["POSTGRES_PASSWORD"] != "secret/matrix-postgres-config-placeholder" {
		t.Errorf("config Job must use only its disposable placeholder, got %q", generate.Env["POSTGRES_PASSWORD"])
	}
	if generate.Env["POSTGRES_HOST"] != "/run/postgresql" {
		t.Errorf("config generator writes database host %q, want the private socket", generate.Env["POSTGRES_HOST"])
	}
	if postgres.Env["POSTGRES_HOST_AUTH_METHOD"] != "trust" || !slices.Contains(postgres.Command, "listen_addresses=") {
		t.Errorf("Postgres is not socket-only: auth=%q command=%v", postgres.Env["POSTGRES_HOST_AUTH_METHOD"], postgres.Command)
	}
	if !mountsVolume(postgres, "matrix-postgres-socket", "/run/postgresql") ||
		!mountsVolume(synapse, "matrix-postgres-socket", "/run/postgresql") {
		t.Error("Postgres and Synapse do not share the capability-owned socket volume")
	}
	if !mountsVolume(postgres, "matrix-pgdata", "/var/lib/postgresql") {
		t.Error("PostgreSQL 18 live data is not persisted from /var/lib/postgresql")
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

// Element Web (packages/matrix-element) is a separate package from the
// homeserver, so nothing structural stops the two from drifting apart: the
// client could name a homeserver Synapse is not served at, and Synapse could
// fail to recognise the client's origin after SSO. These checks hold the two
// packages to each other.

func elementResource(t *testing.T, kind resource.Kind, name string) resource.Resource {
	t.Helper()
	rendered, ok := renderShippedPackages(t)["matrix-element"]
	if !ok {
		t.Fatal("matrix-element is not in the catalog")
	}
	for _, r := range rendered {
		if r.Kind == kind && r.Metadata.Name == name {
			return r
		}
	}
	t.Fatalf("matrix-element renders no %s/%s", kind, name)
	return resource.Resource{}
}

// elementConfig is the subset of Element's config.json these checks read.
type elementConfig struct {
	DefaultServerConfig struct {
		Homeserver struct {
			BaseURL    string `json:"base_url"`
			ServerName string `json:"server_name"`
		} `json:"m.homeserver"`
	} `json:"default_server_config"`
	DisableCustomURLs  bool            `json:"disable_custom_urls"`
	DisableGuests      bool            `json:"disable_guests"`
	SSORedirectOptions map[string]bool `json:"sso_redirect_options"`
	PermalinkPrefix    string          `json:"permalink_prefix"`
	ElementCall        struct {
		Disable bool `json:"disable"`
	} `json:"element_call"`
	SettingDefaults map[string]any `json:"setting_defaults"`
	// Every top-level key as written. Decoding into a struct cannot tell an
	// explicit null from an absent key, and for the integration manager that
	// is the whole difference: absent falls back to Element's hosted one.
	raw map[string]json.RawMessage
}

func renderedElementConfig(t *testing.T) elementConfig {
	t.Helper()
	var cfg elementConfig
	content := elementResource(t, resource.KindConfigFile, "element-config").ConfigFile.Content
	if err := json.Unmarshal([]byte(content), &cfg); err != nil {
		t.Fatalf("Element's config.json is not valid JSON, so Element would start with its defaults: %v\n%s", err, content)
	}
	if err := json.Unmarshal([]byte(content), &cfg.raw); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// synapseConfig is the subset of matrix-synapse's private.yaml these checks read.
type synapseConfig struct {
	ServerName    string `yaml:"server_name"`
	PublicBaseURL string `yaml:"public_baseurl"`
	SSO           struct {
		ClientWhitelist []string `yaml:"client_whitelist"`
	} `yaml:"sso"`
}

func renderedSynapseConfig(t *testing.T) synapseConfig {
	t.Helper()
	var cfg synapseConfig
	content := matrixResource(t, resource.KindConfigFile, "matrix-private").ConfigFile.Content
	if err := yaml.Unmarshal([]byte(content), &cfg); err != nil {
		t.Fatalf("Synapse's private.yaml does not parse: %v", err)
	}
	return cfg
}

func TestElementIsLockedToTheHouseholdHomeserver(t *testing.T) {
	element := renderedElementConfig(t)
	synapse := renderedSynapseConfig(t)

	hs := element.DefaultServerConfig.Homeserver
	if hs.BaseURL+"/" != synapse.PublicBaseURL {
		t.Errorf("Element connects to %q but Synapse is served at %q", hs.BaseURL, synapse.PublicBaseURL)
	}
	if hs.ServerName != synapse.ServerName {
		t.Errorf("Element expects server_name %q but Synapse's is %q", hs.ServerName, synapse.ServerName)
	}
	if !element.DisableCustomURLs {
		t.Error("Element lets a person point it at another homeserver")
	}
	if !element.DisableGuests {
		t.Error("Element offers guest access that Synapse refuses")
	}
	// Synapse has password login off, so the login form could never work.
	if !element.SSORedirectOptions["immediate"] {
		t.Error("Element shows a login form instead of going straight to household SSO")
	}
}

func TestElementSendsNothingToThirdPartiesByDefault(t *testing.T) {
	element := renderedElementConfig(t)
	for _, key := range []string{"integrations_ui_url", "integrations_rest_url", "integrations_widgets_urls"} {
		if v, ok := element.raw[key]; !ok || string(v) != "null" {
			t.Errorf("%s must be an explicit null, or Element falls back to its hosted integration manager", key)
		}
	}
	if !element.ElementCall.Disable {
		t.Error("Element Call is enabled, and its default is Element's hosted call service")
	}
	for _, feature := range []string{"UIFeature.voip", "UIFeature.widgets", "UIFeature.identityServer"} {
		if element.SettingDefaults[feature] != false {
			t.Errorf("setting_defaults[%q] is %v, want false", feature, element.SettingDefaults[feature])
		}
	}
}

func TestSynapseTrustsElementAfterSSO(t *testing.T) {
	element := renderedElementConfig(t)
	synapse := renderedSynapseConfig(t)
	// permalink_prefix is Element's own origin, rendered by the same origin
	// function from the same pinned label as its Route.
	want := element.PermalinkPrefix + "/"
	if !slices.Contains(synapse.SSO.ClientWhitelist, want) {
		t.Errorf("Synapse's sso.client_whitelist %v does not contain Element's origin %q; every sign-in would stop at a confirmation page",
			synapse.SSO.ClientWhitelist, want)
	}
}

func TestElementRouteIsPrivateAndBrowserOnly(t *testing.T) {
	route := elementResource(t, resource.KindRoute, "element").Route
	if route.Auth != "authelia_oidc" || route.Classification != "family_private" {
		t.Errorf("Element route is auth=%q classification=%q; want authelia_oidc on family_private", route.Auth, route.Classification)
	}
	if route.Host != "element.example.internal" {
		t.Errorf("Element route host is %q; it must retain the pinned element label", route.Host)
	}
	web := elementResource(t, resource.KindContainer, "element-web").Container
	if port := web.Env["ELEMENT_WEB_PORT"]; route.Upstream != "container/element-web:"+port {
		t.Errorf("Element route points at %q but nginx listens on ELEMENT_WEB_PORT=%q", route.Upstream, port)
	}
}
