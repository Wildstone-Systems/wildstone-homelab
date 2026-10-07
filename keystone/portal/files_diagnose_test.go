package portal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #551: the root takes writes, the namespaces below it do not.
func TestDiagnoseStorageFindsUnwritableNamespacesUnderWritableRoot(t *testing.T) {
	s := store(t)
	for dir, mode := range map[string]os.FileMode{"Shared": 0o755, "Photos": 0o755, "Members": 0o711} {
		if err := os.Mkdir(filepath.Join(s.Root, dir), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(s.Root, dir), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(s.Root, "Members", "alice"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, "Members", "alice", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("permission denied")
	s.access = func(root *os.Root, rel string, write bool) error {
		if write && rel != "." {
			return denied
		}
		if !write && rel == "Members/alice" {
			return denied
		}
		return nil
	}
	got, err := s.DiagnoseStorage()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, f := range got {
		seen[strings.TrimPrefix(f.Path, s.Root+"/")] = f.Need
	}
	want := map[string]string{"Shared": "write", "Photos": "write", "Members": "write", "Members/alice": "list"}
	if len(seen) != len(want) {
		t.Fatalf("findings = %v", got)
	}
	for p, need := range want {
		if seen[p] != need {
			t.Fatalf("%s: %v", p, got)
		}
	}
	for _, f := range got {
		if f.Path == filepath.Join(s.Root, "Members") && !strings.HasSuffix(f.Owner, "0711") {
			t.Fatalf("Members mode not reported: %q", f.Owner)
		}
	}
	if msg := StorageProblem(got); !strings.Contains(msg, "Shared") || !strings.Contains(msg, "0711") {
		t.Fatalf("reason = %q", msg)
	}
	// Diagnosis must not have changed anything.
	if info, _ := os.Stat(filepath.Join(s.Root, "Members")); info.Mode().Perm() != 0o711 {
		t.Fatalf("Members mode changed to %v", info.Mode().Perm())
	}
}

func TestDiagnoseStorageHealthyAndReadOnly(t *testing.T) {
	s := store(t)
	save(t, s, "shared", "a.txt", "a")
	got, err := s.DiagnoseStorage()
	if err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root, "Shared"))
	if len(entries) != 1 {
		t.Fatalf("probe left residue: %v", entries)
	}
	fresh := &FileStore{Root: filepath.Join(t.TempDir(), "absent")}
	if got, err := fresh.DiagnoseStorage(); err != nil || got != nil {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := os.Stat(fresh.Root); err == nil {
		t.Fatal("diagnosis created the root")
	}
}

func TestDiagnoseStorageSkipsWriteProbesWhenFenced(t *testing.T) {
	s := store(t)
	save(t, s, "shared", "a.txt", "a")
	s.BeforeWrite = func() error { return errors.New("fenced") }
	s.access = func(_ *os.Root, _ string, write bool) error {
		if write {
			t.Fatal("write probe ran while fenced")
		}
		return nil
	}
	if _, err := s.DiagnoseStorage(); err != nil {
		t.Fatal(err)
	}
}

// A root that cannot be examined is an error, not a healthy empty result.
func TestDiagnoseStoragePropagatesRootStatFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &FileStore{Root: filepath.Join(file, "root")}
	if got, err := s.DiagnoseStorage(); err == nil {
		t.Fatalf("want error, got findings %v", got)
	}
}

// The requesting member's own directory is write-probed; others are not.
func TestDiagnoseStorageProbesRequestersOwnDirectory(t *testing.T) {
	s := store(t)
	s.Member = "alice"
	for _, d := range []string{"Members/alice", "Members/bob"} {
		if err := os.MkdirAll(filepath.Join(s.Root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var wrote []string
	s.access = func(_ *os.Root, rel string, write bool) error {
		if write {
			wrote = append(wrote, rel)
			if rel == "Members/alice" {
				return errors.New("permission denied")
			}
		}
		return nil
	}
	got, err := s.DiagnoseStorage()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Need != "write" || !strings.HasSuffix(got[0].Path, "Members/alice") {
		t.Fatalf("findings = %v", got)
	}
	for _, rel := range wrote {
		if rel == "Members/bob" {
			t.Fatal("another member's directory was write-probed")
		}
	}
}
