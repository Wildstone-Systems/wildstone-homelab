package resource

import (
	"strings"
	"testing"
)

func validDigest() string {
	return "ghcr.io/example/app@sha256:" + strings.Repeat("a", 64)
}

func TestYAMLRoundTrip(t *testing.T) {
	cases := []Resource{
		{
			ApiVersion: ApiVersion,
			Kind:       KindContainer,
			Metadata:   Metadata{Name: "jellyfin", Owner: "capability/media", Node: "node-a"},
			Container: &ContainerSpec{
				Image:   validDigest(),
				Env:     map[string]string{"TZ": "UTC"},
				Ports:   []string{"8096:8096"},
				Mounts:  []Mount{{Source: "/data/jellyfin", Target: "/config", ReadOnly: false}},
				Restart: "unless-stopped",
			},
		},
		{
			ApiVersion: ApiVersion,
			Kind:       KindVolume,
			Metadata:   Metadata{Name: "jellyfin-config", Owner: "capability/media", Node: "node-a"},
			Volume:     &VolumeSpec{Name: "jellyfin-config"},
		},
		{
			ApiVersion: ApiVersion,
			Kind:       KindRoute,
			Metadata:   Metadata{Name: "jellyfin-route", Owner: "capability/media"},
			Route: &RouteSpec{
				Host:           "jellyfin.wildstone.dev",
				Upstream:       "container/jellyfin:8096",
				Auth:           "authelia_oidc",
				Classification: "family_public",
			},
		},
		{
			ApiVersion: ApiVersion,
			Kind:       KindDnsRecord,
			Metadata:   Metadata{Name: "jellyfin-dns", Owner: "capability/media"},
			DnsRecord: &DnsRecordSpec{
				Zone:    "wildstone.dev",
				Name:    "jellyfin",
				Type:    "CNAME",
				Content: "tunnel.example.com",
				Proxied: true,
			},
		},
		{
			ApiVersion: ApiVersion,
			Kind:       KindOidcClient,
			Metadata:   Metadata{Name: "immich", Owner: "capability/identity"},
			OidcClient: &OidcClientSpec{
				ClientId:     "immich",
				ClientName:   "Immich",
				RedirectUris: []string{"https://photos.example.com/auth/login", "app.immich:///oauth-callback"},
				Scopes:       []string{"openid", "profile", "email", "groups"},
				AuthPolicy:   "two_factor",
				SecretRef:    "secret/immich-oidc",
			},
		},
		{
			ApiVersion: ApiVersion,
			Kind:       KindLdapGroup,
			Metadata:   Metadata{Name: "photos-users", Owner: "capability/identity"},
			LdapGroup: &LdapGroupSpec{
				Name:    "photos-users",
				Members: []string{"alice", "bob"},
			},
		},
	}

	for _, want := range cases {
		t.Run(string(want.Kind), func(t *testing.T) {
			data, err := Marshal(want)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got, err := Unmarshal(data)
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.ApiVersion != want.ApiVersion || got.Kind != want.Kind || got.Metadata != want.Metadata {
				t.Fatalf("envelope mismatch: got %+v want %+v", got, want)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("round-tripped resource failed validation: %v", err)
			}
		})
	}
}

func TestYAMLRoundTripContainerSpecFields(t *testing.T) {
	want := Resource{
		ApiVersion: ApiVersion,
		Kind:       KindContainer,
		Metadata:   Metadata{Name: "jellyfin", Owner: "capability/media", Node: "node-a"},
		Container: &ContainerSpec{
			Image:   validDigest(),
			Env:     map[string]string{"TZ": "UTC"},
			Ports:   []string{"8096:8096"},
			Mounts:  []Mount{{Source: "/data/jellyfin", Target: "/config"}},
			Restart: "unless-stopped",
		},
	}
	data, err := Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Container == nil {
		t.Fatal("Container spec nil after round trip")
	}
	if got.Container.Image != want.Container.Image {
		t.Errorf("Image: got %q want %q", got.Container.Image, want.Container.Image)
	}
	if got.Container.Env["TZ"] != "UTC" {
		t.Errorf("Env: got %+v", got.Container.Env)
	}
	if len(got.Container.Ports) != 1 || got.Container.Ports[0] != "8096:8096" {
		t.Errorf("Ports: got %+v", got.Container.Ports)
	}
	if len(got.Container.Mounts) != 1 || got.Container.Mounts[0].Source != "/data/jellyfin" {
		t.Errorf("Mounts: got %+v", got.Container.Mounts)
	}
	if got.Container.Restart != "unless-stopped" {
		t.Errorf("Restart: got %q", got.Container.Restart)
	}
}

