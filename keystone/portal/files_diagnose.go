package portal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// StorageFinding is one path FileDrop needs but Keystone's effective storage
// identity cannot use.
type StorageFinding struct {
	// Path is absolute, rooted at the configured FileDrop root.
	Path string `json:"path"`
	// Need is "write" or "list".
	Need string `json:"need"`
	// Owner is the path's observed ownership and mode, e.g. "65534:65534 0711".
	Owner string `json:"owner,omitempty"`
	Err   string `json:"error"`
}

// RepairAdvice is what an operator should do about findings. It is advice, not
// an action: FileDrop never changes ownership of an existing tree itself.
const RepairAdvice = "On the storage server, change ownership of only the listed FileDrop paths to the identity " +
	"Keystone's mount maps to (for example after changing an NFS export's root-squash or anonuid mapping). " +
	"Do not chmod the tree world-writable, and keep Members at mode 0711."

// DiagnoseStorage checks, with Keystone's effective storage identity, every
// existing directory FileDrop needs: the root, each namespace directory, the
// personal root, and the listing of each existing member directory. A writable
// root says nothing about the directories below it (#551).
//
// It never creates a configured directory and never writes into another
// member's private directory; the only writes are a bounded probe directory in
// the root, the namespace directories, Members and the requesting member's own
// existing directory, removed straight away. When the
// runtime write fence refuses writes the probes are skipped, since no upload
// could happen either. A nil result means nothing is known to be wrong.
func (s *FileStore) DiagnoseStorage() ([]StorageFinding, error) {
	if !s.Ready() {
		return nil, ErrNoRoot
	}
	if err := s.checkMount(); err != nil {
		return nil, err
	}
	if s.RequiredMount == "" {
		// Only a root that does not exist yet is the intentional empty
		// result (uploads create it). Any other failure means the root
		// cannot be used and must not read as healthy.
		if _, err := os.Stat(s.Root); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
	}
	base, err := s.openStorageRoot()
	if err != nil {
		return nil, err
	}
	defer base.Close()
	canWrite := s.BeforeWrite == nil || s.BeforeWrite() == nil
	check := s.access
	if check == nil {
		check = probeAccess
	}

	var out []StorageFinding
	add := func(rel, need string, err error) {
		f := StorageFinding{Path: filepath.Join(s.Root, rel), Need: need, Err: err.Error()}
		if info, serr := base.Lstat(rel); serr == nil {
			f.Owner = ownerString(info)
		}
		out = append(out, f)
	}
	dirs := []string{"."}
	for _, ns := range Namespaces {
		if ns.Dir != "" {
			dirs = append(dirs, ns.Dir)
		}
	}
	dirs = append(dirs, personalRoot)
	for _, rel := range dirs {
		if info, err := base.Lstat(rel); err != nil || !info.IsDir() {
			continue
		}
		if canWrite {
			if err := check(base, rel, true); err != nil {
				add(rel, "write", err)
			}
		}
		if err := check(base, rel, false); err != nil {
			add(rel, "list", err)
		}
	}
	// The requesting member's own directory must also take writes, or Mine is
	// advertised ready while every upload fails.
	ownRel, hasOwn := PersonalDirFor(s.Member, s.Folders)
	// Existing member directories: listable only, except the requester's own.
	if members, err := base.Open(personalRoot); err == nil {
		names, _ := members.Readdirnames(-1)
		members.Close()
		for _, name := range names {
			rel := personalRoot + "/" + name
			if info, err := base.Lstat(rel); err != nil || !info.IsDir() {
				continue
			}
			if canWrite && hasOwn && rel == ownRel {
				if err := check(base, rel, true); err != nil {
					add(rel, "write", err)
				}
			}
			if err := check(base, rel, false); err != nil {
				add(rel, "list", err)
			}
		}
	}
	return out, nil
}

// StorageProblem summarises findings in one sentence for a status reason.
func StorageProblem(findings []StorageFinding) string {
	if len(findings) == 0 {
		return ""
	}
	paths := make([]string, 0, 3)
	for i, f := range findings {
		if i == 3 {
			break
		}
		paths = append(paths, fmt.Sprintf("%s (%s, %s)", f.Path, f.Need, f.Owner))
	}
	more := ""
	if len(findings) > 3 {
		more = fmt.Sprintf(" and %d more", len(findings)-3)
	}
	return "storage permission problem: Keystone cannot " + strings.Join(paths, "; ") + more + ". " + RepairAdvice
}

// probeAccess tries the operation for real: the server, not the local mode
// bits, decides what an NFS identity may do.
func probeAccess(root *os.Root, rel string, write bool) error {
	if !write {
		d, err := root.Open(rel)
		if err != nil {
			return err
		}
		defer d.Close()
		if _, err := d.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}
	for i := 0; i < 8; i++ {
		probe := filepath.Join(rel, fmt.Sprintf(".wildstone-probe-%d-%d", os.Getpid(), i))
		err := root.Mkdir(probe, 0o700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		return root.Remove(probe)
	}
	return nil
}
