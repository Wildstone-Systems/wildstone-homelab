package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRootKernelOpenRefusesEverySymlinkComponent(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if e := os.Mkdir(real, 0700); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(base, "link")
	if e := os.Symlink(real, link); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{link, filepath.Join(link, "child")} {
		if r, e := openRootNoSymlinks(p); e == nil {
			r.Close()
			t.Fatal("followed symlink", p)
		}
	}
	r, e := openRootNoSymlinks(real)
	if e != nil {
		t.Fatal(e)
	}
	r.Close()
}
func TestRootDescendantRefusesMountCrossing(t *testing.T) {
	// /proc is an existing distinct mount, requiring no mount privileges/fixtures.
	root, e := os.OpenRoot("/")
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	if child, e := openRootDescendant(root, "proc"); e == nil {
		child.Close()
		t.Fatal("crossed mount")
	}
}
func TestMissingRootNeverReplacedBySymlink(t *testing.T) {
	base := t.TempDir()
	target := t.TempDir()
	s := validSettings(filepath.Join(base, "root"))
	if e := os.Symlink(target, s.Root); e != nil {
		t.Fatal(e)
	}
	if r, e := s.OpenRoot(); !errors.Is(e, ErrRoot) {
		if r != nil {
			r.Close()
		}
		t.Fatal(e)
	}
	entries, e := os.ReadDir(target)
	if e != nil || len(entries) != 0 {
		t.Fatal(entries, e)
	}
}
