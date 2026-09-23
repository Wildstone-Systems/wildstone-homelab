package portal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestTreeSpecialFilesDoNotBlockOrMutate(t *testing.T) {
	s := store(t)
	if _, err := s.List("shared"); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(s.Root, "Shared", "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if f, _, err := s.OpenAt("shared", "pipe"); err == nil {
		f.Close()
		t.Fatal("opened fifo")
	}
	if err := s.DeleteAt("shared", "pipe"); err == nil {
		t.Fatal("removed fifo")
	}
	if err := s.Move("shared", "pipe", "moved"); err == nil {
		t.Fatal("moved fifo")
	}
	items, err := s.ListAt("shared", "")
	if err != nil || len(items) != 0 {
		t.Fatalf("%v %v", items, err)
	}
}

func TestSymlinkReplacementRacesNeverEscapeTree(t *testing.T) {
	s := store(t)
	save(t, s, "shared", "local", "local")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(s.Root, "Shared")
	if err := os.Mkdir(filepath.Join(root, "real"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", "secret"), []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "swap")); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = unix.Renameat2(unix.AT_FDCWD, filepath.Join(root, "real"), unix.AT_FDCWD, filepath.Join(root, "swap"), unix.RENAME_EXCHANGE)
		}
	}()
	for i := 0; i < 200; i++ {
		f, _, err := s.OpenAt("shared", "real/secret")
		if err == nil {
			b := make([]byte, 20)
			n, _ := f.Read(b)
			f.Close()
			if string(b[:n]) != "inside" {
				close(stop)
				wg.Wait()
				t.Fatal("read outside", string(b[:n]))
			}
		}
		_, _ = s.SaveAt("shared", "real", "new", strings.NewReader("inside"))
	}
	close(stop)
	wg.Wait()
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 {
		t.Fatalf("outside changed: %v %v", entries, err)
	}
}

// Run deliberately against an existing writable NFS fixture with the SAME
// effective service identity as Keystone. Only a new temporary subdirectory
// is touched; the shared export itself is never cleaned or chmodded.
func TestSharedTreeNFSIntegration(t *testing.T) {
	fixture := os.Getenv("WILDSTONE_TEST_NFS_ROOT")
	if fixture == "" {
		t.Skip("set WILDSTONE_TEST_NFS_ROOT to a writable mounted NFS fixture")
	}
	var st unix.Statfs_t
	if err := unix.Statfs(fixture, &st); err != nil {
		t.Fatal(err)
	}
	if st.Type != unix.NFS_SUPER_MAGIC {
		t.Fatal("fixture is not NFS")
	}
	root, err := os.MkdirTemp(fixture, "wildstone-275-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	s := &FileStore{Root: root, BeforeWrite: func() error { return nil }}
	if err := s.Mkdir("shared", "folder"); err != nil {
		t.Fatal("mapped identity cannot create/chmod", err)
	}
	if _, err := s.SaveAt("shared", "folder", "file", strings.NewReader("nfs")); err != nil {
		t.Fatal("mapped identity cannot publish", err)
	}
	f, _, err := s.OpenAt("shared", "folder/file")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := s.SaveAt("shared", "folder", "file", strings.NewReader("overwrite")); !errors.Is(err, os.ErrExist) {
		t.Fatal("collision", err)
	}
	err = s.Move("shared", "folder/file", "folder/moved")
	if errors.Is(err, ErrAtomicMove) {
		t.Log("NFS rejects atomic no-replace rename; refusal preserves original")
	} else if err != nil {
		t.Fatal(err)
	} else {
		if err := s.Move("shared", "folder/moved", "folder/file"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteAt("shared", "folder/file"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAt("shared", "folder"); err != nil {
		t.Fatal(err)
	}
}

func TestRequiredMountPinsTheActualFilesystem(t *testing.T) {
	root, err := os.MkdirTemp("/dev/shm", "wildstone-275-")
	if err != nil {
		t.Skip("no writable tmpfs fixture", err)
	}
	defer os.RemoveAll(root)
	s := &FileStore{Root: root, RequiredMount: "/dev/shm", BeforeWrite: func() error { return nil }}
	if !s.Available() {
		t.Skip("/dev/shm is not a distinct mount")
	}
	if err := s.Mkdir("shared", "folder"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveAt("shared", "folder", "file", strings.NewReader("mounted")); err != nil {
		t.Fatal(err)
	}
	// A root outside the declared mount is refused even though both exist.
	s.Root = t.TempDir()
	if _, err := s.ListAt("shared", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestCrossDeviceMoveLeavesBothDirectoriesIntact(t *testing.T) {
	other, err := os.MkdirTemp("/dev/shm", "wildstone-275-")
	if err != nil {
		t.Skip("no writable second filesystem", err)
	}
	defer os.RemoveAll(other)
	s := store(t)
	save(t, s, "shared", "source", "keep")
	src, err := os.OpenRoot(filepath.Join(s.Root, "Shared"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenRoot(other)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	var a, b unix.Stat_t
	if err := unix.Stat(s.Root, &a); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(other, &b); err != nil {
		t.Fatal(err)
	}
	if a.Dev == b.Dev {
		t.Skip("fixture shares the test filesystem")
	}
	err = renameNoReplace(src, "source", dst, "destination", func() error { return nil })
	if !errors.Is(err, unix.EXDEV) {
		t.Fatal(err)
	}
	if _, err := src.Stat("source"); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Stat("destination"); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
