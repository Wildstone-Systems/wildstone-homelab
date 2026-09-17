package portal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func store(t *testing.T) *FileStore {
	t.Helper()
	return &FileStore{Root: t.TempDir()}
}

func save(t *testing.T, s *FileStore, ns, name, body string) Stored {
	t.Helper()
	got, err := s.Save(ns, name, strings.NewReader(body))
	if err != nil {
		t.Fatalf("Save(%q, %q): %v", ns, name, err)
	}
	return got
}

// The MVP acceptance criterion, as a test: what a phone drops on the
// dashboard is a real file, under the administrator's root, that
// another application reading that root can see.
func TestAnUploadIsAFileOnDisk(t *testing.T) {
	s := store(t)
	got := save(t, s, "shared", "IMG_4821.jpg", "pretend-jpeg")

	onDisk := filepath.Join(s.Root, "Shared", got.Name)
	body, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("the Files application would not find it: %v", err)
	}
	if string(body) != "pretend-jpeg" {
		t.Errorf("stored %q, want the bytes that were uploaded", body)
	}
	if got.Size != int64(len("pretend-jpeg")) {
		t.Errorf("reported size %d, want %d", got.Size, len("pretend-jpeg"))
	}
}

// #197: "`../`, absolute paths, encoded traversal and symlink/path
// escape attempts cannot leave the configured logical namespace."
//
// Each of these is a filename a caller controls entirely. None of them
// may produce a file outside the namespace directory — and the check is
// deliberately "nothing appeared anywhere else", not "the call failed",
// because a normalisation that quietly wrote ../etc/passwd as a file
// called etc_passwd is correct and one that wrote it as ../etc/passwd
// is not.
func TestAnUploadCannotLeaveItsNamespace(t *testing.T) {
	for _, offered := range []string{
		"../escaped.txt",
		"../../../../etc/cron.d/wildstone",
		"/etc/shadow",
		`..\..\windows\system32\drivers\etc\hosts`,
		"%2e%2e%2fescaped.txt",
		"....//escaped.txt",
		".",
		"..",
		"",
		"\x00escaped.txt",
	} {
		t.Run(offered, func(t *testing.T) {
			s := store(t)
			got := save(t, s, "shared", offered, "x")

			inside := filepath.Join(s.Root, "Shared", got.Name)
			if _, err := os.Stat(inside); err != nil {
				t.Fatalf("%q stored as %q, which is not in the namespace: %v", offered, got.Name, err)
			}
			// The whole root, walked: the only regular file anywhere
			// under it must be the one inside Shared.
			var found []string
			if err := filepath.WalkDir(s.Root, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() {
					found = append(found, path)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(found) != 1 || found[0] != inside {
				t.Errorf("%q produced %v, want exactly [%s]", offered, found, inside)
			}
			if strings.ContainsAny(got.Name, `/\`) || got.Name == "." || got.Name == ".." {
				t.Errorf("stored name %q is not a single safe segment", got.Name)
			}
		})
	}
}

// A symlink planted in the destination is the traversal that survives
// every amount of string cleaning: the NAME is innocent and the
// filesystem does the escaping. os.Root is what refuses it.
func TestAnUploadDoesNotFollowASymlinkOutOfTheRoot(t *testing.T) {
	s := store(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "captured.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.Root, "Shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(s.Root, "Shared", "captured.txt")); err != nil {
		t.Fatal(err)
	}

	// The upload names the symlink. Whatever happens, the file outside
	// the root must not be the one that changes.
	if _, err := s.Save("shared", "captured.txt", strings.NewReader("overwritten")); err != nil {
		t.Logf("refused, which is one acceptable answer: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "original" {
		t.Errorf("an upload wrote through a symlink and changed %s", target)
	}
}

// Two phones, one filename. Neither person loses a photo.
func TestASecondUploadOfTheSameNameKeepsBoth(t *testing.T) {
	s := store(t)
	first := save(t, s, "shared", "IMG_0001.jpg", "one")
	second := save(t, s, "shared", "IMG_0001.jpg", "two")

	if second.Name == first.Name {
		t.Fatalf("both landed as %q — the second overwrote the first", first.Name)
	}
	if !second.Renamed {
		t.Errorf("stored as %q without reporting the rename; the person is not told they now have two", second.Name)
	}
	if filepath.Ext(second.Name) != ".jpg" {
		t.Errorf("collision name %q lost its extension, so it no longer opens", second.Name)
	}
	body, err := os.ReadFile(filepath.Join(s.Root, "Shared", first.Name))
	if err != nil || string(body) != "one" {
		t.Errorf("the first upload is %q, %v — it should be untouched", body, err)
	}
}

func TestAnUploadIsBounded(t *testing.T) {
	s := &FileStore{Root: t.TempDir(), MaxUpload: 16}

	if _, err := s.Save("shared", "small.bin", strings.NewReader(strings.Repeat("x", 16))); err != nil {
		t.Errorf("a file of exactly the limit was refused: %v", err)
	}
	_, err := s.Save("shared", "big.bin", strings.NewReader(strings.Repeat("x", 17)))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Save over the limit returned %v, want ErrTooLarge", err)
	}
	// The refusal must not leave the partial write behind.
	entries, err := os.ReadDir(filepath.Join(s.Root, "Shared"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "small.bin" {
			t.Errorf("a refused upload left %q behind", e.Name())
		}
	}
}

// A destination that is not in the table is refused rather than
// coerced to the default: an upload that lands somewhere other than
// where the person was told is worse than one that fails.
func TestAnUnknownNamespaceIsRefused(t *testing.T) {
	s := store(t)
	for _, ns := range []string{"etc", "../shared", "Shared", "me"} {
		if _, err := s.Save(ns, "x.txt", strings.NewReader("x")); !errors.Is(err, ErrNamespace) {
			t.Errorf("Save to %q returned %v, want ErrNamespace", ns, err)
		}
	}
	if _, err := s.Save("", "x.txt", strings.NewReader("x")); err != nil {
		t.Errorf("an upload naming no destination should land in %s: %v", DefaultNamespace, err)
	}
}

// A store with no root answers "not configured" rather than writing to
// a relative path beside whatever the process's working directory is.
func TestAStoreWithNoRootRefusesEverything(t *testing.T) {
	for _, root := range []string{"", "relative/path"} {
		s := &FileStore{Root: root}
		if s.Ready() {
			t.Errorf("Ready() with root %q", root)
		}
		if _, err := s.Save("shared", "x.txt", strings.NewReader("x")); !errors.Is(err, ErrNoRoot) {
			t.Errorf("Save with root %q returned %v, want ErrNoRoot", root, err)
		}
		if _, err := s.Recent(5); !errors.Is(err, ErrNoRoot) {
			t.Errorf("Recent with root %q returned %v, want ErrNoRoot", root, err)
		}
	}
}

func TestRecentIsNewestFirstAcrossNamespaces(t *testing.T) {
	s := store(t)
	old := save(t, s, "shared", "old.txt", "old")
	mid := save(t, s, "photos", "mid.jpg", "mid")
	recent := save(t, s, "shared", "new.txt", "new")

	// Modification times are what "recent" means, and a test that wrote
	// three files in one millisecond would be asserting the tiebreak
	// rather than the order.
	stamp(t, s, "Shared", old.Name, -3)
	stamp(t, s, "Photos", mid.Name, -2)
	stamp(t, s, "Shared", recent.Name, -1)

	got, err := s.Recent(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Recent(2) returned %d items", len(got))
	}
	if got[0].Name != recent.Name || got[1].Name != mid.Name {
		t.Errorf("Recent returned %s, %s; want %s, %s", got[0].Name, got[1].Name, recent.Name, mid.Name)
	}
	if got[1].Namespace != "photos" {
		t.Errorf("item from Photos reported namespace %q", got[1].Namespace)
	}
}

// A half-arrived upload must not appear in a listing, and neither must
// anything a person hid with a leading dot.
func TestListingSkipsScratchFilesAndDirectories(t *testing.T) {
	s := store(t)
	save(t, s, "shared", "real.txt", "real")
	dir := filepath.Join(s.Root, "Shared")
	if err := os.WriteFile(filepath.Join(dir, ".wildstone-upload-abc123"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "a-folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := s.List("shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "real.txt" {
		t.Errorf("List returned %v, want only real.txt", got)
	}
}

// Reading back is bounded by the same rule the write path applied: a
// name that could not have been created here cannot be read here.
func TestOpenRefusesNamesTheWritePathWouldNeverProduce(t *testing.T) {
	s := store(t)
	save(t, s, "shared", "real.txt", "real")

	f, _, err := s.Open("shared", "real.txt")
	if err != nil {
		t.Fatalf("Open of a stored file: %v", err)
	}
	f.Close()

	for _, name := range []string{"../../etc/passwd", "..", ".", "", ".hidden", "sub/real.txt"} {
		if _, _, err := s.Open("shared", name); !errors.Is(err, ErrNotFound) {
			t.Errorf("Open(%q) returned %v, want ErrNotFound", name, err)
		}
	}
}

func TestSafeNameKeepsNamesAPersonWouldRecognise(t *testing.T) {
	for offered, want := range map[string]string{
		"IMG_4821.jpg":         "IMG_4821.jpg",
		"Rent receipt (2).pdf": "Rent receipt (2).pdf",
		"Sam's list.txt":       "Sam's list.txt",
		"vacances été.jpeg":    "vacances été.jpeg",
		"../../etc/passwd":     "passwd",
		"report;rm -rf ~.docx": "report_rm -rf _.docx",
		".hidden":              "hidden",
		"  spaced  .txt  ":     "spaced  .txt",
	} {
		if got := SafeName(offered); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", offered, got, want)
		}
	}
	long := strings.Repeat("a", 400) + ".jpg"
	got := SafeName(long)
	if len([]rune(got)) != MaxFilename {
		t.Errorf("SafeName of a 400-character name is %d characters, want %d", len([]rune(got)), MaxFilename)
	}
	if filepath.Ext(got) != ".jpg" {
		t.Errorf("truncation lost the extension: %q", got)
	}
}

// stamp moves a stored file's modification time by whole seconds, so an
// ordering test asserts the ordering rather than the clock.
func stamp(t *testing.T, s *FileStore, dir, name string, secondsAgo int) {
	t.Helper()
	path := filepath.Join(s.Root, dir, name)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	when := info.ModTime().Add(time.Duration(secondsAgo) * time.Second)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}
