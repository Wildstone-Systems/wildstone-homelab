// Package knowledge is the household's Markdown knowledge store
// (ADR-W046): one folder of plain .md files that people edit in whatever
// Markdown client they like and agents reach through a handful of
// bounded operations.
//
// THE FOLDER IS THE CONTRACT. There is no index, no database row and no
// sidecar file a note depends on: a note is a UTF-8 Markdown file at a
// relative path, and its revision is a hash of its bytes. Anything built
// on top — a search index, embeddings, a web UI's cache — is derived and
// can be thrown away and rebuilt from the folder. Removing a UI package
// therefore removes nothing a note needs.
//
// A CALLER NEVER NAMES A HOST PATH. Every operation takes a path
// RELATIVE to the configured root, validated by keystone/portal's
// ParseRelativePath — refused, never cleaned — and resolved through an
// os.Root, so neither "..", an absolute path nor a symlink can reach
// outside the folder. Symlinks inside it are refused too: a note is a
// regular file, and following a link would make "which file did the
// agent change" a question about the link's target.
//
// WRITES ARE ATOMIC AND NEVER BLIND. A new note is staged, synced and
// published by an exclusive hard link, so a same-name collision is a
// refusal rather than an overwrite. An update names the revision it was
// based on; if the file changed since (a person saved in the web UI, say)
// the update is refused with the current revision and nothing is
// written. The replaced bytes are kept under .history first, so an
// agent's update is always reversible by copying a file back.
package knowledge

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
)

// Extension is the only kind of file the store reads or writes.
const Extension = ".md"

// Limits. Each one is a bound on what a single call can make the control
// plane do, not a statement about how big a household's notes may get:
// a person can keep a longer note, and the agent tools simply decline to
// write one or read past the cap.
const (
	// MaxWriteBytes bounds one agent-written note. A model producing
	// more than this in one call is writing a document, not a note.
	MaxWriteBytes = 64 << 10
	// MaxReadBytes is the largest note the store will read or search.
	MaxReadBytes = 1 << 20
	// MaxScanFiles bounds one search. Past it the search stops and says
	// it did.
	MaxScanFiles = 5000
	// MinQuery is the shortest search the store will run.
	MinQuery = 2
	// MaxSnippetRunes bounds one search hit's excerpt.
	MaxSnippetRunes = 160
)

// HistoryDir holds every version an update replaced. Hidden, so the
// agent tools cannot list, read or write it (ParseRelativePath refuses
// dot components) and most Markdown clients do not show it.
const HistoryDir = ".history"

// File modes the store publishes with. Explicit because Keystone runs
// under UMask=0077, and a note only root can read is a note the web UI
// cannot show.
const (
	fileMode os.FileMode = 0o644
	dirMode  os.FileMode = 0o755
)

// Errors a caller can tell apart.
var (
	ErrNoRoot   = errors.New("no knowledge folder is configured")
	ErrPath     = errors.New("not a usable note path")
	ErrNotFound = errors.New("no such note")
	ErrNotNote  = errors.New("not a note")
	ErrExists   = errors.New("a note already exists at that path")
	ErrConflict = errors.New("the note changed since it was read")
	ErrTooLarge = errors.New("the note is too large")
	ErrContent  = errors.New("the note's content is not accepted")
	ErrReserved = errors.New("that path is reserved")
	ErrQuery    = errors.New("the search is too short")
)

// Note describes one note without its content.
type Note struct {
	// Path is relative to the root, with forward slashes and the .md
	// extension.
	Path    string
	Size    int64
	ModTime time.Time
	// Revision identifies these exact bytes. An update must name it.
	Revision string
}

// Entry is one item in a folder listing.
type Entry struct {
	Path    string
	Dir     bool
	Size    int64
	ModTime time.Time
}

// Hit is one search result.
type Hit struct {
	Note
	// Snippet is a short excerpt around the first match in the content,
	// or the note's opening when only the path matched.
	Snippet string
}

