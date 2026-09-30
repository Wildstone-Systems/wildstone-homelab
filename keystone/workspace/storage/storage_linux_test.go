package storage

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
	"golang.org/x/sys/unix"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(workspace.Settings{Root: t.TempDir(), QuotaBytes: 1 << 30, MaxFileBytes: 1024, ScratchRetentionDays: 7, TrashRetentionDays: 30}, "person-one", "agent-one")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func stage(t *testing.T, s *Store, text string) Object {
	t.Helper()
	o, e := s.Stage("report.md", strings.NewReader(text))
	if e != nil {
		t.Fatal(e)
	}
	return o
}
func allow() error { return nil }
func filePath(s *Store, kind, id string) string {
	return filepath.Join(s.settings.Root, s.namespace, kind, id)
}
func TestStagePublishReadAndRestart(t *testing.T) {
	s := testStore(t)
	for _, ext := range []string{"md", "txt", "json", "csv"} {
		o, e := s.Stage("reports/data."+ext, strings.NewReader("héllo\n"))
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Read(o); e == nil {
			t.Fatal("staging exposed as revision")
		}
		// Reopen reconstructs no authority and loses no staged bytes.
		fresh, e := New(s.settings, "person-one", "agent-one")
		if e != nil {
			t.Fatal(e)
		}
		if e = fresh.Publish(o, allow); e != nil {
			t.Fatal(e)
		}
		b, e := fresh.Read(o)
		if e != nil || string(b) != "héllo\n" {
			t.Fatalf("%q %v", b, e)
		}
		info, e := os.Stat(filePath(s, "revisions", o.ID))
		if e != nil || info.Mode().Perm() != 0400 {
			t.Fatalf("mode %v %v", info, e)
		}
		if e = s.Publish(o, allow); !errors.Is(e, ErrConflict) {
			t.Fatal(e)
		}
	}
	empty := stage(t, s, "")
	if e := s.Publish(empty, allow); e != nil {
		t.Fatal(e)
	}
	other, e := New(s.settings, "person-two", "agent-one")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = other.Read(empty); e == nil {
		t.Fatal("other owner resolved bytes")
	}
	other, e = New(s.settings, "person-one", "agent-two")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = other.Read(empty); e == nil {
		t.Fatal("other agent resolved bytes")
	}
}
func TestPathBoundsAndInertText(t *testing.T) {
	bad := []string{"", "/x.md", "../x.md", "a/../x.md", "a//x.md", "a/./x.md", ".hidden/x.md", "a/.hidden.md", "a\\x.md", "a/\nx.md", string([]byte{255}) + ".md", strings.Repeat("a/", 32) + "x.md", strings.Repeat("x", 256) + ".md", strings.Repeat(strings.Repeat("x", 250)+"/", 17) + "x.md"}
	for _, p := range bad {
		if ValidatePath(p, false) == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	for _, p := range []string{"a.md", "目录/a.csv", strings.Repeat("x", 252) + ".md", strings.Repeat("a/", 31) + "x.txt"} {
		if e := ValidatePath(p, false); e != nil {
			t.Fatalf("%q: %v", p, e)
		}
	}
	if ValidatePath("", true) != nil {
		t.Fatal("directory root refused")
	}
	s := testStore(t)
	for _, p := range []string{"a.html", "a.sh", "a.MD", "a.pdf", "a"} {
		if _, e := s.Stage(p, strings.NewReader("text")); !errors.Is(e, ErrFormat) {
			t.Fatal(p, e)
		}
	}
	for _, b := range [][]byte{{0}, {255}, bytes.Repeat([]byte("x"), 1025)} {
		if _, e := s.Stage("x.txt", bytes.NewReader(b)); e == nil {
			t.Fatal("bad text accepted")
		}
	}
	if _, e := s.Stage("x.txt", nil); !errors.Is(e, ErrFormat) {
		t.Fatal(e)
	}
	if _, e := s.Stage("x.txt", failingReader{}); e == nil {
		t.Fatal("reader failure accepted")
	}
	if entries, e := os.ReadDir(s.settings.Root); e != nil || len(entries) != 0 {
		t.Fatal("validation had storage effects", entries, e)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestMissingStorageAndMountNeverCreateReplacement(t *testing.T) {
	s := testStore(t)
	s.settings.Root = filepath.Join(s.settings.Root, "missing")
	if _, e := s.Stage("a.md", strings.NewReader("a")); !errors.Is(e, workspace.ErrRoot) {
		t.Fatal(e)
	}
	if _, e := os.Stat(s.settings.Root); !os.IsNotExist(e) {
		t.Fatal("created missing root", e)
	}
	s = testStore(t)
	s.settings.RequiredMount = s.settings.Root
	if _, e := s.Stage("a.md", strings.NewReader("a")); !errors.Is(e, workspace.ErrMount) {
		t.Fatal(e)
	}
	entries, e := os.ReadDir(s.settings.Root)
	if e != nil || len(entries) != 0 {
		t.Fatal(entries, e)
	}
}
func TestSymlinkAndSpecialFileRefusal(t *testing.T) {
	for _, kind := range []string{"symlink-outside", "symlink-inside", "fifo", "directory", "socket"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			o := stage(t, s, "safe")
			p := filePath(s, "staging", o.ID)
			if e := os.Remove(p); e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "symlink-outside":
				target := filepath.Join(t.TempDir(), "outside")
				if e := os.WriteFile(target, []byte("safe"), 0600); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(target, p); e != nil {
					t.Fatal(e)
				}
			case "symlink-inside":
				if e := os.WriteFile(p+"-target", []byte("safe"), 0600); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(o.ID+"-target", p); e != nil {
					t.Fatal(e)
				}
			case "fifo":
				if e := unix.Mkfifo(p, 0600); e != nil {
					t.Fatal(e)
				}
			case "directory":
				if e := os.Mkdir(p, 0700); e != nil {
					t.Fatal(e)
				}
			case "socket":
				fd, e := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
				if e != nil {
					t.Fatal(e)
				}
				defer unix.Close(fd) // Long temp paths exceed sockaddr_un; a proc parent path is bounded.
				parent, e := os.Open(filepath.Dir(p))
				if e != nil {
					t.Fatal(e)
				}
				defer parent.Close()
				if e = unix.Bind(fd, &unix.SockaddrUnix{Name: filepath.Join("/proc/self/fd", fmtFD(parent), o.ID)}); e != nil {
					t.Fatal(e)
				}
			}
			if e := s.Publish(o, allow); e == nil {
				t.Fatal("unsafe inode published")
			}
		})
	}
}
func fmtFD(f *os.File) string { return fmt.Sprint(f.Fd()) }
func TestAncestorSymlinkSubstitution(t *testing.T) {
	s := testStore(t)
	outside := t.TempDir()
	first := strings.Split(s.namespace, "/")[0]
	s.hooks.beforeOpen = func() {
		s.hooks.beforeOpen = nil
		if e := os.Symlink(outside, filepath.Join(s.settings.Root, first)); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.Stage("x.md", strings.NewReader("safe")); e == nil {
		t.Fatal("followed substituted parent")
	}
	entries, e := os.ReadDir(outside)
	if e != nil || len(entries) != 0 {
		t.Fatal("outside effects", entries, e)
	}
}
func TestPinnedStagePreventsSourceReplacement(t *testing.T) {
	s := testStore(t)
	o := stage(t, s, "original")
	s.hooks.beforeLink = func() {
		s.hooks.beforeLink = nil
		p := filePath(s, "staging", o.ID)
		if e := os.Rename(p, p+"-old"); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(p, []byte("attacker"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.Publish(o, allow); e != nil {
		t.Fatal(e)
	}
	b, e := s.Read(o)
	if e != nil || string(b) != "original" {
		t.Fatalf("%q %v", b, e)
	}
}
func TestDestinationReplacementAndMountLossAtFence(t *testing.T) {
	for _, kind := range []string{"rename", "symlink", "mount-loss"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			o := stage(t, s, "safe")
			outside := t.TempDir()
			s.hooks.beforeLink = func() {
				s.hooks.beforeLink = nil
				p := filePath(s, "revisions", "")
				if e := os.Rename(p, p+"-old"); e != nil {
					t.Fatal(e)
				}
				if kind == "symlink" {
					if e := os.Symlink(outside, p); e != nil {
						t.Fatal(e)
					}
				} else {
					if e := os.Mkdir(p, 0700); e != nil {
						t.Fatal(e)
					}
				}
				if kind == "mount-loss" {
					s.settings.RequiredMount = s.settings.Root
				}
			}
			if e := s.Publish(o, allow); e == nil {
				t.Fatal("published after storage replacement")
			}
			if _, e := os.Stat(filePath(s, "revisions-old", o.ID)); !os.IsNotExist(e) {
				t.Fatal("published to old parent", e)
			}
			entries, e := os.ReadDir(outside)
			if e != nil || len(entries) != 0 {
				t.Fatal(entries, e)
			}
		})
	}
}
func TestCollisionConcurrentPublicationAndPriorBytes(t *testing.T) {
	s := testStore(t)
	o := stage(t, s, "original")
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.Publish(o, allow) }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for e := range results {
		if e == nil {
			successes++
		} else if !errors.Is(e, ErrConflict) {
			t.Fatal(e)
		}
	}
	if successes != 1 {
		t.Fatal(successes)
	}
	b, e := s.Read(o)
	if e != nil || string(b) != "original" {
		t.Fatalf("%q %v", b, e)
	}
	// A different stage cannot replace an existing destination even when metadata
	// has been corrupted to request the colliding ID.
	p := filePath(s, "staging", o.ID)
	if e = os.Remove(p); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p, []byte("changed!"), 0400); e != nil {
		t.Fatal(e)
	}
	changed := o
	changed.SHA256 = digest([]byte("changed!"))
	changed.Size = 8
	if e = s.Publish(changed, allow); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	b, e = s.Read(o)
	if e != nil || string(b) != "original" {
		t.Fatalf("%q %v", b, e)
	}
}
func TestFaultsIntegrityAndFence(t *testing.T) {
	s := testStore(t)
	o := stage(t, s, "original")
	if e := s.Publish(o, nil); !errors.Is(e, ErrFence) {
		t.Fatal(e)
	}
	denied := errors.New("denied")
	if e := s.Publish(o, func() error { return denied }); !errors.Is(e, denied) {
		t.Fatal(e)
	}
	s.hooks.link = func(int, string, int, string, int) error { return unix.EOPNOTSUPP }
	if e := s.Publish(o, allow); !errors.Is(e, ErrUnsupported) {
		t.Fatal(e)
	}
	if _, e := os.Stat(filePath(s, "revisions", o.ID)); !os.IsNotExist(e) {
		t.Fatal(e)
	}
	linked := false
	s.hooks.link = func(a int, b string, c int, d string, e int) error {
		err := unix.Linkat(a, b, c, d, e)
		linked = err == nil
		return err
	}
	s.hooks.sync = func(f *os.File) error {
		if linked {
			return unix.EIO
		}
		return f.Sync()
	}
	if e := s.Publish(o, allow); !errors.Is(e, ErrUncertain) {
		t.Fatal(e)
	}
	s.hooks.sync = nil
	b, e := s.Read(o)
	if e != nil || string(b) != "original" {
		t.Fatal(string(b), e)
	}
	wrong := o
	wrong.SHA256 = strings.Repeat("0", 64)
	if _, e = s.Read(wrong); !errors.Is(e, ErrIntegrity) {
		t.Fatal(e)
	}
	wrong = o
	wrong.ID = "../escape"
	if _, e = s.Read(wrong); !errors.Is(e, ErrPath) {
		t.Fatal(e)
	}
	if e = os.Chmod(filePath(s, "revisions", o.ID), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filePath(s, "revisions", o.ID), []byte("tampered"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Read(o); !errors.Is(e, ErrIntegrity) {
		t.Fatal(e)
	}
}
func TestStageFaultsDoNotLeavePartialObjects(t *testing.T) {
	for _, kind := range []string{"sync", "link"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			dir, e := s.directory("staging", true)
			if e != nil {
				t.Fatal(e)
			}
			dir.Close()
			if kind == "sync" {
				s.hooks.sync = func(f *os.File) error {
					info, err := f.Stat()
					if err != nil {
						return err
					}
					if info.Mode().IsRegular() {
						return unix.EIO
					}
					return f.Sync()
				}
			} else {
				s.hooks.link = func(int, string, int, string, int) error { return unix.EOPNOTSUPP }
			}
			if _, e = s.Stage("x.md", strings.NewReader("safe")); e == nil {
				t.Fatal("fault hidden")
			}
			entries, e := os.ReadDir(filePath(s, "staging", ""))
			if e != nil || len(entries) != 0 {
				t.Fatal(entries, e)
			}
		})
	}
}

