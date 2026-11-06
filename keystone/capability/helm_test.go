package capability

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"gopkg.in/yaml.v3"
)

func helmFixture(t *testing.T) (*pkgspec.Package, HelmBindings, string) {
	t.Helper()
	p, err := pkgspec.Load("testdata/audiobookshelf-helm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return p, HelmBindings{Name: "example-books", Namespace: "example-apps", ServiceAccount: "example-reconciler", ConfigClaim: "example-config", MetadataClaim: "example-metadata", MediaClaim: "example-media", SecretName: "example-session", SecretRevision: strings.Repeat("c", 64)}, strings.Repeat("a", 40)
}

func TestHelmProjectionBindsReviewAndPreservesStorage(t *testing.T) {
	p, b, rev := helmFixture(t)
	e := NewEngine()
	a, err := e.ProjectHelm(p, nil, b, rev)
	if err != nil {
		t.Fatal(err)
	}
	z, err := e.ProjectHelm(p, map[string]string{}, b, rev)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != z.Digest {
		t.Fatal("same input changed review digest")
	}
	for name, data := range a.Files {
		if !bytes.Equal(data, z.Files[name]) {
			t.Fatal("unstable output")
		}
	}
	var release map[string]any
	if err := yaml.Unmarshal(a.Files["release.yaml"], &release); err != nil {
		t.Fatal(err)
	}
	spec := release["spec"].(map[string]any)
	v := spec["values"].(map[string]any)
	if spec["serviceAccountName"] != b.ServiceAccount || v["replicaCount"] != 1 {
		t.Fatal("authority or singleton changed")
	}
	if v["permissionsInitContainer"].(map[string]any)["enabled"] != false {
		t.Fatal("root chown enabled")
	}
	for role, claim := range map[string]string{"config": b.ConfigClaim, "metadata": b.MetadataClaim} {
		if v["persistence"].(map[string]any)[role].(map[string]any)["existingClaim"] != claim {
			t.Fatal("chart may provision replacement storage")
		}
	}
	if _, err := e.Expand(p, nil, nil); err == nil {
		t.Fatal("chart entered ordinary install path")
	}
	changed, err := e.ProjectHelm(p, map[string]string{"timezone": "Europe/London"}, b, rev)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Digest == a.Digest {
		t.Fatal("changed values reused review")
	}
	p.Helm.SourceCommit = strings.Repeat("b", 40)
	changed, err = e.ProjectHelm(p, nil, b, rev)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Digest == a.Digest {
		t.Fatal("changed artifact reused review")
	}
	p, b, rev = helmFixture(t)
	changed, err = e.ProjectHelm(p, nil, b, strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	if changed.Digest == a.Digest {
		t.Fatal("changed accepted revision reused review")
	}
	b.SecretName = "other-session"
	changed, err = e.ProjectHelm(p, nil, b, rev)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Digest == a.Digest {
		t.Fatal("changed credential reference reused review")
	}
}

func TestHelmProjectionReferencesPrivateEncryptedSecretDeterministically(t *testing.T) {
	p, b, rev := helmFixture(t)
	b.SecretFile = "secret.sops.yaml"
	b.SecretGeneration = 7
	b.SecretRestartPolicy = "recreate-on-ciphertext-revision"
	projection, err := NewEngine().ProjectHelm(p, nil, b, rev)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Files) != 3 || projection.Files[b.SecretFile] != nil {
		t.Fatal("ciphertext entered the public-safe Helm projection")
	}
	var release map[string]any
	if err := yaml.Unmarshal(projection.Files["release.yaml"], &release); err != nil {
		t.Fatal(err)
	}
	annotations := release["spec"].(map[string]any)["values"].(map[string]any)["podAnnotations"].(map[string]any)
	if annotations["wildstone.dev/encrypted-secret-revision"] != b.SecretRevision {
		t.Fatal("Pod replacement is not bound to the ciphertext revision")
	}
	var kustomization map[string]any
	if err := yaml.Unmarshal(projection.Files["kustomization.yaml"], &kustomization); err != nil {
		t.Fatal(err)
	}
	if got := kustomization["resources"].([]any); !reflect.DeepEqual(got, []any{"source.yaml", "release.yaml", "secret.sops.yaml"}) {
		t.Fatalf("private Secret not referenced exactly once: %v", got)
	}
	for _, change := range []string{"revision", "generation"} {
		changed := b
		if change == "revision" {
			changed.SecretRevision = strings.Repeat("d", 64)
		} else {
			changed.SecretGeneration++
		}
		next, err := NewEngine().ProjectHelm(p, nil, changed, rev)
		if err != nil {
			t.Fatal(err)
		}
		if next.Digest == projection.Digest {
			t.Fatalf("changed secret %s reused projection digest", change)
		}
	}
}

func TestHelmProjectionRefusesArbitrarySecretProjection(t *testing.T) {
	for _, change := range []string{"path", "missing-generation", "restart", "partial"} {
		t.Run(change, func(t *testing.T) {
			p, b, rev := helmFixture(t)
			b.SecretFile = "secret.sops.yaml"
			b.SecretGeneration = 1
			b.SecretRestartPolicy = "recreate-on-ciphertext-revision"
			switch change {
			case "path":
				b.SecretFile = "../secret.sops.yaml"
			case "missing-generation":
				b.SecretGeneration = 0
			case "restart":
				b.SecretRestartPolicy = "always"
			case "partial":
				b.SecretFile = ""
			}
			if _, err := NewEngine().ProjectHelm(p, nil, b, rev); err == nil {
				t.Fatal("arbitrary/incomplete Secret projection accepted")
			}
		})
	}
}

func TestHelmProjectionRefusesUnsafeInput(t *testing.T) {
	for _, tc := range []string{"raw-values", "timezone", "revision", "secret-revision", "namespace", "same-claim", "source-credentials", "floating-source", "floating-image", "traversal", "templates", "post-hook", "unknown-field"} {
		t.Run(tc, func(t *testing.T) {
			p, b, rev := helmFixture(t)
			params := map[string]string{}
			switch tc {
			case "raw-values":
				params["values"] = "privileged"
			case "timezone":
				params["timezone"] = "UTC\nextra: true"
			case "revision":
				rev = "main"
			case "secret-revision":
				b.SecretRevision = "unversioned"
			case "namespace":
				b.Namespace = "kube-system"
			case "same-claim":
				b.MediaClaim = b.ConfigClaim
			case "source-credentials":
				p.Helm.SourceURL = "https://user@example.invalid/repo.git"
			case "floating-source":
				p.Helm.SourceCommit = "main"
			case "floating-image":
				p.Helm.Image = "example.invalid/app:latest"
			case "traversal":
				p.Helm.ChartPath = "../other"
			case "templates":
				p.Templates = []string{"unreviewed"}
			case "post-hook":
				p.Lifecycle.PostInstall = []pkgspec.HookSpec{{}}
			case "unknown-field":
				data, _ := os.ReadFile("testdata/audiobookshelf-helm.yaml")
				if _, err := pkgspec.Parse(append(data, []byte("  rawValues: {}\n")...)); err == nil {
					t.Fatal("unknown chart field accepted")
				}
				return
			}
			if _, err := NewEngine().ProjectHelm(p, params, b, rev); err == nil {
				t.Fatal("unsafe input accepted")
			}
		})
	}
}

// Optional output directory is only for local schema/Helm verification; the
// production projection API itself has no filesystem or network side effects.
func TestHelmProjectionExport(t *testing.T) {
	dir := os.Getenv("WILDSTONE_PROJECTION_TEST_OUT")
	if dir == "" {
		t.Skip("optional local schema validation export")
	}
	p, b, rev := helmFixture(t)
	r, err := NewEngine().ProjectHelm(p, nil, b, rev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range r.Files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
