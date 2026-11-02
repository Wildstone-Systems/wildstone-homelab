package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/localgit"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

func TestCommandWritesOnlyPrivateReviewBundle(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "source.json")
	out := filepath.Join(root, "review")
	c := localgit.Config{ListenAddress: "127.0.0.1", Port: 22222, ClientCIDR: "127.0.0.1/32", ServiceUser: "example-source", Repository: filepath.Join(root, "deployment.git"), MirrorRepository: filepath.Join(root, "source.git"), InstallDirectory: filepath.Join(root, "installed"), Namespace: "example-app", SourceName: "example-local", CredentialSecret: "example-key"}
	repo, err := statefile.OpenLocalDeployment(c.Repository)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(input, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.InstallDirectory); !os.IsNotExist(err) {
		t.Fatal("rendering created installed configuration")
	}
	if head, err := repo.Head(); err != nil || head != "" {
		t.Fatal("rendering mutated deployment ref", err)
	}
	if err := run(input, out); err == nil {
		t.Fatal("overwrote bundle")
	}
	if err := os.Chmod(input, 0644); err != nil {
		t.Fatal(err)
	}
	if err := run(input, filepath.Join(root, "public")); err == nil {
		t.Fatal("accepted public installation configuration")
	}
	if err := os.Chmod(input, 0600); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{`{"unknown":"refused"}`, string(raw) + ` {}`, `{"listenAddress":`} {
		if err := os.WriteFile(input, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := run(input, filepath.Join(root, "invalid")); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}
