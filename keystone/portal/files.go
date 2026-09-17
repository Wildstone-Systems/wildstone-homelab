package portal

// The Files boundary (#197): upload, list, read, under a root the
// household's administrator chose and a household member cannot name.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

// DefaultMaxUpload bounds one upload.
//
// A number rather than "whatever fits", because the request is streamed
// to disk: without a limit the first person to point a phone's whole
// camera roll at this fills the household's storage, and the failure
// arrives as every other service breaking rather than as a refused
// upload. 512 MiB clears a long 4K video from a phone, which is the
// largest thing a family actually drops on a dashboard.
const DefaultMaxUpload int64 = 512 << 20

// MaxFilename bounds the one caller-supplied string that reaches the
// filesystem. Long enough for "IMG_20260917_084233_Portrait.jpg", short
// enough to stay well inside every filesystem's own limit once a
// collision suffix is appended.
const MaxFilename = 120

// Namespace is a logical destination, and the whole of what an upload
// may choose.
//
// Dir is a single path segment under the root, never a path: the
// mapping from "what a person picked in a dropdown" to "where bytes
// land" is this table and nothing else. Adding a destination is a row
// here plus a line in the widget; it is deliberately not something a
// package, a request or a model can invent.
type Namespace struct {
	Key   string
	Title string
	Dir   string
}

// Namespaces is the closed set. "shared" first: it is the default, and
// the default is what an upload with no opinion gets.
//
// Deliberately two rather than the four #197 sketches. Personal space
// (/files/me) is a real feature and a real decision — whether a
// household member's own subtree is private FROM the operator, what
// happens to it when they are offboarded — and shipping a directory
// named after a person before that decision is made would be shipping
// the word "private" without the property.
var Namespaces = []Namespace{
	{Key: "shared", Title: "Shared", Dir: "Shared"},
	{Key: "photos", Title: "Photos", Dir: "Photos"},
}

// DefaultNamespace is what an upload that names none lands in.
const DefaultNamespace = "shared"

var (
	// ErrNoRoot is "this capability is installed but has no storage
	// root", which is a configuration fault rather than a caller's.
	ErrNoRoot = errors.New("portal: no files root is configured")
	// ErrNamespace is a destination that is not in the table. Refused,
	// never coerced to the default: an upload that lands somewhere
	// other than where the person was told it would is worse than one
	// that fails.
	ErrNamespace = errors.New("portal: no such destination")
	// ErrTooLarge is the bounded-upload refusal.
	ErrTooLarge = errors.New("portal: file is larger than this household allows")
	// ErrNotFound is a read of something that is not there. Shaped the
	// same for "no such file" and "not a regular file", so that probing
	// cannot map the root's shape.
	ErrNotFound = errors.New("portal: no such file")
)

// FileStore is the Files capability's data plane.
//
// Root is an ABSOLUTE host path, supplied by the capability's own
// declaration (the ConfigFile the package renders), never by a request.
// MaxUpload bounds one file; zero means DefaultMaxUpload.
type FileStore struct {
	Root      string
	MaxUpload int64
}

