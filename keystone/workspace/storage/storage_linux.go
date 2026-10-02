package storage

import (
	"errors"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"strings"
)

type hooks struct {
	beforeOpen func()
	beforeLink func()
	link       func(int, string, int, string, int) error
	sync       func(*os.File) error
}

func storageError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EEXIST) {
		return ErrConflict
	}
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EXDEV) {
		return ErrUnsupported
	}
	return workspace.ErrUnavailable // Never expose syscall paths to tools/audit.
}
func openAt(parent *os.File, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat2(int(parent.Fd()), name, &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return nil, storageError(err)
	}
	return os.NewFile(uintptr(fd), "workspace"), nil
}
func (s *Store) root() (*os.File, error) {
	root, err := s.settings.OpenRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(".")
	if err != nil {
		return nil, workspace.ErrRoot
	}
	return f, nil
}
func (s *Store) sync(f *os.File) error {
	if s.hooks.sync != nil {
		return s.hooks.sync(f)
	}
	return f.Sync()
}

// Only namespace children are created; never provision the configured root.
func (s *Store) directory(kind string, create bool) (*os.File, error) {
	parent, err := s.root()
	if err != nil {
		return nil, err
	}
	if s.hooks.beforeOpen != nil {
		s.hooks.beforeOpen()
	}
	for _, part := range strings.Split(s.namespace+"/"+kind, "/") {
		if create {
			err = unix.Mkdirat(int(parent.Fd()), part, 0700)
			if err != nil && !errors.Is(err, unix.EEXIST) {
				parent.Close()
				return nil, storageError(err)
			}
			// Sync even an existing entry: an earlier interrupted mkdir may
			// have left its parent durability unconfirmed.
			if err = s.sync(parent); err != nil {
				parent.Close()
				return nil, storageError(err)
			}
		}
		child, e := openAt(parent, part, unix.O_RDONLY|unix.O_DIRECTORY)
		parent.Close()
		if e != nil {
			return nil, e
		}
		parent = child
	}
	return parent, nil
}
func sameFile(a, b *os.File) bool {
	x, e := a.Stat()
	if e != nil {
		return false
	}
	y, e := b.Stat()
	return e == nil && os.SameFile(x, y)
}

// Reopening checks kernel mount metadata/IDs and refuses replaced parents.
func (s *Store) current(dir *os.File, kind string) error {
	now, err := s.directory(kind, false)
	if err != nil {
		return err
	}
	defer now.Close()
	if !sameFile(dir, now) {
		return workspace.ErrRoot
	}
	return nil
}
func (s *Store) link(file, dir *os.File, name string) error {
	link := s.hooks.link
	if link == nil {
		link = unix.Linkat
	}
	// Link the pinned inode, never a swappable staging pathname. This trusted
	// procfs descriptor path avoids AT_EMPTY_PATH's capability requirement.
	return storageError(link(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", file.Fd()), int(dir.Fd()), name, unix.AT_SYMLINK_FOLLOW))
}

// Stage persists one complete bounded object with a random server-issued ID.
// It streams assembled chunks, not a write-session API. Stages are not visible
// artifacts and survive restart. Unsupported O_TMPFILE stores refuse visibly.
func (s *Store) Stage(logicalPath string, body io.Reader) (Object, error) {
	if err := ValidatePath(logicalPath, false); err != nil {
		return Object{}, err
	}
	data, err := readText(body, s.settings.MaxFileBytes)
	if err != nil {
		return Object{}, err
	}
	id, err := newID()
	if err != nil {
		return Object{}, err
	}
	obj := Object{ID: id, SHA256: digest(data), Size: int64(len(data))}
	dir, err := s.directory("staging", true)
	if err != nil {
		return Object{}, err
	}
	defer dir.Close()
	if err = s.current(dir, "staging"); err != nil {
		return Object{}, err
	}
	// Unnamed inodes cannot be substituted and disappear after failed writes.
	fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0600)
	if err != nil {
		return Object{}, storageError(err)
	}
	f := os.NewFile(uintptr(fd), "workspace-stage")
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return Object{}, storageError(err)
	}
	if err = f.Chmod(0400); err != nil {
		return Object{}, storageError(err)
	}
	if err = s.sync(f); err != nil {
		return Object{}, storageError(err)
	}
	if s.hooks.beforeLink != nil {
		s.hooks.beforeLink()
	}
	if err = s.current(dir, "staging"); err != nil {
		return Object{}, err
	}
	if err = s.link(f, dir, id); err != nil {
		return Object{}, err
	}
	if err = s.sync(dir); err != nil {
		return obj, ErrUncertain
	}
	// A pinned directory remains writable after an ancestor rename/unmount.
	// Do not claim success if the effect landed in a detached namespace.
	if err = s.current(dir, "staging"); err != nil {
		return obj, ErrUncertain
	}
	return obj, nil
}
func (s *Store) verified(kind string, obj Object) (*os.File, []byte, error) {
	if !validObject(obj, s.settings.MaxFileBytes) {
		return nil, nil, ErrPath
	}
	dir, err := s.directory(kind, false)
	if err != nil {
		return nil, nil, err
	}
	defer dir.Close()
	f, err := openAt(dir, obj.ID, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, ErrIntegrity
	}
	data, err := readText(f, s.settings.MaxFileBytes)
	if err != nil || int64(len(data)) != obj.Size || digest(data) != obj.SHA256 {
		f.Close()
		return nil, nil, ErrIntegrity
	}
	if err = s.current(dir, kind); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, data, nil
}

// Publish exclusively links the verified stage inode using the same object ID.
// A collision always refuses, including retries: the coordinator settles these.
// The mandatory fence verifies current authority/config and prepared intent.
// Staging is retained for recovery; cleanup and DB commit are separate slices.
func (s *Store) Publish(obj Object, fence func() error) error {
	if fence == nil {
		return ErrFence
	}
	if err := fence(); err != nil {
		return err
	}
	f, _, err := s.verified("staging", obj)
	if err != nil {
		return err
	}
	defer f.Close()
	dir, err := s.directory("revisions", true)
	if err != nil {
		return err
	}
	defer dir.Close()
	if s.hooks.beforeLink != nil {
		s.hooks.beforeLink()
	}
	if err = fence(); err != nil {
		return err
	}
	if err = s.current(dir, "revisions"); err != nil {
		return err
	}
	if err = s.link(f, dir, obj.ID); err != nil {
		return err
	}
	if err = s.sync(dir); err != nil {
		return ErrUncertain
	}
	if err = s.current(dir, "revisions"); err != nil {
		return ErrUncertain
	}
	return nil
}

// Read checks bytes against trusted journal metadata; callers must separately
// prove this exact revision object is committed and currently authorized.
func (s *Store) Read(obj Object) ([]byte, error) {
	f, data, err := s.verified("revisions", obj)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return data, nil
}
