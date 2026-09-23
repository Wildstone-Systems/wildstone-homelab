package portal

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

// Open a single child without following symlinks. /proc/self/fd names an
// already-open descriptor, not a client path; retain it until OpenRoot has
// taken its own reference. os.Root has no no-follow OpenRoot variant.
func openChildRoot(root *os.Root, name string) (*os.Root, error) {
	f, err := openNoFollow(root, name, unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
}

func openRegular(root *os.Root, name string) (*os.File, error) {
	f, err := openNoFollow(root, name, unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, ErrNotFound
	}
	return f, nil
}

func renameNoReplace(src *os.Root, from string, dst *os.Root, to string, beforeWrite func() error) error {
	a, err := src.Open(".")
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := dst.Open(".")
	if err != nil {
		return err
	}
	defer b.Close()
	if err := beforeWrite(); err != nil {
		return err
	}
	info, err := src.Lstat(from)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return ErrNotFound
	}
	// No reservation-and-replace fallback: a second writer can replace the
	// reservation before rename. Unsupported filesystems must fail closed.
	err = unix.Renameat2(int(a.Fd()), from, int(b.Fd()), to, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) {
		return ErrAtomicMove
	}
	return err
}

// Root.OpenFile follows in-root symlinks even when passed O_NOFOLLOW (Go
// resolves them itself). Use a single validated leaf relative to an fd opened
// through the Root so the kernel's no-follow decision is final.
func openNoFollow(root *os.Root, name string, flags int) (*os.File, error) {
	if err := treeLeaf(name); err != nil {
		return nil, err
	}
	parent, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|flags, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
