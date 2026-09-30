package workspace

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

// The kernel checks every component during the open. EvalSymlinks followed by
// OpenRoot alone leaves a substitution window, including for in-root symlinks.
func openRootNoSymlinks(path string) (*os.Root, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
			return nil, ErrFilesystem
		}
		return nil, ErrRoot
	}
	defer unix.Close(fd)
	root, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return nil, ErrRoot
	}
	return root, nil
}

func openRootDescendant(base *os.Root, path string) (*os.Root, error) {
	pin, err := base.Open(".")
	if err != nil {
		return nil, ErrRoot
	}
	defer pin.Close()
	fd, err := unix.Openat2(int(pin.Fd()), path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return nil, ErrRoot
	}
	defer unix.Close(fd)
	root, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return nil, ErrRoot
	}
	return root, nil
}
