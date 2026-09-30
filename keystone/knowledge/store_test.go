package knowledge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	return &Store{Root: dir, Now: func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }}, dir
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestANoteWrittenCanBeReadBack(t *testing.T) {
	s, dir := newStore(t)
	note, err := s.Create("projects/garden", "# Garden\n\nPlant garlic in October.\n")
	if err != nil {
		t.Fatal(err)
	}
	if note.Path != "projects/garden.md" {
		t.Fatalf("path = %q, want the .md extension added", note.Path)
	}
	content, read, err := s.Read("projects/garden.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# Garden\n\nPlant garlic in October.\n" || read.Revision != note.Revision {
		t.Fatalf("read back %q at %s, wrote revision %s", content, read.Revision, note.Revision)
	}
	// Nothing but the note is left behind: no staging file.
	entries, _ := os.ReadDir(filepath.Join(dir, "projects"))
	if len(entries) != 1 {
		t.Fatalf("projects/ holds %d entries, want only the note", len(entries))
	}
}

func TestPublishedNotesAreReadableByAWebUIDespiteTheUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	s, dir := newStore(t)
	if _, err := s.Create("a/b/note", "hello"); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]os.FileMode{"a": dirMode, "a/b": dirMode, "a/b/note.md": fileMode} {
		fi, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", rel, fi.Mode().Perm(), want)
		}
	}
}

func TestCreatingNeverOverwrites(t *testing.T) {
	s, dir := newStore(t)
	write(t, dir, "people/alex.md", "a person wrote this")
	if _, err := s.Create("people/alex", "an agent's version"); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "people/alex.md"))
	if string(got) != "a person wrote this" {
		t.Fatalf("the existing note was changed to %q", got)
	}
}

func TestPathsOutsideTheFolderAreRefusedNotCleaned(t *testing.T) {
	s, _ := newStore(t)
	for _, bad := range []string{
		"../escape", "/etc/passwd", "a/../b", "./a", ".hidden", "a/.git/config",
		`a\b`, "a//b", "", "   ", ".md", "a/\x01b",
	} {
		if _, err := s.Create(bad, "x"); err == nil {
			t.Errorf("Create(%q) succeeded", bad)
		}
		if _, _, err := s.Read(bad); err == nil {
			t.Errorf("Read(%q) succeeded", bad)
		}
	}
	if _, err := s.List("../"); err == nil {
		t.Error("List(\"../\") succeeded")
	}
}

func TestSymlinksAreNeitherFollowedNorWrittenThrough(t *testing.T) {
	s, dir := newStore(t)
	outside := t.TempDir()
	write(t, outside, "secret.md", "not a household note")
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(dir, "leaf.md")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "real.md", "inside")
	if err := os.Symlink("real.md", filepath.Join(dir, "alias.md")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"linked/secret.md", "leaf.md", "alias.md"} {
		if _, _, err := s.Read(p); err == nil {
			t.Errorf("Read(%q) followed a symlink", p)
		}
	}
	if _, err := s.Create("linked/new", "x"); err == nil {
		t.Error("Create wrote through a symlinked folder")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.md")); err == nil {
		t.Fatal("a file was created outside the knowledge folder")
	}
	if _, err := s.List("linked"); err == nil {
		t.Error("List followed a symlinked folder")
	}
	hits, _, err := s.Search("household")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("search reached through a symlink: %+v", hits)
	}
}

func TestAgentsCannotWriteWhatANoteAppRuns(t *testing.T) {
	s, _ := newStore(t)
	for _, p := range []string{"CONTAINER_BOOT", "config.md", "SETTINGS", "PLUGS", "_plug/x", "notes/_hidden/y"} {
		if _, err := s.Create(p, "plain"); !errors.Is(err, ErrReserved) {
			t.Errorf("Create(%q) = %v, want ErrReserved", p, err)
		}
	}
	// A nested file of the same name is an ordinary note.
	if _, err := s.Create("projects/config", "plain"); err != nil {
		t.Errorf("projects/config.md was refused: %v", err)
	}
	for _, content := range []string{
		"```space-lua\nprint(1)\n```",
		"  ~~~ Space-Script\nx\n~~~",
		"```space-style\nbody{}\n```",
		"> ```space-lua\n> print(1)\n> ```",
		"> > ```SPACE-LUA\n> > x\n> > ```",
		"- ```space-lua\n  x\n  ```",
		"1. ~~~~ space-style\n   x\n   ~~~~",
		"Total: ${1+1}",
		"nul\x00byte",
		string([]byte{0xff, 0xfe}),
		strings.Repeat("a", MaxWriteBytes+1),
	} {
		if _, err := s.Create("n", content); err == nil {
			t.Errorf("content %q was accepted", content[:min(len(content), 30)])
		}
	}
	// Ordinary code blocks are fine.
	if _, err := s.Create("ok", "```go\nfmt.Println(1)\n```\n$5 and {braces}"); err != nil {
		t.Errorf("an ordinary code block was refused: %v", err)
	}
}

// A refusal reaches the operator's audit stream, so it must not quote
// the text it refused.
func TestARefusalDoesNotQuoteTheNote(t *testing.T) {
	err := CheckContent("> ```space-privatephrase\n> x\n> ```")
	if !errors.Is(err, ErrContent) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "privatephrase") {
		t.Fatalf("the refusal quotes the note: %v", err)
	}
}

func TestAnUpdateIsRefusedWhenThePersonSavedFirst(t *testing.T) {
	s, dir := newStore(t)
	note, err := s.Create("decisions/roof", "v1")
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "decisions/roof.md", "a person's edit")
	_, err = s.Update("decisions/roof", "agent's v2", note.Revision)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want a ConflictError", err)
	}
	if conflict.Current != Revision([]byte("a person's edit")) {
		t.Fatalf("conflict names %s, not the current revision", conflict.Current)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "decisions/roof.md"))
	if string(got) != "a person's edit" {
		t.Fatalf("the person's edit was overwritten with %q", got)
	}
	if _, err := s.Update("decisions/roof", "x", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("an update naming no revision was not refused: %v", err)
	}
}