// Store is the knowledge folder. The zero value has no root and refuses
// everything with ErrNoRoot.
type Store struct {
	// Root is the absolute host path of the knowledge folder.
	Root string
	// BeforeWrite is the household execution fence, checked when a
	// write starts and again immediately before it publishes. nil means
	// no fence (tests).
	BeforeWrite func() error
	// Now is the clock, for history file names.
	Now func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) fence() error {
	if s.BeforeWrite != nil {
		return s.BeforeWrite()
	}
	return nil
}

func (s *Store) open() (*os.Root, error) {
	if s == nil || strings.TrimSpace(s.Root) == "" {
		return nil, ErrNoRoot
	}
	if !path.IsAbs(s.Root) {
		return nil, fmt.Errorf("%w: the configured folder is not an absolute path", ErrNoRoot)
	}
	// Created on first use, as the drop box's root is: the Owner chose
	// the path at install time, and nothing else on the control plane
	// makes directories on a package's behalf.
	if _, err := os.Lstat(s.Root); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(s.Root, dirMode); err != nil {
			return nil, fmt.Errorf("%w: creating the configured folder: %v", ErrNoRoot, err)
		}
		if err := os.Chmod(s.Root, dirMode); err != nil {
			return nil, err
		}
	}
	return os.OpenRoot(s.Root)
}

// Revision is the identity of a note's bytes.
func Revision(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:8])
}

// NotePath validates a note path and gives it the .md extension when it
// has none. Nothing else about it is changed: a path that is not already
// a clean relative path is refused.
func NotePath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrPath
	}
	if !strings.HasSuffix(strings.ToLower(raw), Extension) {
		raw += Extension
	}
	p, err := portal.ParseRelativePath(raw)
	if err != nil || p == "" {
		return "", ErrPath
	}
	if path.Base(string(p)) == Extension {
		return "", ErrPath
	}
	return string(p), nil
}

// FolderPath validates a folder path. Empty is the root.
func FolderPath(raw string) (string, error) {
	raw = strings.Trim(strings.TrimSpace(raw), "/")
	p, err := portal.ParseRelativePath(raw)
	if err != nil {
		return "", ErrPath
	}
	return string(p), nil
}

// reservedRootNames are files a Markdown client treats as its own
// configuration or runs as code. SilverBullet executes CONTAINER_BOOT.md
// as a shell script when its container starts, and reads CONFIG.md,
// SETTINGS.md and PLUGS.md as configuration. An agent must not be able
// to create or change any of them.
var reservedRootNames = map[string]bool{
	"container_boot.md": true,
	"config.md":         true,
	"settings.md":       true,
	"plugs.md":          true,
}

// checkWritable refuses a path an agent may not write: a reserved root
// file, or anything under a component starting with "_", which
// SilverBullet (and several other clients) use for plugs and internals.
func checkWritable(p string) error {
	if !strings.Contains(p, "/") && reservedRootNames[strings.ToLower(p)] {
		return ErrReserved
	}
	for _, part := range strings.Split(p, "/") {
		if strings.HasPrefix(part, "_") {
			return ErrReserved
		}
	}
	return nil
}

