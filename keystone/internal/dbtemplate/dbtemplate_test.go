package dbtemplate

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

// writeFile is a stand-in migration: it produces a file with known
// contents and nothing else. These tests are about the copying, not
// about SQLite.
func writeFile(body string) func(string) error {
	return func(path string) error { return os.WriteFile(path, []byte(body), 0o600) }
}

func TestTheTemplateIsBuiltOnceAndCopiedPerTest(t *testing.T) {
	var mu sync.Mutex
	builds := 0
	tpl := New(func(path string) error {
		mu.Lock()
		builds++
		mu.Unlock()
		return writeFile("schema")(path)
	})

	first, second := tpl.Path(t), tpl.Path(t)
	if builds != 1 {
		t.Fatalf("built %d times, want exactly one build for the binary", builds)
	}
	if first == second {
		t.Fatalf("both tests were handed the same file %q; each needs its own", first)
	}
	for _, path := range []string{first, second} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if string(got) != "schema" {
			t.Fatalf("copy at %s is %q, want the migrated contents", path, got)
		}
	}
}

func TestOneCopyCannotDisturbAnother(t *testing.T) {
	tpl := New(writeFile("schema"))
	first, second := tpl.Path(t), tpl.Path(t)

	if err := os.WriteFile(first, []byte("scribbled on"), 0o600); err != nil {
		t.Fatalf("writing to the first copy: %v", err)
	}
	got, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("reading the second copy: %v", err)
	}
	if string(got) != "schema" {
		t.Fatalf("the second copy became %q; a test must not see another test's writes", got)
	}
	// And the template itself is untouched, so the next test still gets
	// a clean one.
	third, err := os.ReadFile(tpl.Path(t))
	if err != nil {
		t.Fatalf("reading a third copy: %v", err)
	}
	if string(got) != string(third) {
		t.Fatalf("a later copy is %q, want the original schema", third)
	}
}

func TestAFailedMigrationIsReported(t *testing.T) {
	boom := errors.New("migration 0039 exploded")
	tpl := New(func(string) error { return boom })

	_, err := tpl.build()
	if !errors.Is(err, boom) {
		t.Fatalf("build error is %v, want it to carry %v — a template that silently built empty would hand every test a database with no schema", err, boom)
	}
}

func TestALeftOverWriteAheadLogIsRefused(t *testing.T) {
	// A migrate function that forgets to close leaves the committed
	// pages in the -wal. Copying the main file alone would hand every
	// test a database missing them, and the failures would land far
	// from the cause.
	tpl := New(func(path string) error {
		if err := os.WriteFile(path, []byte("schema"), 0o600); err != nil {
			return err
		}
		return os.WriteFile(path+"-wal", []byte("uncheckpointed"), 0o600)
	})

	_, err := tpl.build()
	if err == nil {
		t.Fatal("build accepted a template with a write-ahead log beside it")
	}
	if !strings.Contains(err.Error(), "write-ahead log") {
		t.Fatalf("error is %q, want it to name the write-ahead log", err)
	}
}

func TestIntoLeavesAnExistingDatabaseAlone(t *testing.T) {
	tpl := New(writeFile("schema"))
	path := tpl.Path(t)
	if err := os.WriteFile(path, []byte("written by the first open"), 0o600); err != nil {
		t.Fatalf("writing to the copy: %v", err)
	}

	if got := tpl.Into(t, path); got != path {
		t.Fatalf("Into returned %q, want the path it was given", got)
	}
	back, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if string(back) != "written by the first open" {
		t.Fatalf("reopening the same path found %q; the second open must see the first one's writes", back)
	}
}