func TestAnUpdateKeepsWhatItReplaced(t *testing.T) {
	s, dir := newStore(t)
	note, err := s.Create("decisions/roof", "v1")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.Update("decisions/roof.md", "v2", note.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != Revision([]byte("v2")) {
		t.Fatalf("revision = %s", updated.Revision)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "decisions/roof.md"))
	if string(got) != "v2" {
		t.Fatalf("note = %q", got)
	}
	kept, _ := filepath.Glob(filepath.Join(dir, HistoryDir, "decisions", "roof", "*.md"))
	if len(kept) != 1 {
		t.Fatalf("history holds %v, want one previous version", kept)
	}
	old, _ := os.ReadFile(kept[0])
	if string(old) != "v1" {
		t.Fatalf("history kept %q, want v1", old)
	}
	// History is invisible to the tools.
	if _, _, err := s.Read(HistoryDir + "/decisions/roof"); err == nil {
		t.Error("the history folder was readable through the store")
	}
	entries, _ := s.List("")
	for _, e := range entries {
		if strings.HasPrefix(e.Path, ".") {
			t.Errorf("List showed %q", e.Path)
		}
	}
}

func TestAppendKeepsWhatWasThere(t *testing.T) {
	s, dir := newStore(t)
	write(t, dir, "log.md", "# Log\n```space-lua\n-- a person's own script\n```")
	if _, err := s.Append("log", "Boiler serviced."); err != nil {
		t.Fatalf("appending beside a person's own script: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "log.md"))
	want := "# Log\n```space-lua\n-- a person's own script\n```\n\nBoiler serviced.\n"
	if string(got) != want {
		t.Fatalf("note = %q, want %q", got, want)
	}
	if _, err := s.Append("log", "${evil()}"); !errors.Is(err, ErrContent) {
		t.Fatalf("appended active content: %v", err)
	}
	if _, err := s.Append("missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("append created a note: %v", err)
	}
	if _, err := s.Append("CONFIG", "x"); !errors.Is(err, ErrReserved) {
		t.Fatalf("appended to a reserved file: %v", err)
	}
}

func TestUpdatingANoteThatIsNotThereIsNotACreate(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Update("missing", "x", "abc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestTheExecutionFenceStopsWrites(t *testing.T) {
	s, dir := newStore(t)
	stopped := errors.New("household execution is paused")
	s.BeforeWrite = func() error { return stopped }
	if _, err := s.Create("n", "x"); !errors.Is(err, stopped) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "n.md")); err == nil {
		t.Fatal("a fenced write published")
	}
}

func TestListShowsFoldersThenNotes(t *testing.T) {
	s, dir := newStore(t)
	write(t, dir, "b.md", "b")
	write(t, dir, "a.md", "a")
	write(t, dir, "photo.jpg", "not a note")
	write(t, dir, ".hidden.md", "hidden")
	write(t, dir, "projects/x.md", "x")
	entries, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Path)
	}
	if strings.Join(got, ",") != "projects,a.md,b.md" {
		t.Fatalf("listed %v", got)
	}
	if _, err := s.List("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("listing a missing folder: %v", err)
	}
}

func TestSearchMatchesPathsThenContent(t *testing.T) {
	s, dir := newStore(t)
	write(t, dir, "garden/plan.md", "Tomatoes along the south fence.")
	write(t, dir, "shopping.md", "Buy GARDEN twine and tomato feed.")
	write(t, dir, ".history/garden/old.md", "garden")
	hits, truncated, err := s.Search("garden")
	if err != nil || truncated {
		t.Fatal(err, truncated)
	}
	if len(hits) != 2 || hits[0].Path != "garden/plan.md" || hits[1].Path != "shopping.md" {
		t.Fatalf("hits = %+v", hits)
	}
	if !strings.Contains(hits[1].Snippet, "GARDEN twine") {
		t.Fatalf("snippet = %q", hits[1].Snippet)
	}
	if _, _, err := s.Search("g"); !errors.Is(err, ErrQuery) {
		t.Fatalf("a one-character search ran: %v", err)
	}
}

func TestNoFolderConfiguredIsSaidPlainly(t *testing.T) {
	for _, s := range []*Store{{}, {Root: "relative"}} {
		if _, err := s.List(""); !errors.Is(err, ErrNoRoot) {
			t.Errorf("root %q: %v, want ErrNoRoot", s.Root, err)
		}
	}
}

func TestTheFolderIsCreatedOnFirstUse(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	root := filepath.Join(t.TempDir(), "knowledge")
	s := &Store{Root: root}
	if entries, err := s.List(""); err != nil || len(entries) != 0 {
		t.Fatalf("a fresh folder listed %v, %v", entries, err)
	}
	fi, err := os.Stat(root)
	if err != nil || fi.Mode().Perm() != dirMode {
		t.Fatalf("root = %v, %v", fi, err)
	}
}

func TestNotesTakeTheFolderOwnerSoAWebUICanSaveThem(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving a file away needs root, as Keystone runs")
	}
	s, dir := newStore(t)
	if err := os.Chown(dir, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	note, err := s.Create("a/n", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update("a/n", "v2", note.Revision); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"a", "a/n.md"} {
		fi, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if st.Uid != 1000 || st.Gid != 1000 {
			t.Errorf("%s is owned by %d:%d, want the folder's 1000:1000", rel, st.Uid, st.Gid)
		}
	}
}
