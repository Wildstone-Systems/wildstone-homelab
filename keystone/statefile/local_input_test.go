package statefile

import (
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLocalProductInputIsolation(t *testing.T) {
	for _, change := range []string{"remote", "nested-checkout", "software-marker", "shared-objects", "foreign-tree"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			s, err := New(filepath.Join(root, "statefile"))
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := s.BuildCandidate("", nil, nil, "example-owner", "example-operation", "example empty input")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.CheckLocalInput(candidate); err != nil {
				t.Fatal("isolated input refused", err)
			}
			switch change {
			case "remote":
				cmd := exec.Command("git", "-C", s.Root, "remote", "add", "external", "https://example.invalid/forbidden.git")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatal(err, string(out))
				}
			case "nested-checkout":
				if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
					t.Fatal(err)
				}
			case "software-marker":
				if err := os.WriteFile(filepath.Join(s.Root, "go.mod"), []byte("module example.invalid/source"), 0600); err != nil {
					t.Fatal(err)
				}
			case "shared-objects":
				if err := os.WriteFile(filepath.Join(s.Root, ".git", "objects", "info", "alternates"), []byte(filepath.Join(root, "elsewhere")), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-tree":
				humanCommit(t, s, "not-product.txt", "example unrelated data")
				candidate, err = s.Head()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := s.CheckLocalInput(candidate); err == nil {
				t.Fatal("unsafe product repository accepted")
			}
		})
	}
}

func TestProductGitIgnoresInheritedCheckoutConfiguration(t *testing.T) {
	root := t.TempDir()
	foreign, err := New(filepath.Join(root, "foreign"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(filepath.Join(root, "product"))
	if err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(root, "forbidden-trace")
	t.Setenv("GIT_DIR", filepath.Join(foreign.Root, ".git"))
	t.Setenv("GIT_WORK_TREE", foreign.Root)
	t.Setenv("GIT_TRACE", trace)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	candidate, err := s.BuildCandidate("", []resource.Resource{volume("example-data")}, nil, "example-owner", "example-operation", "example product")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Mirror(candidate); err != nil {
		t.Fatal(err)
	}
	if head, err := s.Head(); err != nil || head != candidate {
		t.Fatal("product Git redirected", err)
	}
	if head, err := foreign.Head(); err != nil || head != "" {
		t.Fatal("foreign checkout changed", err)
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatal("Git tracing exposed product operations")
	}
}
