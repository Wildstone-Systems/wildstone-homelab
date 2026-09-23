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
	"regexp"
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
// "me" is the personal one, and its Dir is EMPTY because it does not
// have one until a person is known: it resolves to a directory under
// Members/ named for the authenticated member, and the name comes from
// the proven identity rather than from any field of a request. There is
// no spelling in this API for "somebody else's personal space".
//
// The two questions that held this back have answers now, and both are
// the repository's rather than mine.
//
// IS IT PRIVATE FROM THE OPERATOR? No, and it cannot be. These files
// live on the household's own filesystem and whoever administers the
// machine can read them; encryption at rest with a key the operator does
// not hold is a different feature. So the property this actually
// provides is privacy FROM OTHER MEMBERS, and the page says exactly
// that rather than the word "private" on its own. Shipping a directory
// named after a person while implying more than that is what the
// earlier note was refusing to do.
//
// WHAT HAPPENS AT OFFBOARDING? ADR-W030 already decided it for personal
// work: it pauses and nothing is deleted, because it is somebody's
// unfinished business and the household may need to know what it was.
// That falls out here with no code: "me" resolves from the authenticated
// member, an offboarded person no longer authenticates, and their
// directory stays exactly where it is for the household to deal with.
var Namespaces = []Namespace{
	{Key: "shared", Title: "Shared", Dir: "Shared"},
	{Key: "photos", Title: "Photos", Dir: "Photos"},
	{Key: "me", Title: "Mine", Dir: ""},
}

// personalRoot is the directory every member's own space lives under.
const personalRoot = "Members"

// memberDir is the strict shape a member's directory name may take.
//
// STRICT, AND REFUSING RATHER THAN NORMALISING, which is the whole
// point. A sanitiser that mapped "a/b" and "a_b" onto one directory
// would silently join two people's private space; refusing anything
// that is not already a plain name cannot. Members come from a
// directory service and look like this; one that does not gets no
// personal space rather than somebody else's.
var memberDir = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

// PersonalDir is the directory name for one member's own space, or
// ok=false for a name this will not turn into a directory.
//
// Lower-cased, because the rest of the household compares member names
// case-insensitively (ADR-W032 A4 records what happens when it does
// not) and two spellings of one person must not become two folders.
func PersonalDir(member string) (string, bool) {
	name := strings.TrimSpace(member)
	if !memberDir.MatchString(name) {
		return "", false
	}
	// "." and ".." pass the character class and are not names.
	if strings.Trim(name, ".") == "" {
		return "", false
	}
	return personalRoot + "/" + strings.ToLower(name), true
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
	// BeforeWrite is the runtime store execution fence, required for mutations.
	BeforeWrite func() error
	Root        string
	// RequiredMount is an optional owner-configured mountpoint containing Root.
	// When set, no access may create or use a local replacement if it is absent.
	RequiredMount string
	MaxUpload     int64
	// Member is the authenticated person this store is acting for, and
	// it is the ONLY thing that resolves the "me" namespace. Set by the
	// caller from the proven identity; empty means the personal space is
	// unavailable rather than shared, so a bug that forgets to set it
	// fails closed.
	Member string
}

// Item is one stored file as a person sees it: what it is called, how
// big it is, when it landed, and which destination holds it.
type Item struct {
	Name      string    `json:"name"`
	Path      string    `json:"path,omitempty"`
	Type      string    `json:"type,omitempty"`
	Namespace string    `json:"namespace"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"modTime"`
}

// NamespaceFor resolves a key to its row. The empty key is the default;
// anything else unrecognised is refused.
//
// It does NOT resolve "me" to a directory — it cannot, because it does
// not know who is asking. Every path that touches the filesystem goes
// through (*FileStore).namespace instead, which does.
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

