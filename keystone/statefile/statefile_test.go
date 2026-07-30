package statefile

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func digest() string {
	return "ghcr.io/example/app@sha256:" + strings.Repeat("b", 64)
}

func volume(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindVolume,
		Metadata:   resource.Metadata{Name: name, Owner: "system"},
		Volume:     &resource.VolumeSpec{Name: name},
	}
}

func container(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/media", Node: "node-a"},
		Container:  &resource.ContainerSpec{Image: digest()},
	}
}

func TestPutCommitListLoadRoundTrip(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	v := volume("data")
	c := container("jellyfin")

	if err := s.Put(v); err != nil {
		t.Fatalf("Put volume: %v", err)
	}
	if err := s.Put(c); err != nil {
		t.Fatalf("Put container: %v", err)
	}
	if err := s.Commit("user", "plan-1", "initial resources"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	refs, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("List: got %d refs, want 2: %+v", len(refs), refs)
	}
	// Volume < Container lexicographically? Kind is capitalized strings;
	// just assert both are present.
	names := map[string]bool{}
	for _, r := range refs {
		names[string(r.Kind)+"/"+r.Name] = true
	}
	if !names["Volume/data"] || !names["Container/jellyfin"] {
		t.Fatalf("unexpected refs: %+v", refs)
	}

	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("Load: got %d resources, want 2", len(loaded))
	}
	for _, r := range loaded {
		if err := r.Validate(); err != nil {
			t.Errorf("loaded resource failed validation: %v", err)
		}
	}
}

func TestCommitStructuredMessage(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Put(volume("data")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Commit("user", "plan-1", "initial resources"); err != nil {
		t.Fatalf("Commit 1: %v", err)
	}

	if err := s.Put(container("jellyfin")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Commit("butler", "plan-2", "add jellyfin container"); err != nil {
		t.Fatalf("Commit 2: %v", err)
	}

	out, err := exec.CommandContext(context.Background(), "git", "-C", root, "log", "-1", "--pretty=%B").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	msg := string(out)
	if !strings.Contains(msg, "add jellyfin container") {
		t.Errorf("commit message missing subject: %q", msg)
	}
	if !strings.Contains(msg, "Who: butler") {
		t.Errorf("commit message missing Who: %q", msg)
	}
	if !strings.Contains(msg, "Plan: plan-2") {
		t.Errorf("commit message missing Plan: %q", msg)
	}
	if !strings.Contains(msg, "Why: add jellyfin container") {
		t.Errorf("commit message missing Why: %q", msg)
	}

	// Two commits total: git init does not itself commit.
	logOut, err := exec.CommandContext(context.Background(), "git", "-C", root, "log", "--oneline").Output()
	if err != nil {
		t.Fatalf("git log --oneline: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(logOut)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 commits, got %d: %q", len(lines), string(logOut))
	}
}

func TestDeleteThenCommit(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Put(volume("data")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Commit("user", "plan-1", "add volume"); err != nil {
		t.Fatalf("Commit 1: %v", err)
	}

	if err := s.Delete(resource.KindVolume, "data"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Commit("user", "plan-2", "remove volume"); err != nil {
		t.Fatalf("Commit 2: %v", err)
	}

	refs, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected no refs after delete, got %+v", refs)
	}

	out, err := exec.CommandContext(context.Background(), "git", "-C", root, "log", "-1", "--pretty=%B").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(string(out), "remove volume") {
		t.Errorf("delete commit missing subject: %q", string(out))
	}
}

func TestDeleteNonexistentIsNotError(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Delete(resource.KindVolume, "nope"); err != nil {
		t.Fatalf("Delete of nonexistent resource should not error: %v", err)
	}
}

func TestGitRunnerSeamInjectable(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var calls []string
	s.git = func(ctx context.Context, dir string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return defaultGitRunner(ctx, dir, args...)
	}
	if err := s.Put(volume("data")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Commit("user", "plan-1", "add volume"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("expected injected gitRunner to be invoked")
	}
}
