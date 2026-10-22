package portal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRelativePathRefusesInsteadOfCleaning(t *testing.T) {
	for _, raw := range []string{"/etc/passwd", ".", "..", "a/../b", "a/./b", "a//b", "a/", "a\\b", "a\x00b", "a\nb", ".hidden", "a/.hidden", strings.Repeat("x", 256), strings.Repeat("a/", 32) + "b", strings.Repeat("long/", 1000), string([]byte{0xff})} {
		if _, err := ParseRelativePath(raw); !errors.Is(err, ErrPath) {
			t.Errorf("accepted %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"", "Family Photos", "Family Photos/été #1.jpg", "a/constructor", "__proto__"} {
		p, err := ParseRelativePath(raw)
		if err != nil || string(p) != raw {
			t.Errorf("valid %q: %v", raw, err)
		}
	}
}

func TestFileTreeLifecycle(t *testing.T) {
	for _, ns := range []string{"shared", "me"} {
		t.Run(ns, func(t *testing.T) {
			s := store(t)
			s.Member = "sam"
			for _, p := range []string{"Trips", "Trips/2026", "Archive"} {
				if err := s.Mkdir(ns, p); err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.SaveAt(ns, "Trips/2026", "été.txt", strings.NewReader("summer"))
			if err != nil {
				t.Fatal(err)
			}
			if result.Path != "Trips/2026/été.txt" || result.Type != "file" {
				t.Fatal(result)
			}
			files, err := s.ListAt(ns, "Trips")
			if err != nil || len(files) != 1 || files[0].Type != "directory" {
				t.Fatalf("%+v %v", files, err)
			}
			f, _, err := s.OpenAt(ns, result.Path)
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(f)
			f.Close()
			if err != nil || string(b) != "summer" {
				t.Fatal(string(b), err)
			}
			if err := s.DeleteAt(ns, "Trips"); err == nil {
				t.Fatal("deleted nonempty folder")
			}
			if err := s.RenameAt(ns, result.Path, "notes.txt"); err != nil {
				t.Fatal(err)
			}
			if err := s.Move(ns, "Trips/2026", "Archive/2026"); err != nil {
				t.Fatal(err)
			}
			if err := s.Move(ns, "Archive", "Archive/2026/recursive"); err == nil {
				t.Fatal("moved into itself")
			}
			if err := s.DeleteAt(ns, "Archive/2026/notes.txt"); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteAt(ns, "Archive/2026"); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteAt(ns, ""); err == nil {
				t.Fatal("deleted namespace root")
			}

		})
	}
}

func TestTreeKeepsPhotosFlat(t *testing.T) {
	s := store(t)
	s.Member = "sam"
	for _, ns := range []string{"photos"} {
		if err := s.Mkdir(ns, "folder"); err == nil {
			t.Fatal("mkdir", ns)
		}
		if _, err := s.SaveAt(ns, "folder", "x", strings.NewReader("x")); err == nil {
			t.Fatal("upload", ns)
		}
		if _, err := s.ListAt(ns, "folder"); err == nil {
			t.Fatal("list", ns)
		}
		if f, _, err := s.OpenAt(ns, "folder/x"); err == nil {
			f.Close()
			t.Fatal("open", ns)
		}
		if err := s.Move(ns, "a", "b"); err == nil {
			t.Fatal("move", ns)
		}
	}
}

func TestTreeSymlinksAreInvisibleAndInoperable(t *testing.T) {
	s := store(t)
	if err := s.Mkdir("shared", "folder"); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{outside, "outside"}, {"folder", "inside"}, {filepath.Join(outside, "secret"), "leaf"}} {
		if err := os.Symlink(pair[0], filepath.Join(s.Root, "Shared", pair[1])); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.ListAt("shared", "")
	if err != nil || len(entries) != 1 {
		t.Fatalf("%v %v", entries, err)
	}
	for _, p := range []string{"outside/secret", "inside/new", "leaf"} {
		if f, _, err := s.OpenAt("shared", p); err == nil {
			f.Close()
			t.Fatal("opened", p)
		}
		if err := s.DeleteAt("shared", p); err == nil {
			t.Fatal("deleted", p)
		}
		if err := s.Move("shared", p, "moved"); err == nil {
			t.Fatal("moved", p)
		}
	}
	for _, dir := range []string{"outside", "inside"} {
		if _, err := s.ListAt("shared", dir); err == nil {
			t.Fatal("listed", dir)
		}
		if _, err := s.SaveAt("shared", dir, "new", strings.NewReader("x")); err == nil {
			t.Fatal("uploaded", dir)
		}
		if err := s.Mkdir("shared", dir+"/new"); err == nil {
			t.Fatal("mkdir", dir)
		}
	}
	b, err := os.ReadFile(filepath.Join(outside, "secret"))
	if err != nil || string(b) != "keep" {
		t.Fatal("outside changed")
	}
}

func TestTreeUploadRevokedDuringStreamLeavesNoFile(t *testing.T) {
	s := store(t)
	if err := s.Mkdir("shared", "folder"); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("revoked")
	revoked := false
	s.BeforeWrite = func() error {
		if revoked {
			return denied
		}
		return nil
	}
	_, err := s.SaveAt("shared", "folder", "secret", fenceDuringUpload{reader: strings.NewReader("private"), stop: func() { revoked = true }})
	if !errors.Is(err, denied) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(s.Root, "Shared", "folder"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("%v %v", entries, err)
	}
}

func TestTreeMutationsRecheckAuthority(t *testing.T) {
	for _, op := range []string{"mkdir", "move", "rename", "delete", "upload"} {
		t.Run(op, func(t *testing.T) {
			s := store(t)
			save(t, s, "shared", "original", "keep")
			denied := errors.New("revoked")
			calls := 0
			s.BeforeWrite = func() error {
				calls++
				if calls >= 2 {
					return denied
				}
				return nil
			}
			var err error
			switch op {
			case "mkdir":
				err = s.Mkdir("shared", "folder")
			case "move":
				err = s.Move("shared", "original", "moved")
			case "rename":
				err = s.RenameAt("shared", "original", "renamed")
			case "delete":
				err = s.DeleteAt("shared", "original")
			case "upload":
				_, err = s.SaveAt("shared", "", "new", strings.NewReader("new"))
			}
			if !errors.Is(err, denied) {
				t.Fatal(err)
			}
			files, err := os.ReadDir(filepath.Join(s.Root, "Shared"))
			if err != nil || len(files) != 1 || files[0].Name() != "original" {
				t.Fatalf("%v %v", files, err)
			}
		})
	}
}

func TestTreeMkdirRevokedAfterCreationRemovesDirectory(t *testing.T) {
	s := store(t)
	denied := errors.New("revoked")
	calls := 0
	s.BeforeWrite = func() error {
		calls++
		if calls >= 3 {
			return denied
		}
		return nil
	}

	err := s.Mkdir("shared", "folder")
	if !errors.Is(err, denied) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "Shared", "folder")); !os.IsNotExist(err) {
		t.Fatal("refused directory remains", err)
	}
}

func TestConcurrentTreeOperationsNeverReplace(t *testing.T) {
	for _, op := range []string{"upload", "mkdir", "move"} {
		t.Run(op, func(t *testing.T) {
			s := store(t)
			if _, err := s.List("shared"); err != nil {
				t.Fatal(err)
			}
			const n = 12
			for i := 0; i < n; i++ {
				save(t, s, "shared", fmt.Sprintf("source%d", i), fmt.Sprint(i))
			}
			errs := make([]error, n)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					switch op {
					case "upload":
						_, errs[i] = s.SaveAt("shared", "", "target", strings.NewReader(fmt.Sprint(i)))
					case "mkdir":
						errs[i] = s.Mkdir("shared", "target")
					case "move":
						errs[i] = s.Move("shared", fmt.Sprintf("source%d", i), "target")
					}
				}(i)
			}
			close(start)
			wg.Wait()
			winner := -1
			for i, err := range errs {
				if err == nil {
					if winner >= 0 {
						t.Fatal("multiple winners")
					}
					winner = i
				} else if !errors.Is(err, os.ErrExist) {
					t.Fatal(err)
				}
			}
			if winner < 0 {
				t.Fatal("no winner")
			}
			if op != "mkdir" {
				b, err := os.ReadFile(filepath.Join(s.Root, "Shared", "target"))
				if err != nil || string(b) != fmt.Sprint(winner) {
					t.Fatalf("winner overwritten: %q %v", b, err)
				}
			}
			if op == "move" {
				for i := 0; i < n; i++ {
					if i != winner {
						if _, err := os.Stat(filepath.Join(s.Root, "Shared", fmt.Sprintf("source%d", i))); err != nil {
							t.Fatal("loser lost its file", err)
						}
					}
				}
			}
		})
	}
}

