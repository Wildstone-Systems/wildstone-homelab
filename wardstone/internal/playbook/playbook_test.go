package playbook

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func signedSet(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "k3s-server.yml", "- hosts: localhost\n")
	write(t, dir, "templates/config.yaml.j2", "secrets-encryption: true\n")
	write(t, dir, CatalogFile, CatalogHeader+"\nk3s-patch-update 1 k3s-patch-update-v1 k3s-server.yml\n")
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, ManifestFile, string(manifest))
	write(t, dir, SignatureFile, string(Sign(priv, manifest)))
	return dir, pub
}

func TestSignedCatalogBindsIdentityVersionSchemaAndFile(t *testing.T) {
	dir, pub := signedSet(t)
	e, err := VerifyEntry(dir, pub, "k3s-patch-update", "1", "k3s-patch-update-v1")
	if err != nil || e.File != "k3s-server.yml" {
		t.Fatalf("VerifyEntry = %+v, %v", e, err)
	}
	for _, tc := range []struct{ identity, version, schema string }{
		{"other", "1", "k3s-patch-update-v1"},
		{"k3s-patch-update", "2", "k3s-patch-update-v1"},
		{"k3s-patch-update", "1", "free-map-v1"},
	} {
		if _, err := VerifyEntry(dir, pub, tc.identity, tc.version, tc.schema); err == nil {
			t.Fatalf("VerifyEntry(%q,%q,%q) accepted", tc.identity, tc.version, tc.schema)
		}
	}
	write(t, dir, CatalogFile, CatalogHeader+"\nk3s-patch-update 1 k3s-patch-update-v1 other.yml\n")
	if _, err := VerifyEntry(dir, pub, "k3s-patch-update", "1", "k3s-patch-update-v1"); !errors.Is(err, ErrTampered) {
		t.Fatalf("tampered catalog = %v, want ErrTampered", err)
	}

	dir = t.TempDir()
	write(t, dir, CatalogFile, CatalogHeader+"\nk3s-patch-update 1 k3s-patch-update-v1 missing.yml\n")
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, ManifestFile, string(manifest))
	write(t, dir, SignatureFile, string(Sign(priv, manifest)))
	if _, err := VerifyEntry(dir, pub, "k3s-patch-update", "1", "k3s-patch-update-v1"); err == nil {
		t.Fatal("signed catalog accepted a missing target")
	}
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAnUntouchedSignedSetVerifies(t *testing.T) {
	dir, pub := signedSet(t)
	if err := Verify(dir, pub); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestATamperedSetIsRefused(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, dir string)
		want   error
	}{
		{"an edited playbook", func(t *testing.T, dir string) {
			write(t, dir, "k3s-server.yml", "- hosts: all\n")
		}, ErrTampered},
		{"a deleted file", func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, "templates/config.yaml.j2"))
		}, ErrTampered},
		{"a file nobody signed", func(t *testing.T, dir string) {
			write(t, dir, "roles/extra/tasks/main.yml", "- shell: id\n")
		}, ErrUnlisted},
		{"an edited manifest", func(t *testing.T, dir string) {
			m, _ := os.ReadFile(filepath.Join(dir, ManifestFile))
			write(t, dir, ManifestFile, string(m)+"0000000000000000000000000000000000000000000000000000000000000000  extra.yml\n")
		}, ErrUnsigned},
		{"a symlink", func(t *testing.T, dir string) {
			if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link.yml")); err != nil {
				t.Skip(err)
			}
		}, ErrUnlisted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, pub := signedSet(t)
			c.change(t, dir)
			if err := Verify(dir, pub); !errors.Is(err, c.want) {
				t.Fatalf("Verify = %v, want %v", err, c.want)
			}
		})
	}
}

func TestAnotherKeyIsRefused(t *testing.T) {
	dir, _ := signedSet(t)
	other, _, _ := ed25519.GenerateKey(nil)
	if err := Verify(dir, other); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("Verify with another key = %v, want ErrUnsigned", err)
	}
}
