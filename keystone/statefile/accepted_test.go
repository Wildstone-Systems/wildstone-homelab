package statefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// accepted wires s to a settable accepted revision, the shape
// cmd/keystone gives it from the runtime DB.
func accepted(s *Store) *string {
	rev := new(string)
	s.Accepted = func() (string, error) { return *rev, nil }
	return rev
}

func names(t *testing.T, s *Store) []string {
	t.Helper()
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out := make([]string, 0, len(got))
	for _, r := range got {
		out = append(out, string(r.Kind)+"/"+r.Metadata.Name)
	}
	return out
}

func TestLoadReadsOnlyTheAcceptedRevision(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rev := accepted(s)

	// An empty repository is an empty desired state, not an error.
	if got := names(t, s); len(got) != 0 {
		t.Fatalf("empty repo: %v", got)
	}

	if err := s.Put(volume("data")); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit("user", "plan-1", "add data"); err != nil {
		t.Fatal(err)
	}
	// History but nothing accepted: wait, never guess.
	if _, err := s.Load(); !errors.Is(err, ErrNoAcceptedRevision) {
		t.Fatalf("unaccepted history: err = %v, want ErrNoAcceptedRevision", err)
	}
	first, err := s.Head()
	if err != nil {
		t.Fatal(err)
	}
	*rev = first
	if got := names(t, s); len(got) != 1 || got[0] != "Volume/data" {
		t.Fatalf("accepted: %v", got)
	}

	// An uncommitted edit is not desired state.
	if err := s.Put(volume("scratch")); err != nil {
		t.Fatal(err)
	}
	if got := names(t, s); len(got) != 1 {
		t.Fatalf("uncommitted edit leaked into Load: %v", got)
	}
	// Nor is a committed one nobody accepted.
	if err := s.Commit("hand", "", "add scratch"); err != nil {
		t.Fatal(err)
	}
	if got := names(t, s); len(got) != 1 {
		t.Fatalf("unaccepted commit leaked into Load: %v", got)
	}
	// Until it is.
	second, err := s.Head()
	if err != nil {
		t.Fatal(err)
	}
	*rev = second
	if got := names(t, s); len(got) != 2 {
		t.Fatalf("after accepting: %v", got)
	}
}

func TestLoadAtMatchesTheWorkingReadOfTheSameTree(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"a", "b", "c"} {
		if err := s.Put(volume(r)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Put(container("jellyfin")); err != nil {
		t.Fatal(err)
	}
	// Not a resource: LoadAt must skip it as the working read does.
	if err := os.WriteFile(filepath.Join(s.Root, "README.md"), []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit("user", "plan-1", "seed"); err != nil {
		t.Fatal(err)
	}
	head, err := s.Head()
	if err != nil {
		t.Fatal(err)
	}
	working, err := s.LoadWorking()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // second read comes from the cache
		at, err := s.LoadAt(head)
		if err != nil {
			t.Fatalf("LoadAt: %v", err)
		}
		if len(at) != len(working) {
			t.Fatalf("LoadAt %d resources, working %d", len(at), len(working))
		}
		for j := range at {
			if at[j].Kind != working[j].Kind || at[j].Metadata.Name != working[j].Metadata.Name {
				t.Fatalf("resource %d: LoadAt %s/%s, working %s/%s", j,
					at[j].Kind, at[j].Metadata.Name, working[j].Kind, working[j].Metadata.Name)
			}
		}
		// Each call hands out its own resources.
		at[0].Metadata.Name = "mutated"
	}
}

func TestDirtyReportsUncommittedAndUntrackedFiles(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(volume("data")); err != nil {
		t.Fatal(err)
	}
	if dirty, err := s.Dirty(); err != nil || !dirty {
		t.Fatalf("untracked file: dirty=%v err=%v", dirty, err)
	}
	if err := s.Commit("user", "plan-1", "add"); err != nil {
		t.Fatal(err)
	}
	if dirty, err := s.Dirty(); err != nil || dirty {
		t.Fatalf("clean tree: dirty=%v err=%v", dirty, err)
	}
}

func TestParseCatFileBatchRejectsTruncatedOutput(t *testing.T) {
	if _, err := parseCatFileBatch([]byte("abc blob 10\nshort\n"), 1); err == nil {
		t.Fatal("want an error for a body shorter than its declared size")
	}
	got, err := parseCatFileBatch([]byte("a blob 2\nhi\nb blob 0\n\n"), 2)
	if err != nil || string(got[0]) != "hi" || len(got[1]) != 0 {
		t.Fatalf("got %q, %v", got, err)
	}
}

// A symbolic revision is re-read every time: it is not one tree.
func TestLoadAtDoesNotCacheSymbolicRevisions(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := s.Put(volume(name)); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit("user", "", "add "+name); err != nil {
			t.Fatal(err)
		}
		got, err := s.LoadAt("HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if name == "b" && len(got) != 2 {
			t.Fatalf("LoadAt(HEAD) after the second commit = %d resources, want 2", len(got))
		}
	}
}
