package statefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func localFiles() map[string][]byte {
	return map[string][]byte{"source.yaml": []byte("source\n"), "release.yaml": []byte("release\n"), "kustomization.yaml": []byte("list\n")}
}

func TestLocalDeploymentHidesPreparedRefsAndUsesCompleteSnapshots(t *testing.T) {
	d, err := OpenLocalDeployment(filepath.Join(t.TempDir(), "deployment.git"))
	if err != nil {
		t.Fatal(err)
	}
	files := localFiles()
	candidate, err := d.Build("", "example-op", files)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Retain(candidate, candidate); err != nil {
		t.Fatal(err)
	}
	if err := d.Retain(candidate, candidate); err != nil {
		t.Fatal("retention retry", err)
	}
	advertised, err := d.git(nil, "upload-pack", "--advertise-refs", d.root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(advertised, candidate) || strings.Contains(advertised, "refs/wildstone/") {
		t.Fatal("prepared ref advertised")
	}
	if err := d.Promote("", candidate); err != nil {
		t.Fatal(err)
	}
	advertised, err = d.git(nil, "upload-pack", "--advertise-refs", d.root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(advertised, DeploymentRef) {
		t.Fatal("served ref unavailable")
	}
	for name, data := range files {
		got, err := d.git(nil, "show", candidate+":"+name)
		if err != nil || got != strings.TrimSpace(string(data)) {
			t.Fatal("file mismatch", name, err)
		}
	}
	if _, err := d.git(nil, "update-ref", DeploymentRef+"-other", candidate); err != nil {
		t.Fatal(err)
	}
	if head, err := d.Head(); err != nil || head != candidate {
		t.Fatal("prefix ref confused exact head", head, err)
	}
	next, err := d.Build(candidate, "example-next", files)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Promote("", next); err == nil {
		t.Fatal("stale CAS succeeded")
	}
}

func TestLocalDeploymentRejectsForeignReposAndEnvironmentRedirection(t *testing.T) {
	root := filepath.Join(t.TempDir(), "deployment.git")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "not-the-repository"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "remote.origin.url")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://example.invalid/never-contacted.git")
	d, err := OpenLocalDeployment(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Build("", "example-op", localFiles()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, deploymentMarker)); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLocalDeployment(root); err == nil {
		t.Fatal("foreign repository accepted")
	}
}
