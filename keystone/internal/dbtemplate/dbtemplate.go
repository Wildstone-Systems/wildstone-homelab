// Package dbtemplate hands tests a runtime database that has already
// been migrated.
//
// Why it exists. Every test that wants a database opens an empty file
// and replays the whole migration history into it — thirty-nine files
// at the time of writing, several of which rebuild a table by copying
// it. That is a fixed cost per test, it grows with every migration we
// add, and `keystone/api` pays it 184 times in one binary. Under
// `-race` on a slow CI runner the arithmetic stopped working: the suite
// ran out of the default per-binary clock without a single test being
// slow (issue #231).
//
// What it does instead: run the migrations ONCE per test binary, keep
// the resulting file in memory, and give each test its own byte-for-byte
// copy. Every test still gets a private, writable, fully migrated
// database on disk — the isolation is unchanged, only the arithmetic is.
//
// What it deliberately does NOT do is stand in for a migration test. A
// test that exists to prove a migration behaves correctly must still
// open an empty database and run the real thing; a copy of the finished
// article proves nothing about how it got there.
package dbtemplate

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Template is one migrated database, built on first use and copied
// thereafter. Declare one as a package-level variable in the test file
// that needs it, so the build is shared across the binary's tests.
type Template struct {
	migrate func(path string) error

	once sync.Once
	data []byte
	err  error
}

// New returns a Template whose file is produced by migrate.
//
// migrate is handed a path that does not exist yet and must open it,
// bring it fully up to date, and CLOSE it again. Closing is not
// housekeeping: the database runs in WAL mode, and the committed pages
// only land in the main file when the last connection goes away. Build
// refuses to copy a file that still has a write-ahead log beside it
// rather than hand out a database missing its most recent writes.
func New(migrate func(path string) error) *Template {
	return &Template{migrate: migrate}
}

// Path writes a fresh copy of the template into t's own temp directory
// and returns its path. The caller opens it as it would any database.
func (tpl *Template) Path(t *testing.T) string {
	t.Helper()
	return tpl.Into(t, filepath.Join(t.TempDir(), "keystone.db"))
}

// Into is Path at a path the caller chose, and returns it.
//
// Nothing is written when a file is already there. Tests that open one
// database twice — a second reader, a handover to another coordinator —
// pass the same path twice, and the second open must find what the
// first one wrote rather than an empty schema dropped on top of it.
func (tpl *Template) Into(t *testing.T, path string) string {
	t.Helper()
	tpl.once.Do(func() { tpl.data, tpl.err = tpl.build() })
	if tpl.err != nil {
		t.Fatalf("dbtemplate: %v", tpl.err)
	}
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if err := os.WriteFile(path, tpl.data, 0o600); err != nil {
		t.Fatalf("dbtemplate: writing a copy: %v", err)
	}
	return path
}

// build migrates one database and reads it into memory. The scratch
// directory is removed before build returns — the template lives as
// bytes so that nothing has to own a directory for the lifetime of the
// test binary, which is a thing Go tests have no hook for.
func (tpl *Template) build() ([]byte, error) {
	dir, err := os.MkdirTemp("", "wildstone-dbtemplate-")
	if err != nil {
		return nil, fmt.Errorf("scratch directory: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "template.db")
	if err := tpl.migrate(path); err != nil {
		return nil, fmt.Errorf("migrating the template: %w", err)
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		return nil, fmt.Errorf("migrating the template left a write-ahead log beside it; the migrate function must close the database, or the copies will be missing its most recent writes")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the template: %w", err)
	}
	return data, nil
}