// Item is one stored file as a person sees it: what it is called, how
// big it is, when it landed, and which destination holds it.
type Item struct {
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"modTime"`
}

// NamespaceFor resolves a key to its row. The empty key is the default;
// anything else unrecognised is refused.
func NamespaceFor(key string) (Namespace, error) {
	if key == "" {
		key = DefaultNamespace
	}
	for _, ns := range Namespaces {
		if ns.Key == key {
			return ns, nil
		}
	}
	return Namespace{}, fmt.Errorf("%w: %q", ErrNamespace, key)
}

// MaxUploadFor is the effective per-file ceiling: what the household
// configured, or the default. Exported because a client is told the
// limit before it offers an upload, and a client told a different
// number than the one enforced is a client that reports a working
// upload as a failure.
func (s *FileStore) MaxUploadFor() int64 {
	if s != nil && s.MaxUpload > 0 {
		return s.MaxUpload
	}
	return DefaultMaxUpload
}

// Ready reports whether this store can serve anything at all. A store
// with no root answers "not configured" on every endpoint rather than
// writing to a relative path next to whatever the process's working
// directory happens to be.
func (s *FileStore) Ready() bool {
	return s != nil && filepath.IsAbs(s.Root)
}

// open returns an *os.Root confined to the namespace's directory,
// creating it on first use.
//
// os.Root rather than filepath.Join plus a prefix check, and this is
// the load-bearing line of the whole file: every open through it is
// resolved by the kernel WITHOUT leaving the directory, so a symlink
// somebody planted in the media tree — the one traversal that survives
// every amount of string cleaning — cannot carry a write out of it
// either. The filename normalisation below is still applied, because
// "cannot escape" and "is a sane name" are different properties.
func (s *FileStore) open(ns Namespace) (*os.Root, error) {
	if !s.Ready() {
		return nil, ErrNoRoot
	}
	dir := filepath.Join(s.Root, ns.Dir)
	// 0o755: the household's other applications — the media server, a
	// file browser installed later — read this tree as their own user,
	// and a destination only root can list is a destination whose whole
	// point (the file shows up in the Files app) does not happen.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("portal: preparing %s: %w", ns.Title, err)
	}
	return os.OpenRoot(dir)
}

// Stored is the result of one accepted upload.
type Stored struct {
	Item
	// Renamed reports that the file landed under a different name than
	// the one offered, because something was already there. Surfaced to
	// the caller rather than handled silently: a person who uploads
	// IMG_4821.jpg twice should be told they now have two.
	Renamed bool `json:"renamed,omitempty"`
}

// Save streams one upload into a namespace.
//
// The body is copied to a temporary file in the destination directory
// and renamed into place only once it is complete, so a dropped
// connection leaves nothing a file browser will show as a truncated
// photo. It is never buffered in memory: a household upload is the one
// request whose size is chosen by the person making it, and #197 says
// so in as many words.
func (s *FileStore) Save(nsKey, offered string, body io.Reader) (Stored, error) {
	ns, err := NamespaceFor(nsKey)
	if err != nil {
		return Stored{}, err
	}
	root, err := s.open(ns)
	if err != nil {
		return Stored{}, err
	}
	defer root.Close()

	name := SafeName(offered)
	tmpName, tmp, err := createTemp(root)
	if err != nil {
		return Stored{}, err
	}
	// Any failure from here on removes the temporary file. A partial
	// upload that stays on disk is storage nobody can see and nobody
	// will ever delete.
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			_ = root.Remove(tmpName)
		}
	}()

	max := s.MaxUploadFor()
	// max+1 so that a file of exactly the limit is accepted and the
	// first byte past it is what trips the refusal.
	written, err := io.Copy(tmp, io.LimitReader(body, max+1))
	if err != nil {
		return Stored{}, fmt.Errorf("portal: receiving %s: %w", name, err)
	}
	if written > max {
		return Stored{}, fmt.Errorf("%w (%d bytes, limit %d)", ErrTooLarge, written, max)
	}
	// Flushed before the rename, so what the rename publishes is the
	// whole file rather than whatever the page cache has got to.
	if err := tmp.Sync(); err != nil {
		return Stored{}, fmt.Errorf("portal: flushing %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return Stored{}, fmt.Errorf("portal: closing %s: %w", name, err)
	}

	final, renamed, err := freeName(root, name)
	if err != nil {
		return Stored{}, err
	}
	if err := root.Rename(tmpName, final); err != nil {
		return Stored{}, fmt.Errorf("portal: storing %s: %w", final, err)
	}
	committed = true
	// 0o644 for the reason the directory is 0o755.
	_ = root.Chmod(final, 0o644)

	info, err := root.Stat(final)
	if err != nil {
		return Stored{}, fmt.Errorf("portal: reading back %s: %w", final, err)
	}
	return Stored{
		Item: Item{
			Name: final, Namespace: ns.Key,
			Size: info.Size(), ModTime: info.ModTime(),
		},
		Renamed: renamed,
	}, nil
}

// freeName is the collision policy: keep the name, or add a counter.
//
// Overwriting was the alternative and it is not survivable on a
// household surface — two phones both call their photos IMG_0001.jpg,
// and the second upload would silently destroy the first person's. The
// counter goes before the extension so the file still opens.
func freeName(root *os.Root, name string) (string, bool, error) {
	if _, err := root.Stat(name); errors.Is(err, os.ErrNotExist) {
		return name, false, nil
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i <= 999; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if _, err := root.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, true, nil
		}
	}
	return "", false, fmt.Errorf("portal: %s already exists a thousand times over", name)
}

// createTemp opens a uniquely named scratch file inside the namespace's
// own directory.
//
// Inside it rather than in /tmp, because the commit is a rename and a
// rename across filesystems is a copy — which would turn "the file
// appears whole or not at all" back into "the file appears while it is
// still arriving". O_EXCL with a random name rather than a counter, so
// two phones uploading at once cannot pick the same scratch file.
func createTemp(root *os.Root) (string, *os.File, error) {
	var raw [8]byte
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := rand.Read(raw[:]); err != nil {
			return "", nil, fmt.Errorf("portal: naming a temporary file: %w", err)
		}
		// Dotted, so a half-arrived upload is skipped by every listing
		// this package serves and by File Browser's default view.
		name := ".wildstone-upload-" + hex.EncodeToString(raw[:])
		f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return name, f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, fmt.Errorf("portal: opening a temporary file: %w", err)
		}
	}
	return "", nil, errors.New("portal: could not open a temporary file")
}

// SafeName reduces a caller-supplied filename to one that means the
// same thing to a person and nothing at all to a filesystem.
//
// Exported because it is the function a reviewer wants to read, and
// tested directly for the same reason. Everything it refuses, os.Root
// would also refuse; both exist because the second one is the
// guarantee and the first is the one that keeps a household's files
// browsable.
func SafeName(offered string) string {
	// Both separators, because the string may have come from a Windows
	// phone or a browser that kept a whole path.
	offered = strings.ReplaceAll(offered, "\\", "/")
	offered = filepath.Base(strings.TrimSpace(offered))

	var b strings.Builder
	for _, r := range offered {
		switch {
		case r == '/' || r == filepath.Separator:
			// Cannot happen after Base; refused anyway, because this
			// function is the one a future caller will reuse without
			// reading it.
			b.WriteRune('_')
		case unicode.IsControl(r), r == 0:
			// Dropped, not replaced: a control character is not a name
			// a person typed.
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		case strings.ContainsRune(" ._-()[]+&'#,@", r):
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	name := strings.TrimSpace(b.String())
	// A leading dot hides the file from every listing this package
	// serves, which is how an upload lands and then appears to have
	// vanished. "..", ".", and the empty string are the three names
	// that are not names at all.
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "upload"
	}
	if len([]rune(name)) > MaxFilename {
		ext := filepath.Ext(name)
		if len([]rune(ext)) > 12 {
			ext = ""
		}
		stem := []rune(strings.TrimSuffix(name, ext))
		keep := MaxFilename - len([]rune(ext))
		name = strings.TrimSpace(string(stem[:keep])) + ext
	}
	return name
}

// List returns one namespace's files, newest first.
func (s *FileStore) List(nsKey string) ([]Item, error) {
	ns, err := NamespaceFor(nsKey)
	if err != nil {
		return nil, err
	}
	items, err := s.list(ns)
	if err != nil {
		return nil, err
	}
	sortNewestFirst(items)
	return items, nil
}

// Recent is the widget's list: the newest files across every
// namespace, which is what "did my upload work" actually asks.
func (s *FileStore) Recent(n int) ([]Item, error) {
	if n <= 0 {
		n = 5
	}
	var all []Item
	for _, ns := range Namespaces {
		items, err := s.list(ns)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
	}
	sortNewestFirst(all)
	if len(all) > n {
		all = all[:n]
	}
	return all, nil
}

func (s *FileStore) list(ns Namespace) ([]Item, error) {
	root, err := s.open(ns)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("portal: listing %s: %w", ns.Title, err)
	}
	items := make([]Item, 0, len(entries))
	for _, e := range entries {
		// Directories are not listed and not descended into. This is a
		// drop box, not a file manager: a tree view is a browse UI, and
		// a browse UI over a media root is the "give the dashboard the
		// filesystem" non-goal #197 names.
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// A file that vanished between the listing and the stat is
			// not an error worth failing a dashboard over.
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		items = append(items, Item{
			Name: e.Name(), Namespace: ns.Key,
			Size: info.Size(), ModTime: info.ModTime(),
		})
	}
	return items, nil
}

func sortNewestFirst(items []Item) {
	sort.Slice(items, func(i, j int) bool {
		if !items[i].ModTime.Equal(items[j].ModTime) {
			return items[i].ModTime.After(items[j].ModTime)
		}
		// A deterministic tiebreak, so two files written in the same
		// millisecond do not swap places between two reads of the same
		// directory.
		return items[i].Name < items[j].Name
	})
}

// Open returns one file for download, and the only name it will accept
// is one SafeName would have produced.
//
// The comparison is against the normalised form rather than a scan for
// "..": it is the same rule the write path applied, so a name that
// could not have been created here cannot be read here either.
func (s *FileStore) Open(nsKey, name string) (*os.File, os.FileInfo, error) {
	ns, err := NamespaceFor(nsKey)
	if err != nil {
		return nil, nil, err
	}
	if name == "" || SafeName(name) != name {
		return nil, nil, ErrNotFound
	}
	root, err := s.open(ns)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()

	f, err := root.Open(name)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, ErrNotFound
	}
	return f, info, nil
}