// namespace resolves a key for THIS store's member, filling in the
// personal space's directory.
//
// The member is never a parameter and never comes from a request. That
// is the entire isolation guarantee: there is no argument a caller can
// supply that makes this return another person's directory, so "member
// A cannot read member B's files" is a property of the signature rather
// than of a check somebody has to remember.
func (s *FileStore) namespace(key string) (Namespace, error) {
	ns, err := NamespaceFor(key)
	if err != nil {
		return Namespace{}, err
	}
	if ns.Dir != "" {
		return ns, nil
	}
	dir, ok := PersonalDir(s.Member)
	if !ok {
		// No usable member: the personal space is ABSENT, never
		// silently the shared one. An upload that landed in Shared
		// because a name had a dash in it would be a privacy failure
		// wearing a shrug.
		return Namespace{}, fmt.Errorf("%w: %q", ErrNamespace, key)
	}
	ns.Dir = dir
	return ns, nil
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
//
// THE CONFINEMENT STARTS AT THE CONFIGURED ROOT, not at the namespace,
// and that is a correction rather than a flourish. This used to join
// the root and the namespace into a path and hand THAT to os.OpenRoot,
// which resolves its own argument the ordinary way — following any
// symlink it is made of before the confinement exists to stop it. So a
// symlink at `Shared`, or at a member's own directory, was followed,
// and everything the kernel then refused to leave was the wrong
// directory: a household member could list, download, rename and delete
// through it, outside the root an administrator configured, with every
// check in this package passing.
//
// The root is opened first and the namespace is resolved THROUGH it, so
// the boundary is the one the operator actually set. A symlink that
// stays inside the root is still followed — an operator who arranges
// their own tree that way is not attacking themselves — and one that
// leaves it is refused by the kernel, which is the whole point of
// asking the kernel.
func (s *FileStore) open(ns Namespace) (*os.Root, error) {
	if !s.Ready() {
		return nil, ErrNoRoot
	}
	if err := s.checkMount(); err != nil {
		return nil, err
	}
	// Opened before anything is created, so every step below — the
	// existence test, the mkdir, the chmod — is a step the kernel has
	// already confined.
	// The root is created on first use. It is a setting with a default
	// (/srv/wildstone/media), and whether that folder exists depended on
	// whether some OTHER package happened to mount it: a household that
	// installed the files widget without a media server got an error on
	// every upload. Only the root itself is made here — everything below
	// it still goes through os.Root.
	base, err := s.openStorageRoot()
	if err != nil {
		return nil, err
	}
	defer base.Close()
	// 0o755: the household's other applications — the media server, a
	// file browser installed later — read this tree as their own user,
	// and a destination only root can list is a destination whose whole
	// point (the file shows up in the Files app) does not happen.
	//
	// THE CHMOD IS NOT REDUNDANT. MkdirAll's mode is masked by the
	// process umask, and Keystone's own systemd unit sets UMask=0077 —
	// correct for a control plane whose data directory holds a CA key,
	// and exactly wrong for the one directory it creates on the
	// household's behalf. The first live upload landed in a 0700
	// directory: the file was there, and nothing but root could see it.
	//
	// Only on creation. An operator who has deliberately tightened an
	// existing destination should not have it widened again on the next
	// upload.
	//
	// A PERSONAL SPACE IS THE EXCEPTION, and gets 0o700. The reasoning
	// above is about the SHARED destinations: they exist so that the
	// media server and a file browser can see what the family dropped.
	// Nobody else is meant to read one member's own folder, so widening
	// it to every container on the box would hand the property away at
	// the moment of creation. Its parent, Members/, is 0o711 — traversable
	// so each member's own directory can be opened, not listable, so a
	// process that gets that far cannot enumerate the household.
	mode := os.FileMode(0o755)
	if strings.HasPrefix(ns.Dir, personalRoot+"/") {
		mode = 0o700
		// Lstat rather than Stat throughout, so that a symlink AT one of
		// these names is seen as the symlink it is rather than as
		// whatever it points at. What happens to it is then the
		// kernel's to decide, below.
		if _, err := base.Lstat(personalRoot); errors.Is(err, os.ErrNotExist) {
			if err := base.Mkdir(personalRoot, 0o711); err != nil {
				return nil, fmt.Errorf("portal: preparing personal space: %w", err)
			}
			if err := base.Chmod(personalRoot, 0o711); err != nil {
				return nil, fmt.Errorf("portal: preparing personal space: %w", err)
			}
		}
	}
	if _, err := base.Lstat(ns.Dir); errors.Is(err, os.ErrNotExist) {
		if err := base.MkdirAll(ns.Dir, mode); err != nil {
			return nil, fmt.Errorf("portal: preparing %s: %w", ns.Title, err)
		}
		if err := base.Chmod(ns.Dir, mode); err != nil {
			return nil, fmt.Errorf("portal: preparing %s: %w", ns.Title, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("portal: preparing %s: %w", ns.Title, err)
	}
	return base.OpenRoot(ns.Dir)
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
	if err := s.checkWriteAuthority(); err != nil {
		return Stored{}, err
	}
	ns, err := s.namespace(nsKey)
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
	// The temporary file always goes, and that is right whichever way
	// the commit below went: a rename moved it away and this is a
	// no-op, a hard link published its data under the real name and
	// this drops the second link, or the upload failed and this is the
	// partial file going — storage nobody can see and nobody will ever
	// delete.
	defer func() {
		tmp.Close()
		_ = root.Remove(tmpName)
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
	if err := s.checkWriteAuthority(); err != nil {
		return Stored{}, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return Stored{}, err
	}
	info, err := tmp.Stat()
	if err != nil {
		return Stored{}, err
	}
	// Flushed before the rename, so what the rename publishes is the
	// whole file rather than whatever the page cache has got to.
	if err := tmp.Sync(); err != nil {
		return Stored{}, fmt.Errorf("portal: flushing %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return Stored{}, fmt.Errorf("portal: closing %s: %w", name, err)
	}

	if err := s.checkWriteAuthority(); err != nil {
		return Stored{}, err
	}
	final, renamed, err := claimName(root, tmpName, name, s.checkWriteAuthority)
	if err != nil {
		return Stored{}, err
	}

	return Stored{
		Item: Item{
			Name: final, Namespace: ns.Key,
			Size: info.Size(), ModTime: info.ModTime(),
		},
		Renamed: renamed,
	}, nil
}

// claimName is the collision policy: keep the name, or add a counter.
//
// Overwriting was the alternative and it is not survivable on a
// household surface — two phones both call their photos IMG_0001.jpg,
// and the second upload would silently destroy the first person's. The
// counter goes before the extension so the file still opens.
//
// LOOKING AND THEN TAKING IS NOT THE SAME AS TAKING. This used to ask
// whether a name existed and then rename onto it, which is two steps
// with a gap in the middle, and two phones uploading the same common
// filename at the same moment both found it free and both renamed onto
// it: os.Rename replaces a file that is in the way, so the second
// upload destroyed the first while both people were told it worked.
// The counter cannot protect anybody if the answer it is based on is
// already stale by the time it is used.
//
// So the name is TAKEN, in one operation the filesystem will not let
// two callers win, and the candidate that loses is a collision like any
// other — it moves on to the next counter rather than failing.
func claimName(root *os.Root, tmpName, name string, beforeWrite func() error) (string, bool, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i <= 999; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		taken, err := takeName(root, tmpName, candidate, beforeWrite)
		if err != nil {
			return "", false, err
		}
		if taken {
			return candidate, i > 0, nil
		}
	}
	return "", false, fmt.Errorf("portal: %s already exists a thousand times over", name)
}

// takeName publishes the completed upload under one candidate name, and
// reports false if somebody else holds that name.
//
// A hard link first, because it is the whole commit in a single step
// the filesystem refuses to do twice: it fails rather than replaces
// when the name is taken, and it makes the name appear with the whole
// file already behind it. The scratch file's own link goes in Save's
// cleanup.
//
// Filesystems without hard links may use atomic no-replace rename. There is
// no reservation-and-replace fallback: a concurrent delete/upload could take
// that reservation away and the final rename would destroy its replacement.
func takeName(root *os.Root, tmpName, candidate string, beforeWrite func() error) (bool, error) {
	if err := beforeWrite(); err != nil {
		return false, err
	}
	switch err := root.Link(tmpName, candidate); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrExist):
		return false, nil
	}
	if err := beforeWrite(); err != nil {
		return false, err
	}
	if err := renameNoReplace(root, tmpName, root, candidate, beforeWrite); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
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
	ns, err := s.namespace(nsKey)
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
//
// EACH NAMESPACE IS RESOLVED, not taken from the table as it stands.
// The personal entry carries no directory of its own — it cannot, since
// which directory it means depends on who is asking — and looping over
// the table handed that empty directory straight to the opener, which
// read it as "no subdirectory" and listed the storage ROOT under the
// member's own heading. It showed nothing, because the root holds only
// directories and those are skipped, so "Mine" contributed nothing to
// this list and no test noticed. A member's own uploads belong in the
// answer to "did my upload work" more than anybody's.
func (s *FileStore) Recent(n int) ([]Item, error) {
	if n <= 0 {
		n = 5
	}
	var all []Item
	for _, entry := range Namespaces {
		ns, err := s.namespace(entry.Key)
		if err != nil {
			// A member with no usable name has no personal space. The
			// shared destinations are still theirs to see, so this is a
			// namespace to skip rather than a list to fail.
			if errors.Is(err, ErrNamespace) {
				continue
			}
			return nil, err
		}
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
		// Stat'd back THROUGH the root rather than from the directory
		// entry. A DirEntry from an fs.FS carries a lazy Info() that
		// re-resolves the entry by the name its root was opened under,
		// and this root is opened by a relative name inside the
		// configured one — so that lookup resolves against the process's
		// working directory and fails for every file. It fails quietly,
		// into the "vanished between the listing and the stat" case
		// below, which is how a listing that returned nothing at all
		// looked exactly like an empty directory.
		info, err := root.Lstat(e.Name())
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
	ns, err := s.namespace(nsKey)
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

// Delete removes one file from a namespace.
//
// IT IS IMMEDIATE, AND THAT IS WHY THE PAGE SAYS SO. The design's
// delete dialog offers "items are kept 30 days before the storage is
// reclaimed", which is a good promise and a different feature: it needs
// a trash directory, a sweeper with a clock, and a restore path, none
// of which exist here. Shipping that sentence over this function would
// be telling a household their photos are recoverable when they are
// not — the one class of lie a confirmation dialog must never tell. The
// page states the truth instead, and the promise stays on the list.
//
// A directory is refused rather than removed. The namespaces are flat
// today, so any directory in one was put there by something other than
// this surface, and RemoveAll on a caller-named path is not a power a
// household dashboard should hold.
func (s *FileStore) Delete(nsKey, name string) error {
	if err := s.checkWriteAuthority(); err != nil {
		return err
	}
	ns, err := s.namespace(nsKey)
	if err != nil {
		return err
	}
	// The same test Open makes: a name that is not already its own safe
	// form is not a name this surface wrote, and it is not resolved.
	if name == "" || SafeName(name) != name {
		return ErrNotFound
	}
	root, err := s.open(ns)
	if err != nil {
		return err
	}
	defer root.Close()

	info, err := root.Lstat(name)
	if err != nil {
		return ErrNotFound
	}
	// Lstat, not Stat, so a symlink is judged as a symlink: one pointing
	// at a directory outside the namespace would otherwise look like a
	// regular file's sibling and be removed on its behalf.
	if !info.Mode().IsRegular() {
		return ErrNotFound
	}
	if err := s.checkWriteAuthority(); err != nil {
		return err
	}
	if err := root.Remove(name); err != nil {
		return fmt.Errorf("portal: deleting %s: %w", name, err)
	}
	return nil
}

// Rename changes one file's name within its namespace.
//
// WITHIN, and there is no argument for the destination namespace: a
// rename that could also move something is two operations wearing one
// name, and the one that crosses a boundary is the one that wants its
// own thought about authority. The design's Move control is absent from
// the page for the same reason it is absent here — better three fewer
// letters than a control that half-works.
//
// The new name goes through SafeName like an upload's does, and the
// result is reported, because a person who typed "holiday/2026.jpg" and
// got "holiday_2026.jpg" should see that rather than wonder.
func (s *FileStore) Rename(nsKey, from, to string) (string, error) {
	if err := s.checkWriteAuthority(); err != nil {
		return "", err
	}
	ns, err := s.namespace(nsKey)
	if err != nil {
		return "", err
	}
	if from == "" || SafeName(from) != from {
		return "", ErrNotFound
	}
	// SafeName's last resort is the literal "upload", which is the right
	// answer for an upload — the bytes have arrived and must land
	// somewhere — and the wrong one here. A person who cleared the box
	// and pressed Rename would have their file renamed to "upload"; the
	// honest response to "no name" is to ask again.
	if !hasSubstance(to) {
		return "", fmt.Errorf("portal: a file needs a name with at least one letter or number in it")
	}
	safe := SafeName(to)
	root, err := s.open(ns)
	if err != nil {
		return "", err
	}
	defer root.Close()

	info, err := root.Lstat(from)
	if err != nil || !info.Mode().IsRegular() {
		return "", ErrNotFound
	}
	if safe == from {
		return safe, nil
	}
	// Refused rather than overwriting, and refused rather than
	// silently suffixed: an upload collision is somebody adding a
	// second thing and claimName is right there, but a RENAME collision
	// is somebody about to lose a file they did not name in this
	// request.
	//
	// The legacy flat API shares the same atomic no-replace primitive as
	// tree moves. Reservations can be deleted and replaced by another request.
	if err := s.checkWriteAuthority(); err != nil {
		return "", err
	}
	if err := renameNoReplace(root, from, root, safe, s.checkWriteAuthority); err != nil {
		return "", err
	}
	return safe, nil
}

// hasSubstance reports whether an offered name contains anything a
// person would recognise as a name, rather than only spaces, dots and
// punctuation that normalisation would throw away.
//
// It exists because SafeName is deliberately total — every input maps to
// some usable filename — and a rename needs the one question SafeName
// does not answer: was there a name here at all.
func hasSubstance(offered string) bool {
	for _, r := range offered {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func (s *FileStore) checkWriteAuthority() error {
	if s.BeforeWrite == nil {
		return fmt.Errorf("%w: no coordinator authority is wired", ErrWriteAuthority)
	}
	if err := s.checkMount(); err != nil {
		return err
	}
	if err := s.BeforeWrite(); err != nil {
		return fmt.Errorf("%w: %w", ErrWriteAuthority, err)
	}
	return nil
}
