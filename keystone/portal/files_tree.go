package portal

// Shared and Mine support trees (#275). Photos retains the flat API.
// Paths select entries below the identity-resolved namespace, never a host path.
import (
	"errors"
	"fmt"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/internal/confinedpath"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

const MaxPathDepth = confinedpath.MaxPathDepth
const MaxPathBytes = confinedpath.MaxPathBytes
const MaxNameBytes = confinedpath.MaxNameBytes

var (
	ErrPath           = confinedpath.ErrPath
	ErrUnavailable    = errors.New("the files storage is unavailable")
	ErrWriteAuthority = errors.New("file changes are paused")
	ErrAtomicMove     = errors.New("this filesystem does not support safe atomic rename or move")
)

type RelativePath = confinedpath.RelativePath

func ParseRelativePath(raw string) (RelativePath, error) { return confinedpath.ParseRelativePath(raw) }

func treeLeaf(raw string) error {
	p, err := ParseRelativePath(raw)
	if err != nil || p == "" || strings.Contains(raw, "/") {
		return ErrPath
	}
	return nil
}

func treeNamespace(ns string) bool {
	return ns == "" || ns == "shared" || ns == "me"
}

func treePath(ns, raw string, allowRoot bool) (RelativePath, error) {
	p, err := ParseRelativePath(raw)
	if err != nil || (!allowRoot && p == "") {
		return "", ErrPath
	}
	if !treeNamespace(ns) && strings.Contains(raw, "/") {
		return "", ErrPath
	}
	return p, nil
}

// treeDir pins each component before descending. openChildRoot refuses
// symlinks in the same syscall that opens the directory; Lstat followed by
// OpenRoot would have a race. Every child remains rooted in an os.Root.
func (s *FileStore) treeDir(nsKey, raw string) (*os.Root, Namespace, error) {
	ns, err := s.namespace(nsKey)
	if err != nil {
		return nil, ns, err
	}
	p, err := treePath(ns.Key, raw, true)
	if err != nil || (!treeNamespace(ns.Key) && p != "") {
		return nil, ns, ErrPath
	}
	root, err := s.open(ns)
	if err != nil {
		return nil, ns, err
	}
	if p == "" {
		return root, ns, nil
	}
	for _, component := range strings.Split(string(p), "/") {
		child, err := openChildRoot(root, component)
		root.Close()
		if err != nil {
			return nil, ns, err
		}
		root = child
	}
	return root, ns, nil
}

func (s *FileStore) treeParent(nsKey, raw string) (*os.Root, string, error) {
	p, err := treePath(nsKey, raw, false)
	if err != nil {
		return nil, "", err
	}
	parent, leaf := path.Split(string(p))
	root, _, err := s.treeDir(nsKey, strings.TrimSuffix(parent, "/"))
	return root, leaf, err
}

func (s *FileStore) ListAt(nsKey, dir string) ([]Item, error) {
	root, ns, err := s.treeDir(nsKey, dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(entries))
	for _, entry := range entries {
		if treeLeaf(entry.Name()) != nil {
			continue
		}
		info, err := root.Lstat(entry.Name())
		if err != nil || (!info.Mode().IsRegular() && !info.IsDir()) {
			continue
		}
		if info.IsDir() && !treeNamespace(ns.Key) {
			continue
		}
		kind := "file"
		if info.IsDir() {
			kind = "directory"
		}
		items = append(items, Item{Name: entry.Name(), Namespace: ns.Key, Path: path.Join(dir, entry.Name()), Type: kind, Size: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Type != items[j].Type {
			return items[i].Type == "directory"
		}
		return items[i].Name < items[j].Name
	})
	return items, nil
}

func (s *FileStore) OpenAt(nsKey, raw string) (*os.File, os.FileInfo, error) {
	root, leaf, err := s.treeParent(nsKey, raw)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	// Nonblocking also refuses the FIFO trap: inspecting the type after a
	// blocking open would let an uploaded special file park a request forever.
	f, err := openRegular(root, leaf)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

func (s *FileStore) SaveAt(nsKey, dir, name string, body io.Reader) (Stored, error) {
	if err := treeLeaf(name); err != nil {
		return Stored{}, err
	}
	if _, err := treePath(nsKey, pathIn(dir, name), false); err != nil {
		return Stored{}, err
	}
	if err := s.checkWriteAuthority(); err != nil {
		return Stored{}, err
	}
	root, ns, err := s.treeDir(nsKey, dir)
	if err != nil {
		return Stored{}, err
	}
	defer root.Close()
	if err := s.checkWriteAuthority(); err != nil {
		return Stored{}, err
	}
	tmpName, tmp, err := createTemp(root)
	if err != nil {
		return Stored{}, err
	}
	defer func() { tmp.Close(); _ = root.Remove(tmpName) }()
	written, err := io.Copy(tmp, io.LimitReader(body, s.MaxUploadFor()+1))
	if err != nil {
		return Stored{}, err
	}
	if written > s.MaxUploadFor() {
		return Stored{}, ErrTooLarge
	}
	// Set mode through the open descriptor, before publication. A chmod by
	// the published name could follow a concurrently substituted symlink.
	if err := tmp.Chmod(0o644); err != nil {
		return Stored{}, err
	}
	if err := tmp.Sync(); err != nil {
		return Stored{}, err
	}
	info, err := tmp.Stat()
	if err != nil {
		return Stored{}, err
	}
	if err := tmp.Close(); err != nil {
		return Stored{}, err
	}
	if err := s.checkWriteAuthority(); err != nil {
		return Stored{}, err
	}
	// An exclusive hard link publishes atomically, including on NFS.
	// A collision is explicit
	// for this API: the household chooses a new name instead of losing data.
	if err := root.Link(tmpName, name); err != nil {
		return Stored{}, err
	}
	return Stored{Item: Item{Name: name, Namespace: ns.Key, Path: pathIn(dir, name), Type: "file", Size: info.Size(), ModTime: info.ModTime()}}, nil
}

func pathIn(dir, leaf string) string {
	if dir == "" {
		return leaf
	}
	return dir + "/" + leaf
}

func (s *FileStore) Mkdir(nsKey, raw string) error {
	if !treeNamespace(nsKey) {
		return ErrNamespace
	}
	if err := s.checkWriteAuthority(); err != nil {
		return err
	}
	root, leaf, err := s.treeParent(nsKey, raw)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := s.checkWriteAuthority(); err != nil {
		return err
	}
	if err := root.Mkdir(leaf, 0o755); err != nil {
		return err
	}
	if err := s.checkWriteAuthority(); err != nil {
		if cleanupErr := root.Remove(leaf); cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("remove refused directory: %w", cleanupErr))
		}
		return err
	}
	child, err := openChildRoot(root, leaf)
	if err != nil {
		return err
	}
	defer child.Close()
	// Chmod through the pinned directory, including under a restrictive umask.
	return child.Chmod(".", 0o755)
}

func (s *FileStore) Move(nsKey, from, to string) error {
	if !treeNamespace(nsKey) {
		return ErrNamespace
	}
	if err := s.checkWriteAuthority(); err != nil {
		return err
	}
	if _, err := treePath(nsKey, to, false); err != nil {
		return err
	}
	src, old, err := s.treeParent(nsKey, from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, name, err := s.treeParent(nsKey, to)
	if err != nil {
		return err
	}
	defer dst.Close()
	if err := s.checkWriteAuthority(); err != nil {
		return err
	}
	info, err := src.Lstat(old)
	if err != nil {
		return err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return ErrNotFound
	}
	if from == to {
		return nil
	}
	if strings.HasPrefix(to, from+"/") {
		return fmt.Errorf("%w: a folder cannot be moved inside itself", ErrPath)
	}
	return renameNoReplace(src, old, dst, name, s.checkWriteAuthority)
}

func (s *FileStore) RenameAt(nsKey, from, name string) error {
	if err := treeLeaf(name); err != nil {
		return err
	}
	parent, _ := path.Split(from)
	return s.Move(nsKey, from, parent+name)
}

func (s *FileStore) DeleteAt(nsKey, raw string) error {
	if err := s.checkWriteAuthority(); err != nil {
		return err
	}
	root, leaf, err := s.treeParent(nsKey, raw)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := s.checkWriteAuthority(); err != nil {
		return err
	}
	info, err := root.Lstat(leaf)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() && !(info.IsDir() && treeNamespace(nsKey)) {
		return ErrNotFound
	}
	// Remove, never RemoveAll: the filesystem refuses a nonempty directory.
	return root.Remove(leaf)
}
