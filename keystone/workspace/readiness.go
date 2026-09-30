package workspace

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// OpenRoot opens existing storage without creating anything. Required mounts
// are pinned and checked against kernel mount IDs, so disappearance cannot
// silently redirect access to the local directory underneath the mount.
// Storage consumers still need their own publication/authority execution fence.
func (s Settings) OpenRoot() (*os.Root, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if s.RequiredMount != "" {
		return s.openMountedRoot()
	}
	if !canonicalDirectory(s.Root) {
		return nil, ErrRoot
	}
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		return nil, ErrRoot
	}
	return root, nil
}

// Ready verifies local directory presence or required mount metadata. It avoids
// remote filesystem I/O for mount-backed storage, which could hang on hard NFS.
// It does not claim writability, capacity, remote health or recovery consistency;
// storage operations must call OpenRoot again and handle their own I/O errors.
func (s Settings) Ready() error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.RequiredMount != "" {
		info, err := os.Open("/proc/self/mountinfo")
		if err != nil {
			return ErrMount
		}
		defer info.Close()
		if !mountHostsRoot(info, s.RequiredMount, s.Root, "") {
			return ErrMount
		}
		return nil
	}
	root, err := s.OpenRoot()
	if err != nil {
		return err
	}
	return root.Close()
}

func canonicalDirectory(path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

func (s Settings) openMountedRoot() (*os.Root, error) {
	return s.openMountedRootWith(
		func() (io.ReadCloser, error) { return os.Open("/proc/self/mountinfo") },
		func(fd uintptr) ([]byte, error) { return os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd)) },
	)
}

// The injected kernel readers exercise mount loss/replacement without requiring
// privileged test mounts. Production always uses this process's kernel metadata.
func (s Settings) openMountedRootWith(mountInfo func() (io.ReadCloser, error), fdInfo func(uintptr) ([]byte, error)) (*os.Root, error) {
	info, err := mountInfo()
	if err != nil {
		return nil, ErrMount
	}
	present := mountHostsRoot(info, s.RequiredMount, s.Root, "")
	info.Close()
	if !present {
		return nil, ErrMount
	}
	if !canonicalDirectory(s.RequiredMount) {
		return nil, ErrMount
	}
	base, err := os.OpenRoot(s.RequiredMount)
	if err != nil {
		return nil, ErrMount
	}
	defer base.Close()
	id := rootMountID(base, fdInfo)
	if id == "" {
		return nil, ErrMount
	}
	info, err = mountInfo()
	if err != nil {
		return nil, ErrMount
	}
	present = mountHostsRoot(info, s.RequiredMount, s.Root, id)
	info.Close()
	if !present {
		return nil, ErrMount
	}
	if !canonicalDirectory(s.Root) {
		return nil, ErrRoot
	}
	rel, err := filepath.Rel(s.RequiredMount, s.Root)
	if err != nil {
		return nil, ErrRoot
	}
	root, err := base.OpenRoot(rel)
	if err != nil {
		return nil, ErrRoot
	}
	// Opening a descendant can cross a mount added after the metadata check.
	// Pin the actual workspace descriptor too; an ancestor's ID is not enough.
	if rootMountID(root, fdInfo) != id {
		root.Close()
		return nil, ErrMount
	}
	info, err = mountInfo()
	if err != nil {
		root.Close()
		return nil, ErrMount
	}
	present = mountHostsRoot(info, s.RequiredMount, s.Root, id)
	info.Close()
	if !present {
		root.Close()
		return nil, ErrMount
	}
	return root, nil
}

func rootMountID(root *os.Root, fdInfo func(uintptr) ([]byte, error)) string {
	pin, err := root.Open(".")
	if err != nil {
		return ""
	}
	defer pin.Close()
	data, err := fdInfo(pin.Fd())
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "mnt_id:" {
			return fields[1]
		}
	}
	return ""
}

// The configured required mount must host Root directly. Intervening mounts
// require their own explicit required_mount, so loss cannot fall back to an
// otherwise healthy ancestor. Read the whole snapshot and fail closed on errors.
func mountHostsRoot(r io.Reader, target, root, id string) bool {
	present := false
	scan := bufio.NewScanner(r)
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) <= 6 {
			continue
		}
		path := unescape.Replace(fields[4])
		if path == target && (id == "" || fields[0] == id) {
			present = true
		}
		if path != target && strings.HasPrefix(path, target+"/") &&
			(root == path || strings.HasPrefix(root, path+"/")) {
			return false
		}
	}
	return present && scan.Err() == nil
}
