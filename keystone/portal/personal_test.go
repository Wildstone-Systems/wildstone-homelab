package portal

// The personal namespace, and the one property it exists for: one
// member's own space is not reachable by another.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A member's own space resolves under their own name, and nothing a
// caller can say reaches somebody else's.
func TestThePersonalSpaceIsResolvedFromTheMemberAndNothingElse(t *testing.T) {
	root := t.TempDir()
	sandy := &FileStore{Root: root, Member: "sandy"}
	maya := &FileStore{Root: root, Member: "maya"}

	if _, err := sandy.Save("me", "diary.txt", strings.NewReader("sandy's")); err != nil {
		t.Fatal(err)
	}
	if _, err := maya.Save("me", "notes.txt", strings.NewReader("maya's")); err != nil {
		t.Fatal(err)
	}

	// Each sees only their own.
	for _, c := range []struct {
		store   *FileStore
		want    string
		notWant string
	}{
		{sandy, "diary.txt", "notes.txt"},
		{maya, "notes.txt", "diary.txt"},
	} {
		items, err := c.store.List("me")
		if err != nil {
			t.Fatalf("%s: %v", c.store.Member, err)
		}
		var names []string
		for _, i := range items {
			names = append(names, i.Name)
		}
		joined := strings.Join(names, ",")
		if !strings.Contains(joined, c.want) {
			t.Errorf("%s cannot see their own %s (saw %v)", c.store.Member, c.want, names)
		}
		if strings.Contains(joined, c.notWant) {
			t.Errorf("%s can see %s, which is not theirs", c.store.Member, c.notWant)
		}
	}

	// And they are physically separate.
	for member, file := range map[string]string{"sandy": "diary.txt", "maya": "notes.txt"} {
		if _, err := os.Stat(filepath.Join(root, "Members", member, file)); err != nil {
			t.Errorf("%s/%s is not where it should be: %v", member, file, err)
		}
	}
}

// There is no spelling of another person's space. The member is not a
// parameter anywhere, so this is a property of the signature — but the
// namespace KEY is caller-supplied, and these are the things somebody
// would try to put in it.
func TestNoNamespaceKeyReachesAnotherPersonsSpace(t *testing.T) {
	root := t.TempDir()
	maya := &FileStore{Root: root, Member: "maya"}
	if _, err := maya.Save("me", "notes.txt", strings.NewReader("maya's")); err != nil {
		t.Fatal(err)
	}

	sandy := &FileStore{Root: root, Member: "sandy"}
	for _, key := range []string{
		"me/maya", "Members/maya", "../Members/maya", "me/../maya",
		"maya", "me ", "ME", "me\x00", "members/maya",
	} {
		if _, err := sandy.List(key); !errors.Is(err, ErrNamespace) {
			t.Errorf("List(%q) = %v, want ErrNamespace", key, err)
		}
		if _, err := sandy.Save(key, "x.txt", strings.NewReader("x")); !errors.Is(err, ErrNamespace) {
			t.Errorf("Save(%q) = %v, want ErrNamespace", key, err)
		}
		if err := sandy.Delete(key, "notes.txt"); !errors.Is(err, ErrNamespace) {
			t.Errorf("Delete(%q) = %v, want ErrNamespace", key, err)
		}
	}
	// Maya's file is untouched.
	if _, err := os.Stat(filepath.Join(root, "Members", "maya", "notes.txt")); err != nil {
		t.Errorf("maya's file was reached: %v", err)
	}
}

// A member name this cannot turn into a directory gets NO personal
// space, never the shared one. An upload that landed in Shared because
// a name had an odd character in it would be a privacy failure wearing
// a shrug.
func TestAnUnusableMemberNameGetsNoPersonalSpaceRatherThanTheSharedOne(t *testing.T) {
	root := t.TempDir()
	for _, member := range []string{"", "   ", "a/b", "../x", "..", ".", "a b", "a:b", strings.Repeat("x", 65)} {
		s := &FileStore{Root: root, Member: member}
		if _, err := s.Save("me", "x.txt", strings.NewReader("x")); !errors.Is(err, ErrNamespace) {
			t.Errorf("Save as %q = %v, want ErrNamespace", member, err)
		}
	}
	// Nothing landed anywhere.
	for _, dir := range []string{"Members", "Shared", "Photos"} {
		if entries, err := os.ReadDir(filepath.Join(root, dir)); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					t.Errorf("a file landed in %s: %s", dir, e.Name())
				}
			}
		}
	}
}

// Two spellings of one person are one person, because the rest of the
// household compares member names case-insensitively.
func TestOneMemberIsOneFolderWhateverTheCase(t *testing.T) {
	root := t.TempDir()
	if _, err := (&FileStore{Root: root, Member: "Sandy"}).Save("me", "a.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	items, err := (&FileStore{Root: root, Member: "sandy"}).List("me")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("sandy sees %d files that Sandy saved, want 1", len(items))
	}
}

// The personal space is not world-readable the way the shared ones are,
// and its parent cannot be listed.
func TestThePersonalSpaceIsNotOpenedUpToEveryContainer(t *testing.T) {
	root := t.TempDir()
	s := &FileStore{Root: root, Member: "sandy"}
	if _, err := s.Save("me", "a.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	mine, err := os.Stat(filepath.Join(root, "Members", "sandy"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := mine.Mode().Perm(); perm != 0o700 {
		t.Errorf("a member's own folder is %o, want 0700", perm)
	}
	parent, err := os.Stat(filepath.Join(root, "Members"))
	if err != nil {
		t.Fatal(err)
	}
	// Traversable so each member can reach their own, not listable so
	// nothing can enumerate the household.
	if perm := parent.Mode().Perm(); perm&0o044 != 0 {
		t.Errorf("Members/ is %o, which lets something list the household", perm)
	}

	// The shared destinations keep their 0755: the media server has to
	// be able to read what the family dropped.
	if _, err := s.Save("shared", "b.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	shared, err := os.Stat(filepath.Join(root, "Shared"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := shared.Mode().Perm(); perm != 0o755 {
		t.Errorf("Shared is %o, want 0755 — other apps must still read it", perm)
	}
}

// Recent spans the destinations this member can see, and no others.
func TestRecentDoesNotLeakAnotherPersonsFiles(t *testing.T) {
	root := t.TempDir()
	if _, err := (&FileStore{Root: root, Member: "maya"}).Save("me", "maya-secret.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	sandy := &FileStore{Root: root, Member: "sandy"}
	if _, err := sandy.Save("shared", "grocery.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	items, err := sandy.Recent(20)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range items {
		if strings.Contains(i.Name, "maya-secret") {
			t.Errorf("sandy's recent list carries %s", i.Name)
		}
	}
}
