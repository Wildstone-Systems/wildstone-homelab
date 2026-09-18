package portal

// The namespace root itself as a symlink.
//
// Every traversal test in this package asks whether a NAME can climb
// out of the namespace. This one asks the question underneath that:
// whether the namespace is where the operator said it was. A symlink
// planted at Shared, Photos or a member's own directory — by another
// container sharing the mount, or by anything that can write one entry
// into the storage root — used to be followed before the confinement
// existed, and everything the kernel then refused to leave was the
// attacker's directory.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A namespace symlinked OUT of the configured root is refused, and the
// refusal covers reading as well as writing.
func TestANamespaceSymlinkedOutOfTheRootIsRefused(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "the-operators-ssh-key"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "Shared")); err != nil {
		t.Skipf("this filesystem has no symlinks: %v", err)
	}

	s := &FileStore{Root: root, Member: "sandy"}

	if _, err := s.List("shared"); err == nil {
		t.Error("a member can list a directory outside the configured root")
	}
	if _, err := s.Save("shared", "landed.txt", strings.NewReader("x")); err == nil {
		t.Error("a member can write outside the configured root")
	}
	if _, _, err := s.Open("shared", "the-operators-ssh-key"); err == nil {
		t.Error("a member can read a file outside the configured root")
	}
	if err := s.Delete("shared", "the-operators-ssh-key"); err == nil {
		t.Error("a member can delete a file outside the configured root")
	}
	if _, err := s.Rename("shared", "the-operators-ssh-key", "mine.txt"); err == nil {
		t.Error("a member can rename a file outside the configured root")
	}
	// And nothing outside was touched by any of it.
	if _, err := os.Stat(filepath.Join(outside, "the-operators-ssh-key")); err != nil {
		t.Errorf("the file outside the root did not survive: %v", err)
	}
	if entries, err := os.ReadDir(outside); err == nil && len(entries) != 1 {
		t.Errorf("the directory outside the root now holds %d files", len(entries))
	}
}

// A member's own directory is the same boundary. It is created from
// their name rather than chosen, so the symlink has to be planted —
// which is exactly the case the configured root exists to bound.
func TestAPersonalSpaceSymlinkedOutOfTheRootIsRefused(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Members"), 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "Members", "sandy")); err != nil {
		t.Skipf("this filesystem has no symlinks: %v", err)
	}

	s := &FileStore{Root: root, Member: "sandy"}
	if _, err := s.Save("me", "diary.txt", strings.NewReader("x")); err == nil {
		t.Error("a member's personal space followed a symlink out of the root")
	}
	if entries, err := os.ReadDir(outside); err == nil && len(entries) != 0 {
		t.Errorf("the directory outside the root now holds %d files", len(entries))
	}
}

// An operator who arranges their own tree with a symlink INSIDE the
// root is not attacking themselves, and is not refused. The boundary
// is the configured root, not the absence of symlinks.
func TestASymlinkInsideTheRootStillWorks(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "RealShared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("RealShared", filepath.Join(root, "Shared")); err != nil {
		t.Skipf("this filesystem has no symlinks: %v", err)
	}

	s := &FileStore{Root: root, Member: "sandy"}
	if _, err := s.Save("shared", "grocery.txt", strings.NewReader("milk")); err != nil {
		t.Fatalf("an operator's own symlink inside the root was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "RealShared", "grocery.txt")); err != nil {
		t.Errorf("the upload did not land where the symlink points: %v", err)
	}
}
