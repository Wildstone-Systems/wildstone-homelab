package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

func listDirHarness(t *testing.T) (*harness, string) {
	t.Helper()
	managed := filepath.Join(t.TempDir(), "managed")
	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.ManagedPrefixes = []string{managed} })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	return h, managed
}

func listDir(t *testing.T, h *harness, path string) *wsv1.OpResult {
	t.Helper()
	return dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ListDirectory{
		ListDirectory: &wsv1.ListDirectory{Path: path},
	}})
}

// Directories only, sorted, names only. Nothing in a package binds a
// single file, and a picker offering one would offer a mount that fails.
func TestListDirectoryReturnsSortedSubdirectoriesOnly(t *testing.T) {
	h, managed := listDirHarness(t)
	for _, d := range []string{"music", "audiobooks", "books"} {
		if err := os.MkdirAll(filepath.Join(managed, "media", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(managed, "media", "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := listDir(t, h, filepath.Join(managed, "media"))
	if oe := res.GetError(); oe != nil {
		t.Fatalf("listing failed: %s", oe.GetMessage())
	}
	got := res.GetListDirectory().GetDirectories()
	want := []string{"audiobooks", "books", "music"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v (sorted), got %v", want, got)
		}
	}
}

// The picker must only ever offer folders that could actually be bound.
// A bind mount outside the managed prefixes is refused at apply time, so
// browsing the host would offer paths that can never work — and would be
// a read primitive over the whole filesystem besides.
func TestListDirectoryRefusesOutsideTheManagedPrefix(t *testing.T) {
	h, _ := listDirHarness(t)
	for _, p := range []string{"/etc", "/", "/home", "/var/lib"} {
		if res := listDir(t, h, p); res.GetError() == nil {
			t.Errorf("listing %q was allowed; it must be refused", p)
		}
	}
}

// A folder that does not exist yet is a legitimate thing to point at.
// An empty answer says so better than a failed request.
func TestListDirectoryOnAMissingPathIsEmptyNotAnError(t *testing.T) {
	h, managed := listDirHarness(t)
	res := listDir(t, h, filepath.Join(managed, "not-created-yet"))
	if oe := res.GetError(); oe != nil {
		t.Fatalf("want an empty listing, got error: %s", oe.GetMessage())
	}
	if got := res.GetListDirectory().GetDirectories(); len(got) != 0 {
		t.Errorf("want none, got %v", got)
	}
}

// A symlink is a second name for a directory the prefix check may or may
// not admit. Offering one in a picker is offering a path that fails later.
func TestListDirectorySkipsSymlinks(t *testing.T) {
	h, managed := listDirHarness(t)
	if err := os.MkdirAll(filepath.Join(managed, "media", "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(managed, "media", "elsewhere")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	res := listDir(t, h, filepath.Join(managed, "media"))
	for _, d := range res.GetListDirectory().GetDirectories() {
		if d == "elsewhere" {
			t.Error("a symlink was offered as a folder")
		}
	}
}
