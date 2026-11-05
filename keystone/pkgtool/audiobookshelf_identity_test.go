package pkgtool

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

func renderAudiobookshelf(t *testing.T, identity map[string]string) []resource.Resource {
	t.Helper()
	shipped := walkShipped(t)
	entry, ok := shipped["audiobooks-audiobookshelf"]
	if !ok {
		t.Fatal("audiobooks-audiobookshelf is not in the shipped catalog")
	}
	all := make([]*pkgspec.Package, 0, len(shipped))
	for _, candidate := range shipped {
		all = append(all, candidate.pkg)
	}
	params := map[string]string{
		"public_domain":        "example.internal",
		"audiobooks_subdomain": "audiobooks",
		"node":                 "n1",
		"audiobooks_path":      "/srv/wildstone/media/audiobooks",
		"timezone":             "UTC",
	}
	for name, value := range identity {
		params[name] = value
	}
	got, err := capability.NewEngine().Expand(entry.pkg, params, capability.NewCatalog(all...).Rehearsal())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func audiobookshelfContainer(t *testing.T, rendered []resource.Resource) *resource.ContainerSpec {
	t.Helper()
	for i := range rendered {
		if rendered[i].Kind == resource.KindContainer && rendered[i].Metadata.Name == "audiobookshelf" {
			return rendered[i].Container
		}
	}
	t.Fatal("rendered package has no Container/audiobookshelf")
	return nil
}

func TestAudiobookshelfOmittedIdentityPreservesContainerSpec(t *testing.T) {
	c := audiobookshelfContainer(t, renderAudiobookshelf(t, nil))
	if c.RuntimeIdentity != nil || c.PreflightMountAccess {
		t.Fatalf("omitted identity rendered runtimeIdentity=%v preflightMountAccess=%v; existing installs must keep image-default behavior", c.RuntimeIdentity, c.PreflightMountAccess)
	}

	// Pin the old 1.0.3 container bytes. The package version moves, but an
	// install that leaves the migration fields empty must not replace the
	// container merely because an omitted field acquired a zero value.
	got, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"Image":"ghcr.io/advplyr/audiobookshelf@sha256:180acad33d69c99ed208676465d8edcb268fa46967735579a7810859885b1a8e","Env":{"TZ":"UTC"},"Ports":null,"Mounts":[{"Source":"audiobookshelf-config","Target":"/config","ReadOnly":false},{"Source":"audiobookshelf-metadata","Target":"/metadata","ReadOnly":false},{"Source":"audiobookshelf-podcasts","Target":"/podcasts","ReadOnly":false},{"Source":"/srv/wildstone/media/audiobooks","Target":"/audiobooks","ReadOnly":false}],"Command":null,"Restart":"unless-stopped"}`
	if string(got) != want {
		t.Fatalf("omitted migration changed the pinned Container spec\n got: %s\nwant: %s", got, want)
	}
}

func TestAudiobookshelfConfiguredIdentityRequiresPreflight(t *testing.T) {
	uidOnly := audiobookshelfContainer(t, renderAudiobookshelf(t, map[string]string{"runtime_uid": "1000"}))
	if uidOnly.RuntimeIdentity == nil || uidOnly.RuntimeIdentity.UID == nil || *uidOnly.RuntimeIdentity.UID != 1000 {
		t.Fatalf("runtime identity = %#v, want uid 1000", uidOnly.RuntimeIdentity)
	}
	if uidOnly.RuntimeIdentity.GID == nil || *uidOnly.RuntimeIdentity.GID != 1000 {
		t.Fatalf("uid-only gid = %#v, want the explicit uid fallback 1000", uidOnly.RuntimeIdentity.GID)
	}
	if !uidOnly.PreflightMountAccess {
		t.Fatal("configured identity did not require the effective-identity mount preflight")
	}

	distinct := audiobookshelfContainer(t, renderAudiobookshelf(t, map[string]string{"runtime_uid": "1001", "runtime_gid": "2001"}))
	if distinct.RuntimeIdentity == nil || distinct.RuntimeIdentity.UID == nil || distinct.RuntimeIdentity.GID == nil ||
		*distinct.RuntimeIdentity.UID != 1001 || *distinct.RuntimeIdentity.GID != 2001 {
		t.Fatalf("runtime identity = %#v, want 1001:2001", distinct.RuntimeIdentity)
	}

	wantTargets := []string{"/config", "/metadata", "/podcasts", "/audiobooks"}
	if len(distinct.Mounts) != len(wantTargets) {
		t.Fatalf("writable mounts = %v, want all four migration targets", distinct.Mounts)
	}
	for i, target := range wantTargets {
		if distinct.Mounts[i].Target != target || distinct.Mounts[i].ReadOnly {
			t.Errorf("mount %d = %#v, want writable %s", i, distinct.Mounts[i], target)
		}
	}
}

func TestAudiobookshelfIdentityRejectsInvalidInput(t *testing.T) {
	shipped := walkShipped(t)
	pkg := shipped["audiobooks-audiobookshelf"].pkg
	all := make([]*pkgspec.Package, 0, len(shipped))
	for _, candidate := range shipped {
		all = append(all, candidate.pkg)
	}
	catalog := capability.NewCatalog(all...).Rehearsal()
	base := map[string]string{
		"public_domain": "example.internal", "audiobooks_subdomain": "audiobooks", "node": "n1",
		"audiobooks_path": "/srv/wildstone/media/audiobooks", "timezone": "UTC",
	}
	for _, value := range []string{"-1", "4294967296", "1000:1000", "worker"} {
		params := cloneStrings(base)
		params["runtime_uid"] = value
		if _, err := capability.NewEngine().Expand(pkg, params, catalog); err == nil || !strings.Contains(err.Error(), "runtime_uid") {
			t.Errorf("runtime_uid %q error = %v, want parameter refusal", value, err)
		}
	}
	params := cloneStrings(base)
	params["runtime_gid"] = "1000"
	if _, err := capability.NewEngine().Expand(pkg, params, catalog); err == nil || !strings.Contains(err.Error(), "runtimeIdentity.uid") {
		t.Fatalf("gid without uid error = %v, want missing uid refusal", err)
	}
}

func TestAudiobookshelfMigrationPreservesDataBindingsAndRollback(t *testing.T) {
	before := renderAudiobookshelf(t, nil)
	migrated := renderAudiobookshelf(t, map[string]string{"runtime_uid": "1000", "runtime_gid": "1001"})
	rollback := renderAudiobookshelf(t, nil)

	beforeContainer := *audiobookshelfContainer(t, before)
	migratedContainer := *audiobookshelfContainer(t, migrated)
	rollbackContainer := *audiobookshelfContainer(t, rollback)
	migratedContainer.RuntimeIdentity = nil
	migratedContainer.PreflightMountAccess = false
	if !reflect.DeepEqual(beforeContainer, migratedContainer) {
		t.Errorf("migration changed more than runtime identity/preflight\n before: %#v\nmigrated: %#v", beforeContainer, migratedContainer)
	}
	if !reflect.DeepEqual(beforeContainer, rollbackContainer) {
		t.Error("rollback did not restore the byte-compatible image-default Container spec")
	}

	for _, rendered := range [][]resource.Resource{before, migrated, rollback} {
		var volumes []string
		var backup *resource.BackupPlanSpec
		var route *resource.RouteSpec
		for i := range rendered {
			switch rendered[i].Kind {
			case resource.KindVolume:
				volumes = append(volumes, rendered[i].Volume.Name)
			case resource.KindBackupPlan:
				backup = rendered[i].BackupPlan
			case resource.KindRoute:
				route = rendered[i].Route
			}
		}
		wantVolumes := []string{"audiobookshelf-config", "audiobookshelf-metadata", "audiobookshelf-podcasts", "audiobooks-backup-repo"}
		if !reflect.DeepEqual(volumes, wantVolumes) {
			t.Errorf("volumes = %v, want stable data bindings %v", volumes, wantVolumes)
		}
		if backup == nil || !reflect.DeepEqual(backup.Volumes, []string{"audiobookshelf-config", "audiobookshelf-metadata"}) {
			t.Errorf("backup coverage = %#v, want config and metadata", backup)
		}
		if route == nil || route.Upstream != "container/audiobookshelf:80" || route.Auth != "none" {
			t.Errorf("route changed across migration: %#v", route)
		}
	}
}

func cloneStrings(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