func TestActualNestedMountRefused(t *testing.T) {
	root, e := os.Open("/")
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	if f, e := openAt(root, "proc", unix.O_RDONLY|unix.O_DIRECTORY); !errors.Is(e, ErrUnsupported) {
		if f != nil {
			f.Close()
		}
		t.Fatal("mount crossing was not refused", e)
	}
}
func TestStageDirectorySwapAndUncertainDurability(t *testing.T) {
	t.Run("swapped parent", func(t *testing.T) {
		s := testStore(t)
		s.hooks.beforeLink = func() {
			s.hooks.beforeLink = nil
			p := filePath(s, "staging", "")
			if e := os.Rename(p, p+"-old"); e != nil {
				t.Fatal(e)
			}
			if e := os.Mkdir(p, 0700); e != nil {
				t.Fatal(e)
			}
		}
		if _, e := s.Stage("a.md", strings.NewReader("text")); e == nil {
			t.Fatal("stage used replaced directory")
		}
		entries, e := os.ReadDir(filePath(s, "staging-old", ""))
		if e != nil || len(entries) != 0 {
			t.Fatal("partial stage retained", entries, e)
		}
	})
	t.Run("directory sync", func(t *testing.T) {
		s := testStore(t)
		dir, e := s.directory("staging", true)
		if e != nil {
			t.Fatal(e)
		}
		dir.Close()
		linked := false
		s.hooks.link = func(a int, b string, c int, d string, e int) error {
			err := unix.Linkat(a, b, c, d, e)
			linked = err == nil
			return err
		}
		s.hooks.sync = func(f *os.File) error {
			if linked {
				return unix.EIO
			}
			return f.Sync()
		}
		o, e := s.Stage("a.md", strings.NewReader("text"))
		if !errors.Is(e, ErrUncertain) || !validObject(o, s.settings.MaxFileBytes) {
			t.Fatal(o, e)
		}
		s.hooks.sync = nil
		if e = s.Publish(o, allow); e != nil {
			t.Fatal("cannot reconcile uncertain stage", e)
		}
	})
}