// CheckContent refuses content an agent may not write.
//
// Some Markdown clients execute parts of a note: SilverBullet runs
// space-lua, space-script and space-style fenced blocks and ${...}
// expressions in the browser of whoever opens the page. A note an agent
// wrote must never carry code into a person's session, so any of those
// is refused outright rather than escaped — escaping would need to know
// every client's syntax, and refusing needs only to know the openers.
func CheckContent(content string) error {
	if len(content) > MaxWriteBytes {
		return fmt.Errorf("%w: at most %d bytes", ErrTooLarge, MaxWriteBytes)
	}
	if !utf8.ValidString(content) {
		return fmt.Errorf("%w: not valid UTF-8", ErrContent)
	}
	for _, r := range content {
		if r == 0 || (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') {
			return fmt.Errorf("%w: it contains control characters", ErrContent)
		}
	}
	if strings.Contains(content, "${") {
		return fmt.Errorf("%w: \"${\" starts an expression some note apps run as code", ErrContent)
	}
	// Anywhere in a line, not only at its start: a fence inside a block
	// quote ("> ```space-lua") or a list item ("- ```space-lua") is still
	// a fenced block to a Markdown parser. Refusing a mention of one in
	// running text too is the price of not parsing Markdown here.
	if opensActiveFence(content) {
		// Generic on purpose: the refusal ends up in the operator's audit
		// stream, and echoing the fence would copy note text into it.
		return fmt.Errorf("%w: it contains a space-* code block, which some note apps run", ErrContent)
	}
	return nil
}

// opensActiveFence reports whether content contains a run of three or
// more backticks or tildes followed, after optional spaces, by "space-".
func opensActiveFence(content string) bool {
	lower := strings.ToLower(content)
	for i := 0; i+3 <= len(lower); i++ {
		c := lower[i]
		if c != '`' && c != '~' || lower[i+1] != c || lower[i+2] != c {
			continue
		}
		j := i + 3
		for j < len(lower) && lower[j] == c {
			j++
		}
		for j < len(lower) && (lower[j] == ' ' || lower[j] == '\t') {
			j++
		}
		if strings.HasPrefix(lower[j:], "space-") {
			return true
		}
		i = j - 1
	}
	return false
}

// checkNoLinks refuses a path any of whose existing components is not a
// plain directory (or, for the last one, a regular file). It reports
// whether the leaf exists.
func checkNoLinks(root *os.Root, p string) (fs.FileInfo, error) {
	parts := strings.Split(p, "/")
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		fi, err := root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		last := i == len(parts)-1
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			return nil, ErrPath
		case !last && !fi.IsDir():
			return nil, ErrPath
		case last:
			return fi, nil
		}
	}
	return nil, nil
}

// Read returns a note's content.
func (s *Store) Read(raw string) ([]byte, Note, error) {
	p, err := NotePath(raw)
	if err != nil {
		return nil, Note{}, err
	}
	root, err := s.open()
	if err != nil {
		return nil, Note{}, err
	}
	defer root.Close()
	content, fi, err := readNote(root, p)
	if err != nil {
		return nil, Note{}, err
	}
	return content, noteOf(p, fi, content), nil
}