func TestPinnedParentCannotBeReplacedBySymlinkDuringUpload(t *testing.T) {
	s := store(t)
	if err := s.Mkdir("shared", "folder"); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	swapped := false
	_, err := s.SaveAt("shared", "folder", "file", fenceDuringUpload{reader: strings.NewReader("keep"), stop: func() {
		if swapped {
			return
		}
		swapped = true
		if err := os.Rename(filepath.Join(s.Root, "Shared", "folder"), filepath.Join(s.Root, "Shared", "held")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(s.Root, "Shared", "folder")); err != nil {
			t.Fatal(err)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("escaped root: %v %v", entries, err)
	}
	b, err := os.ReadFile(filepath.Join(s.Root, "Shared", "held", "file"))
	if err != nil || string(b) != "keep" {
		t.Fatal(string(b), err)
	}
}

func TestRequiredMountNeverCreatesLocalReplacement(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(fmt.Sprint(exists), func(t *testing.T) {
			mount := filepath.Join(t.TempDir(), "not-mounted")
			if exists {
				if err := os.Mkdir(mount, 0755); err != nil {
					t.Fatal(err)
				}
			}
			s := &FileStore{Root: filepath.Join(mount, "files"), RequiredMount: mount, BeforeWrite: func() error { return nil }}
			if _, err := s.ListAt("shared", ""); !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
			if _, err := s.SaveAt("shared", "", "file", strings.NewReader("x")); !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
			if _, err := s.Save("shared", "file", strings.NewReader("x")); !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
			if _, err := os.Stat(s.Root); !os.IsNotExist(err) {
				t.Fatal("created local replacement", err)
			}
		})
	}
	if !mountPresent(strings.NewReader("42 20 0:1 / /srv/family\\040files rw - nfs host:/export rw\n"), "/srv/family files") {
		t.Fatal("escaped mountpoint not recognised")
	}
}

func TestMixedLegacyAndTreeWritersCannotOverwriteEachOther(t *testing.T) {
	s := store(t)
	const n = 16
	for i := 0; i < n; i++ {
		save(t, s, "shared", fmt.Sprintf("source%d", i), fmt.Sprint(i))
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			switch i % 4 {
			case 0:
				_, errs[i] = s.Rename("shared", fmt.Sprintf("source%d", i), "target")
			case 1:
				errs[i] = s.Move("shared", fmt.Sprintf("source%d", i), "target")
			case 2:
				_, errs[i] = s.SaveAt("shared", "", "target", strings.NewReader(fmt.Sprint(i)))
			case 3:
				errs[i] = s.Mkdir("shared", "target")
			}
		}(i)
	}
	close(start)
	wg.Wait()
	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner >= 0 {
				t.Fatal("multiple winners")
			}
			winner = i
		} else if !errors.Is(err, os.ErrExist) {
			t.Fatal(err)
		}
	}
	if winner < 0 {
		t.Fatal("no winner")
	}
	if winner%4 == 3 {
		info, err := os.Stat(filepath.Join(s.Root, "Shared", "target"))
		if err != nil || !info.IsDir() {
			t.Fatal("winning folder replaced", err)
		}
	} else {
		b, err := os.ReadFile(filepath.Join(s.Root, "Shared", "target"))
		if err != nil || string(b) != fmt.Sprint(winner) {
			t.Fatal("winning file replaced", string(b), err)
		}
	}
	for i := 0; i < n; i++ {
		if i != winner {
			if _, err := os.Stat(filepath.Join(s.Root, "Shared", fmt.Sprintf("source%d", i))); err != nil {
				t.Fatal("losing source removed", err)
			}
		}
	}
}
