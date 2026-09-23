package portal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// checkMount reads kernel mount metadata, not the remote filesystem: an
// unreachable hard NFS mount must not hang the readiness check. This does not
// establish reachability; actual I/O still reports the server's errors.
func (s *FileStore) checkMount() error {
	if s.RequiredMount == "" {
		return nil
	}
	mount := filepath.Clean(s.RequiredMount)
	if !filepath.IsAbs(mount) || mount == "/" || !filepath.IsAbs(s.Root) {
		return ErrUnavailable
	}
	rel, err := filepath.Rel(mount, s.Root)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return ErrUnavailable
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("%w: cannot inspect required mount", ErrUnavailable)
	}
	defer f.Close()
	if !mountPresent(f, mount) {
		return fmt.Errorf("%w: required mount is absent", ErrUnavailable)
	}
	return nil
}

func mountPresent(r io.Reader, target string) bool {
	scan := bufio.NewScanner(r)
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) > 5 && unescape.Replace(fields[4]) == target {
			return true
		}
	}
	return false
}

// openStorageRoot pins the required mount before opening the configured root.
// No MkdirAll is allowed in this branch, even if the mount disappears between
// the metadata check and opening it. The fd's mount ID proves that the opened
// directory is on the declared mount, not the local directory underneath.
func (s *FileStore) openStorageRoot() (*os.Root, error) {
	if s.RequiredMount == "" {
		if _, err := os.Stat(s.Root); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(s.Root, 0o755); err != nil {
				return nil, err
			}
		}
		return os.OpenRoot(s.Root)
	}
	if err := s.checkMount(); err != nil {
		return nil, err
	}
	mount := filepath.Clean(s.RequiredMount)
	base, err := os.OpenRoot(mount)
	if err != nil {
		return nil, fmt.Errorf("%w: required mount cannot be opened", ErrUnavailable)
	}
	defer base.Close()
	pin, err := base.Open(".")
	if err != nil {
		return nil, err
	}
	defer pin.Close()
	fdinfo, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", pin.Fd()))
	if err != nil {
		return nil, ErrUnavailable
	}
	id := ""
	for _, line := range strings.Split(string(fdinfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "mnt_id:" {
			id = fields[1]
		}
	}
	info, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, ErrUnavailable
	}
	defer info.Close()
	scan := bufio.NewScanner(info)
	present := false
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) > 5 && fields[0] == id && mountPresent(strings.NewReader(scan.Text()), mount) {
			present = true
			break
		}
	}
	if !present {
		return nil, ErrUnavailable
	}
	rel, err := filepath.Rel(mount, s.Root)
	if err != nil {
		return nil, ErrUnavailable
	}
	root, err := base.OpenRoot(rel)
	if err != nil {
		return nil, fmt.Errorf("%w: configured folder is absent or inaccessible on the mount", ErrUnavailable)
	}
	return root, nil
}

// Available checks configuration and required mount presence without remote I/O.
// It does not claim the remote server is reachable or writable.
func (s *FileStore) Available() bool { return s.Ready() && s.checkMount() == nil }