func TestStatusNeverSerialized(t *testing.T) {
	r := Resource{
		ApiVersion: ApiVersion,
		Kind:       KindVolume,
		Metadata:   Metadata{Name: "vol", Owner: "system"},
		Volume:     &VolumeSpec{Name: "vol"},
	}
	data, err := Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "status") {
		t.Fatalf("marshaled output contains status: %s", data)
	}
}

func TestValidateRouteRequiresClassification(t *testing.T) {
	r := Resource{
		Kind:     KindRoute,
		Metadata: Metadata{Name: "r1", Owner: "system"},
		Route: &RouteSpec{
			Host:     "example.wildstone.dev",
			Upstream: "container/x:80",
			Auth:     "none",
		},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing classification")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if ve.Field != "spec.classification" {
		t.Errorf("expected field spec.classification, got %q", ve.Field)
	}
}

func TestValidateContainerRejectsTag(t *testing.T) {
	r := Resource{
		Kind:     KindContainer,
		Metadata: Metadata{Name: "c1", Owner: "system"},
		Container: &ContainerSpec{
			Image: "ghcr.io/example/app:latest",
		},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for tag-pinned image")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "spec.image" {
		t.Fatalf("expected spec.image validation error, got %v", err)
	}
}

func TestValidateContainerAcceptsDigest(t *testing.T) {
	r := Resource{
		Kind:     KindContainer,
		Metadata: Metadata{Name: "c1", Owner: "system"},
		Container: &ContainerSpec{
			Image: validDigest(),
		},
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateDnsRecordBadType(t *testing.T) {
	r := Resource{
		Kind:     KindDnsRecord,
		Metadata: Metadata{Name: "d1", Owner: "system"},
		DnsRecord: &DnsRecordSpec{
			Zone: "wildstone.dev",
			Name: "x",
			Type: "MX",
		},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for bad dns record type")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "spec.type" {
		t.Fatalf("expected spec.type validation error, got %v", err)
	}
}

func TestValidateMissingOwner(t *testing.T) {
	r := Resource{
		Kind:     KindVolume,
		Metadata: Metadata{Name: "v1"},
		Volume:   &VolumeSpec{Name: "v1"},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing owner")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "metadata.owner" {
		t.Fatalf("expected metadata.owner validation error, got %v", err)
	}
}

func TestValidateRejectsPathTraversalNames(t *testing.T) {
	// A resource name is used verbatim as a filesystem path segment by the
	// Statefile and fragment writers; a hostile name must be refused at
	// validation, before it can escape the intended directory.
	for _, name := range []string{
		"../../../etc/cron.d/x", // dot-dot traversal
		"/etc/passwd",           // absolute
		"foo/bar",               // embedded slash
		"UPPER",                 // not a DNS label (uppercase)
		"has space",             // whitespace
		"bad\x00name",           // NUL
		"-leading-dash",         // must start alphanumeric
	} {
		r := Resource{
			Kind:     KindVolume,
			Metadata: Metadata{Name: name, Owner: "capability/x"},
			Volume:   &VolumeSpec{Name: "v1"},
		}
		err := r.Validate()
		if err == nil {
			t.Fatalf("name %q: expected validation error, got nil", name)
		}
		ve, ok := err.(*ValidationError)
		if !ok || ve.Field != "metadata.name" {
			t.Fatalf("name %q: expected metadata.name validation error, got %v", name, err)
		}
	}

	// A well-formed name still validates.
	ok := Resource{
		Kind:     KindVolume,
		Metadata: Metadata{Name: "immich-library.v2", Owner: "capability/x"},
		Volume:   &VolumeSpec{Name: "v1"},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid name rejected: %v", err)
	}
}

func TestValidateOidcClientMissingRedirectUri(t *testing.T) {
	r := Resource{
		Kind:     KindOidcClient,
		Metadata: Metadata{Name: "immich", Owner: "capability/identity"},
		OidcClient: &OidcClientSpec{
			ClientId:   "immich",
			AuthPolicy: "two_factor",
			SecretRef:  "secret/immich-oidc",
		},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing redirectUris")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "spec.redirectUris" {
		t.Fatalf("expected spec.redirectUris validation error, got %v", err)
	}
}

func TestValidateOidcClientRejectsLiteralSecret(t *testing.T) {
	r := Resource{
		Kind:     KindOidcClient,
		Metadata: Metadata{Name: "immich", Owner: "capability/identity"},
		OidcClient: &OidcClientSpec{
			ClientId:     "immich",
			RedirectUris: []string{"https://photos.example.com/auth/login"},
			AuthPolicy:   "two_factor",
			SecretRef:    "hunter2plaintextsecret", // literal, not a "secret/<name>" reference
		},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for literal secretRef")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "spec.secretRef" {
		t.Fatalf("expected spec.secretRef validation error, got %v", err)
	}
}

func TestValidateOidcClientMissingClientId(t *testing.T) {
	r := Resource{
		Kind:     KindOidcClient,
		Metadata: Metadata{Name: "immich", Owner: "capability/identity"},
		OidcClient: &OidcClientSpec{
			RedirectUris: []string{"https://photos.example.com/auth/login"},
			AuthPolicy:   "two_factor",
			SecretRef:    "secret/immich-oidc",
		},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing clientId")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "spec.clientId" {
		t.Fatalf("expected spec.clientId validation error, got %v", err)
	}
}

func TestValidateLdapGroupMissingName(t *testing.T) {
	r := Resource{
		Kind:      KindLdapGroup,
		Metadata:  Metadata{Name: "photos-users", Owner: "capability/identity"},
		LdapGroup: &LdapGroupSpec{Members: []string{"alice"}},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing ldap group name")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "spec.name" {
		t.Fatalf("expected spec.name validation error, got %v", err)
	}
}

func TestValidateApiVersionMismatch(t *testing.T) {
	r := Resource{
		ApiVersion: "wildstone.dev/v2",
		Kind:       KindVolume,
		Metadata:   Metadata{Name: "v1", Owner: "system"},
		Volume:     &VolumeSpec{Name: "v1"},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for mismatched apiVersion")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "apiVersion" {
		t.Fatalf("expected apiVersion validation error, got %v", err)
	}
}

func TestValidateApiVersionEmptyAllowed(t *testing.T) {
	r := Resource{
		Kind:     KindVolume,
		Metadata: Metadata{Name: "v1", Owner: "system"},
		Volume:   &VolumeSpec{Name: "v1"},
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("unexpected error for empty apiVersion: %v", err)
	}
}

func TestValidateSpecExclusivity(t *testing.T) {
	r := Resource{
		Kind:      KindContainer,
		Metadata:  Metadata{Name: "c1", Owner: "system"},
		Container: &ContainerSpec{Image: validDigest()},
		Volume:    &VolumeSpec{Name: "c1"},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for extra populated spec")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "spec" {
		t.Fatalf("expected spec validation error, got %v", err)
	}
}

func TestValidateContainerBadPort(t *testing.T) {
	r := Resource{
		Kind:     KindContainer,
		Metadata: Metadata{Name: "c1", Owner: "system"},
		Container: &ContainerSpec{
			Image: validDigest(),
			Ports: []string{"not-a-port"},
		},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for malformed port mapping")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "spec.ports" {
		t.Fatalf("expected spec.ports validation error, got %v", err)
	}
}

func TestValidateContainerMountRequiresSourceAndTarget(t *testing.T) {
	r := Resource{
		Kind:     KindContainer,
		Metadata: Metadata{Name: "c1", Owner: "system"},
		Container: &ContainerSpec{
			Image:  validDigest(),
			Mounts: []Mount{{Source: "", Target: "/data"}},
		},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for empty mount source")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "spec.mounts[0].source" {
		t.Fatalf("expected spec.mounts[0].source validation error, got %v", err)
	}
}

func TestValidateRouteBadAuth(t *testing.T) {
	r := Resource{
		Kind:     KindRoute,
		Metadata: Metadata{Name: "r1", Owner: "system"},
		Route: &RouteSpec{
			Host:           "example.wildstone.dev",
			Upstream:       "container/x:80",
			Auth:           "basic",
			Classification: "family_public",
		},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for bad auth value")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "spec.auth" {
		t.Fatalf("expected spec.auth validation error, got %v", err)
	}
}

func TestValidateOidcClientBadAuthPolicy(t *testing.T) {
	r := Resource{
		Kind:     KindOidcClient,
		Metadata: Metadata{Name: "immich", Owner: "capability/identity"},
		OidcClient: &OidcClientSpec{
			ClientId:     "immich",
			RedirectUris: []string{"https://photos.example.com/auth/login"},
			AuthPolicy:   "no_factor",
			SecretRef:    "secret/immich-oidc",
		},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for bad authPolicy")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "spec.authPolicy" {
		t.Fatalf("expected spec.authPolicy validation error, got %v", err)
	}
}

func TestValidateDnsRecordEmptyContent(t *testing.T) {
	r := Resource{
		Kind:     KindDnsRecord,
		Metadata: Metadata{Name: "d1", Owner: "system"},
		DnsRecord: &DnsRecordSpec{
			Zone: "wildstone.dev",
			Name: "x",
			Type: "CNAME",
		},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for empty dns record content")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "spec.content" {
		t.Fatalf("expected spec.content validation error, got %v", err)
	}
}

func TestValidateMissingName(t *testing.T) {
	r := Resource{
		Kind:     KindVolume,
		Metadata: Metadata{Owner: "system"},
		Volume:   &VolumeSpec{Name: "v1"},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing name")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Field != "metadata.name" {
		t.Fatalf("expected metadata.name validation error, got %v", err)
	}
}

func TestBackupPlanValidateAndRoundTrip(t *testing.T) {
	good := Resource{
		ApiVersion: ApiVersion,
		Kind:       KindBackupPlan,
		Metadata:   Metadata{Name: "photos", Owner: "capability/photos", Node: "n1"},
		BackupPlan: &BackupPlanSpec{
			Volumes:        []string{"immich-library"},
			Repository:     "backup-repo",
			PasswordSecret: "secret/restic-photos",
			Schedule:       "daily",
			Retention:      BackupRetention{KeepDaily: 7},
		},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid BackupPlan rejected: %v", err)
	}
	data, err := Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("round trip decode: %v", err)
	}
	if back.BackupPlan == nil || back.BackupPlan.Retention.KeepDaily != 7 ||
		back.BackupPlan.PasswordSecret != "secret/restic-photos" {
		t.Fatalf("round trip lost fields: %+v", back.BackupPlan)
	}

	bad := good
	spec := *good.BackupPlan
	bad.BackupPlan = &spec
	// A literal secret value where the reference belongs is refused at
	// validation, before it can ever reach the Statefile.
	spec.PasswordSecret = "hunter2-literal-value"
	if err := bad.Validate(); err == nil {
		t.Fatal("literal password accepted; must require a secret/<name> reference")
	}
	spec.PasswordSecret = "secret/restic-photos"
	spec.Schedule = "hourly"
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown schedule accepted; the set is closed")
	}
	spec.Schedule = "daily"
	spec.Volumes = nil
	if err := bad.Validate(); err == nil {
		t.Fatal("empty volume list accepted")
	}
}
