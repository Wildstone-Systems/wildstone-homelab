package statefile

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

func humanCommit(t *testing.T, s *Store, file, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(s.Root, file)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=mika", "-c", "user.email=mika@example.invalid", "commit", "-qm", "by hand"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = s.Root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// A candidate is built from the base tree alone: the working files, the
// index and the branch are untouched until Mirror.
func TestBuildCandidateTouchesNoWorkingFile(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// From an empty repository.
	first, err := s.BuildCandidate("", []resource.Resource{volume("data")}, nil, "test", "plan-1", "add data")
	if err != nil {
		t.Fatalf("BuildCandidate: %v", err)
	}
	if head, _ := s.Head(); head != "" {
		t.Fatalf("building moved the branch to %s", head)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "volume", "data.yaml")); !os.IsNotExist(err) {
		t.Fatalf("building wrote a working file: %v", err)
	}
	got, err := s.LoadAt(first)
	if err != nil || len(got) != 1 || got[0].Metadata.Name != "data" {
		t.Fatalf("candidate tree = %+v, %v", got, err)
	}

	// An uncommitted edit never rides into a candidate.
	if err := s.Put(volume("scratch")); err != nil {
		t.Fatal(err)
	}
	second, err := s.BuildCandidate(first, []resource.Resource{volume("photos")}, []ResourceRef{{Kind: "Volume", Name: "data"}}, "test", "plan-2", "swap")
	if err != nil {
		t.Fatal(err)
	}
	got, _ = s.LoadAt(second)
	if len(got) != 1 || got[0].Metadata.Name != "photos" {
		t.Fatalf("second candidate = %+v; want only photos", got)
	}
	// An unchanged tree is no new commit.
	if same, err := s.BuildCandidate(second, nil, nil, "test", "", "nothing"); err != nil || same != second {
		t.Fatalf("no-op candidate = %s, %v; want %s", same, err, second)
	}
}

// Mirror fast-forwards the branch, from an unborn one too, keeps an
// unrelated local edit, and refuses to overwrite a diverged history.
func TestMirrorOnlyFastForwards(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, _ := s.BuildCandidate("", []resource.Resource{volume("data")}, nil, "test", "", "add data")
	if err := s.Mirror(first); err != nil {
		t.Fatalf("Mirror onto an unborn branch: %v", err)
	}
	if head, _ := s.Head(); head != first {
		t.Fatalf("head = %s, want %s", head, first)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "volume", "data.yaml")); err != nil {
		t.Fatalf("mirror did not write the working file: %v", err)
	}
	if dirty, _ := s.Dirty(); dirty {
		t.Fatal("mirror left the tree dirty")
	}

	// A local untracked note survives a fast-forward.
	if err := os.WriteFile(filepath.Join(s.Root, "NOTES.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, _ := s.BuildCandidate(first, []resource.Resource{volume("photos")}, nil, "test", "", "add photos")
	if err := s.Mirror(second); err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "NOTES.md")); err != nil {
		t.Fatalf("mirror discarded a local file: %v", err)
	}

	// A human commits on top; a candidate built from second is not a
	// descendant of the branch any more.
	humanCommit(t, s, "volume/hand.yaml", "not even a resource\n")
	third, _ := s.BuildCandidate(second, []resource.Resource{volume("music")}, nil, "test", "", "add music")
	if err := s.Mirror(third); !errors.Is(err, ErrDiverged) {
		t.Fatalf("Mirror over a diverged branch = %v, want ErrDiverged", err)
	}
}

func TestPublicationRefsAreCompareAndSwap(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.BuildCandidate("", []resource.Resource{volume("a")}, nil, "test", "", "a")
	b, _ := s.BuildCandidate("", []resource.Resource{volume("b")}, nil, "test", "", "b")
	ref := PublicationRefPrefix + "pub-1"
	if err := s.CreateRef(ref, a); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRef(ref, b); err == nil {
		t.Fatal("an existing publication ref was moved")
	}
	if got, _ := s.RefTarget(ref); got != a {
		t.Fatalf("ref = %s, want %s", got, a)
	}
	if err := s.DeleteRef(ref, b); err == nil {
		t.Fatal("a ref was deleted at the wrong old value")
	}
	if err := s.DeleteRef(ref, a); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RefTarget(ref); got != "" {
		t.Fatalf("ref after delete = %q", got)
	}
}

// An accepted revision only a publication ref reaches (the branch never
// caught up) survives a bundle and restore, and so does the ref.
func TestPublicationRefsSurviveBundleAndRestore(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := s.BuildCandidate("", []resource.Resource{volume("data")}, nil, "test", "", "add")
	if err := s.Mirror(first); err != nil {
		t.Fatal(err)
	}
	unmirrored, _ := s.BuildCandidate(first, []resource.Resource{volume("photos")}, nil, "test", "", "add photos")
	ref := PublicationRefPrefix + "pub-only"
	if err := s.CreateRef(ref, unmirrored); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "statefile.bundle")
	if err := s.Bundle(bundle); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreBundle(bundle, filepath.Join(t.TempDir(), "restored"))
	if err != nil {
		t.Fatalf("RestoreBundle: %v", err)
	}
	if !restored.HasCommit(unmirrored) {
		t.Fatal("the unmirrored accepted revision did not survive the restore")
	}
	if got, _ := restored.RefTarget(ref); got != unmirrored {
		t.Fatalf("publication ref after restore = %q, want %s", got, unmirrored)
	}
}