func readNote(root *os.Root, p string) ([]byte, fs.FileInfo, error) {
	fi, err := checkNoLinks(root, p)
	if err != nil {
		return nil, nil, err
	}
	if fi == nil {
		return nil, nil, ErrNotFound
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, ErrNotNote
	}
	f, err := root.OpenFile(p, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !opened.Mode().IsRegular() {
		return nil, nil, ErrNotNote
	}
	if opened.Size() > MaxReadBytes {
		return nil, nil, fmt.Errorf("%w: larger than %d bytes", ErrTooLarge, MaxReadBytes)
	}
	content, err := io.ReadAll(io.LimitReader(f, MaxReadBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(content) > MaxReadBytes {
		return nil, nil, fmt.Errorf("%w: larger than %d bytes", ErrTooLarge, MaxReadBytes)
	}
	return content, opened, nil
}

func noteOf(p string, fi fs.FileInfo, content []byte) Note {
	return Note{Path: p, Size: int64(len(content)), ModTime: fi.ModTime(), Revision: Revision(content)}
}

// List returns one folder's notes and subfolders: folders first, then
// notes, each by name. Hidden entries, symlinks and non-Markdown files
// are not listed.
func (s *Store) List(rawDir string) ([]Entry, error) {
	dir, err := FolderPath(rawDir)
	if err != nil {
		return nil, err
	}
	root, err := s.open()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	name := dir
	if name == "" {
		name = "."
	} else {
		fi, err := checkNoLinks(root, dir)
		if err != nil {
			return nil, err
		}
		if fi == nil {
			return nil, ErrNotFound
		}
		if !fi.IsDir() {
			return nil, ErrPath
		}
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	des, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, de := range des {
		n := de.Name()
		if strings.HasPrefix(n, ".") || de.Type()&fs.ModeSymlink != 0 {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		rel := path.Join(dir, n)
		switch {
		case fi.IsDir():
			out = append(out, Entry{Path: rel, Dir: true, ModTime: fi.ModTime()})
		case fi.Mode().IsRegular() && strings.HasSuffix(strings.ToLower(n), Extension):
			out = append(out, Entry{Path: rel, Size: fi.Size(), ModTime: fi.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// Search finds notes whose path or content contains query, ignoring
// case. Path matches come first, then the most recently changed. It
// scans at most MaxScanFiles notes and reports whether it stopped early.
func (s *Store) Search(query string) ([]Hit, bool, error) {
	query = strings.TrimSpace(query)
	if len([]rune(query)) < MinQuery {
		return nil, false, ErrQuery
	}
	root, err := s.open()
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	needle := strings.ToLower(query)
	var pathHits, contentHits []Hit
	scanned := 0
	truncated := false
	walkErr := fs.WalkDir(root.FS(), ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			return nil
		}
		if p == "." {
			return nil
		}
		if strings.HasPrefix(de.Name(), ".") {
			if de.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// WalkDir never descends a symlink, and a symlinked file is
		// skipped here because its type is not regular.
		if de.IsDir() || !de.Type().IsRegular() || !strings.HasSuffix(strings.ToLower(p), Extension) {
			return nil
		}
		if scanned >= MaxScanFiles {
			truncated = true
			return fs.SkipAll
		}
		scanned++
		content, fi, err := readNote(root, p)
		if err != nil {
			return nil
		}
		note := noteOf(p, fi, content)
		if strings.Contains(strings.ToLower(p), needle) {
			pathHits = append(pathHits, Hit{Note: note, Snippet: snippet(string(content), -1)})
			return nil
		}
		lower := bytes.ToLower(content)
		if i := bytes.Index(lower, []byte(needle)); i >= 0 {
			contentHits = append(contentHits, Hit{Note: note, Snippet: snippet(string(content), i)})
		}
		return nil
	})
	if walkErr != nil {
		return nil, false, walkErr
	}
	byRecent := func(h []Hit) {
		sort.SliceStable(h, func(i, j int) bool {
			if !h[i].ModTime.Equal(h[j].ModTime) {
				return h[i].ModTime.After(h[j].ModTime)
			}
			return h[i].Path < h[j].Path
		})
	}
	byRecent(pathHits)
	byRecent(contentHits)
	return append(pathHits, contentHits...), truncated, nil
}

// snippet is a one-line excerpt around byte offset at, or the opening of
// the note when at is negative.
func snippet(content string, at int) string {
	start := 0
	if at >= 0 {
		start = at - MaxSnippetRunes/3
		if start < 0 {
			start = 0
		}
		for start > 0 && !utf8.RuneStart(content[start]) {
			start--
		}
	}
	out := strings.Join(strings.Fields(content[start:]), " ")
	runes := []rune(out)
	if len(runes) > MaxSnippetRunes {
		out = string(runes[:MaxSnippetRunes]) + "…"
	}
	if start > 0 {
		out = "…" + out
	}
	return out
}

// Create writes a new note. It refuses, and writes nothing, when a note
// already exists at the path.
func (s *Store) Create(raw, content string) (Note, error) {
	p, err := NotePath(raw)
	if err != nil {
		return Note{}, err
	}
	if err := checkWritable(p); err != nil {
		return Note{}, err
	}
	if err := CheckContent(content); err != nil {
		return Note{}, err
	}
	if err := s.fence(); err != nil {
		return Note{}, err
	}
	root, err := s.open()
	if err != nil {
		return Note{}, err
	}
	defer root.Close()
	existing, err := checkNoLinks(root, p)
	if err != nil {
		return Note{}, err
	}
	if existing != nil {
		return Note{}, ErrExists
	}
	dir := path.Dir(p)
	if err := mkdirs(root, dir); err != nil {
		return Note{}, err
	}
	uid, gid, own := ownerOfDir(root, dir)
	tmp, err := stage(root, dir, []byte(content), fileMode, uid, gid, own)
	if err != nil {
		return Note{}, err
	}
	defer root.Remove(tmp)
	if err := s.fence(); err != nil {
		return Note{}, err
	}
	if err := root.Link(tmp, p); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return Note{}, ErrExists
		}
		return Note{}, fmt.Errorf("publishing the note: %w", err)
	}
	fi, err := root.Lstat(p)
	if err != nil {
		return Note{}, err
	}
	return noteOf(p, fi, []byte(content)), nil
}

// Update replaces a note's content. expected must be the revision the
// caller last read; if the note changed since, nothing is written and
// the error is a *ConflictError naming the current revision. The
// replaced version is kept under HistoryDir first.
func (s *Store) Update(raw, content, expected string) (Note, error) {
	p, err := NotePath(raw)
	if err != nil {
		return Note{}, err
	}
	if err := checkWritable(p); err != nil {
		return Note{}, err
	}
	if err := CheckContent(content); err != nil {
		return Note{}, err
	}
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return Note{}, fmt.Errorf("%w: an update must name the revision it is based on", ErrConflict)
	}
	if err := s.fence(); err != nil {
		return Note{}, err
	}
	root, err := s.open()
	if err != nil {
		return Note{}, err
	}
	defer root.Close()
	current, fi, err := readNote(root, p)
	if err != nil {
		return Note{}, err
	}
	if rev := Revision(current); rev != expected {
		return Note{}, &ConflictError{Current: rev}
	}
	if bytes.Equal(current, []byte(content)) {
		return noteOf(p, fi, current), nil
	}
	uid, gid, own := ownerOfFile(fi)
	if err := s.keep(root, p, current, uid, gid, own); err != nil {
		return Note{}, fmt.Errorf("keeping the previous version: %w", err)
	}
	dir := path.Dir(p)
	tmp, err := stage(root, dir, []byte(content), fi.Mode().Perm()|0o444, uid, gid, own)
	if err != nil {
		return Note{}, err
	}
	defer root.Remove(tmp)
	if err := s.fence(); err != nil {
		return Note{}, err
	}
	// Checked again as late as possible: a person saving in the web UI
	// between the first check and here must not be overwritten.
	again, _, err := readNote(root, p)
	if err != nil {
		return Note{}, err
	}
	if rev := Revision(again); rev != expected {
		return Note{}, &ConflictError{Current: rev}
	}
	if err := root.Rename(tmp, p); err != nil {
		return Note{}, fmt.Errorf("publishing the note: %w", err)
	}
	after, err := root.Lstat(p)
	if err != nil {
		return Note{}, err
	}
	return noteOf(p, after, []byte(content)), nil
}

// Append adds text to the end of an existing note, on a new paragraph.
//
// It needs no revision from the caller because it replaces nothing: what
// was there stays there. It still refuses to publish over a change that
// landed while it was writing, so a person's save is never lost. Only
// the appended text is checked with CheckContent — the note may already
// hold things a person chose to put there.
func (s *Store) Append(raw, text string) (Note, error) {
	p, err := NotePath(raw)
	if err != nil {
		return Note{}, err
	}
	if err := checkWritable(p); err != nil {
		return Note{}, err
	}
	if strings.TrimSpace(text) == "" {
		return Note{}, fmt.Errorf("%w: nothing to add", ErrContent)
	}
	if err := CheckContent(text); err != nil {
		return Note{}, err
	}
	if err := s.fence(); err != nil {
		return Note{}, err
	}
	root, err := s.open()
	if err != nil {
		return Note{}, err
	}
	defer root.Close()
	current, fi, err := readNote(root, p)
	if err != nil {
		return Note{}, err
	}
	base := Revision(current)
	sep := ""
	switch {
	case len(current) == 0:
	case bytes.HasSuffix(current, []byte("\n\n")):
	case bytes.HasSuffix(current, []byte("\n")):
		sep = "\n"
	default:
		sep = "\n\n"
	}
	next := append(append(append([]byte{}, current...), sep...), text...)
	if !strings.HasSuffix(text, "\n") {
		next = append(next, '\n')
	}
	if len(next) > MaxReadBytes {
		return Note{}, fmt.Errorf("%w: the note would be larger than %d bytes", ErrTooLarge, MaxReadBytes)
	}
	uid, gid, own := ownerOfFile(fi)
	tmp, err := stage(root, path.Dir(p), next, fi.Mode().Perm()|0o444, uid, gid, own)
	if err != nil {
		return Note{}, err
	}
	defer root.Remove(tmp)
	if err := s.fence(); err != nil {
		return Note{}, err
	}
	again, _, err := readNote(root, p)
	if err != nil {
		return Note{}, err
	}
	if rev := Revision(again); rev != base {
		return Note{}, &ConflictError{Current: rev}
	}
	if err := root.Rename(tmp, p); err != nil {
		return Note{}, fmt.Errorf("publishing the note: %w", err)
	}
	after, err := root.Lstat(p)
	if err != nil {
		return Note{}, err
	}
	return noteOf(p, after, next), nil
}

// ConflictError is an update refused because the note changed.
type ConflictError struct{ Current string }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s: its current revision is %s", ErrConflict, e.Current)
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

// keep writes the version an update is about to replace to
// .history/<note path without .md>/<UTC time>-<revision>.md.
func (s *Store) keep(root *os.Root, p string, content []byte, uid, gid int, own bool) error {
	dir := path.Join(HistoryDir, strings.TrimSuffix(p, path.Ext(p)))
	if err := mkdirs(root, dir); err != nil {
		return err
	}
	name := path.Join(dir, s.now().UTC().Format("20060102T150405.000000000Z")+"-"+Revision(content)+Extension)
	tmp, err := stage(root, dir, content, fileMode, uid, gid, own)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if err := root.Link(tmp, name); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// mkdirs creates each missing directory of dir with dirMode and its
// parent's owner, refusing to pass through anything that is not a plain
// directory.
func mkdirs(root *os.Root, dir string) error {
	if dir == "." || dir == "" {
		return nil
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		fi, err := root.Lstat(prefix)
		if err == nil {
			if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
				return ErrPath
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		parent := path.Dir(prefix)
		uid, gid, own := ownerOfDir(root, parent)
		if err := root.Mkdir(prefix, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := root.Chmod(prefix, dirMode); err != nil {
			return err
		}
		if own {
			if err := root.Lchown(prefix, uid, gid); err != nil {
				return err
			}
		}
	}
	return nil
}

// stage writes content to a hidden temporary file in dir, synced, with
// mode and (when own is set) owner already applied, and returns its
// path. The caller publishes it by link or rename and removes it.
func stage(root *os.Root, dir string, content []byte, mode os.FileMode, uid, gid int, own bool) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	tmp := path.Join(dir, ".wildstone-tmp-"+hex.EncodeToString(suffix[:]))
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, fileMode)
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		f.Close()
		root.Remove(tmp)
		return "", err
	}
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if own {
		if err := f.Chown(uid, gid); err != nil {
			return fail(err)
		}
	}
	if _, err := f.Write(content); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		root.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

func ownerOfDir(root *os.Root, dir string) (int, int, bool) {
	if dir == "" {
		dir = "."
	}
	fi, err := root.Stat(dir)
	if err != nil {
		return 0, 0, false
	}
	return ownerOfFile(fi)
}
