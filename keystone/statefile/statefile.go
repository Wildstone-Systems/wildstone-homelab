// Package statefile is Keystone's git-backed desired-state store
// (ADR-W004 A2). Every accepted resource change is a file write plus a
// structured git commit; status is never written — the Statefile only ever
// holds resource.Resource, which has no status field.
package statefile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// gitRunner execs the git binary against a working directory. Tests can
// inject a fake; the zero-value Store uses defaultGitRunner, which runs the
// real os/exec git (present in CI and on the VM per ADR-W004 A2).
type gitRunner func(ctx context.Context, dir string, args ...string) (stdout string, err error)

// gitInputRunner is gitRunner with stdin, for the one batch read LoadAt
// makes (`git cat-file --batch`).
type gitInputRunner func(ctx context.Context, dir string, stdin []byte, args ...string) (stdout []byte, err error)

// ErrNoAcceptedRevision is Load's answer when the repository has history
// but no revision has been accepted yet: a restored or upgraded control
// plane before its first ingestion tick. Reading the working files instead
// would make an unvalidated tree live; reading nothing would plan the
// removal of everything. Callers wait.
var ErrNoAcceptedRevision = errors.New("statefile: no accepted revision yet")

// Store is a git repository at Root holding one YAML file per resource at
// <kind-lowercased>/<name>.yaml.
type Store struct {
	Root string

	// Accepted names the accepted revision: the immutable commit that is
	// desired state (ADR-W007 A6, ADR-W008 A1). When set, Load reads that
	// commit and nothing else, so an uncommitted edit, or a committed one
	// the ingestion loop has not yet accepted, is never desired state.
	// cmd/keystone sets it to the runtime DB's statefile_head. nil keeps
	// the working-tree read, which only tests rely on.
	Accepted func() (string, error)

	git   gitRunner
	gitIn gitInputRunner

	// tree caches the files of the last commit LoadAt read. A commit is
	// immutable, so the cache needs no invalidation: a new accepted
	// revision is a different key. Parsed fresh on every call, because
	// callers own the resources they are handed.
	mu   sync.Mutex
	tree treeCache
}

type treeCache struct {
	commit string
	files  []treeFile
}

type treeFile struct {
	path string
	data []byte
}

// New opens (or initializes) a Statefile store rooted at root. If root is
// not already a git repository, `git init` is run.
func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("statefile: creating root %s: %w", root, err)
	}
	s := &Store{Root: root, git: defaultGitRunner, gitIn: defaultGitInputRunner}
	if _, err := os.Stat(filepath.Join(root, ".git")); os.IsNotExist(err) {
		if _, err := s.git(context.Background(), root, "init"); err != nil {
			return nil, fmt.Errorf("statefile: git init: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("statefile: stat .git: %w", err)
	}
	return s, nil
}

// path returns the on-disk path for a (kind, name) resource.
func (s *Store) path(kind resource.Kind, name string) string {
	return filepath.Join(s.Root, strings.ToLower(string(kind)), name+".yaml")
}

// Put writes r's YAML to <root>/<kind>/<name>.yaml, creating parent
// directories as needed. It does not commit; call Commit afterward.
func (s *Store) Put(r resource.Resource) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("statefile: put: %w", err)
	}
	p := s.path(r.Kind, r.Metadata.Name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("statefile: creating dir for %s: %w", p, err)
	}
	data, err := resource.Marshal(r)
	if err != nil {
		return fmt.Errorf("statefile: marshaling %s/%s: %w", r.Kind, r.Metadata.Name, err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return fmt.Errorf("statefile: writing %s: %w", p, err)
	}
	return nil
}

// Delete removes the file for (kind, name). It does not commit; call
// Commit afterward. Deleting a resource that does not exist is not an
// error.
func (s *Store) Delete(kind resource.Kind, name string) error {
	p := s.path(kind, name)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("statefile: deleting %s: %w", p, err)
	}
	return nil
}

// ResourceRef identifies a resource by kind and name without its spec.
type ResourceRef struct {
	Kind resource.Kind
	Name string
}

// List returns refs for every resource currently on disk, sorted by
// (kind, name) for determinism.
func (s *Store) List() ([]ResourceRef, error) {
	resources, err := s.Load()
	if err != nil {
		return nil, err
	}
	refs := make([]ResourceRef, 0, len(resources))
	for _, r := range resources {
		refs = append(refs, ResourceRef{Kind: r.Kind, Name: r.Metadata.Name})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		return refs[i].Name < refs[j].Name
	})
	return refs, nil
}

// Load returns desired state, sorted by (kind, name) for determinism:
// the resources at the accepted revision when Accepted is set, the
// working files otherwise. An empty repository is an empty desired
// state; a repository with history and no accepted revision is
// ErrNoAcceptedRevision.
func (s *Store) Load() ([]resource.Resource, error) {
	if s.Accepted == nil {
		return s.LoadWorking()
	}
	rev, err := s.AcceptedRevision()
	if err != nil {
		return nil, err
	}
	if rev == "" {
		return nil, nil
	}
	return s.LoadAt(rev)
}

// AcceptedRevision returns the commit Load reads, "" for an empty
// repository, or ErrNoAcceptedRevision when there is history but nothing
// has been accepted. Without an Accepted source it reports HEAD, the
// commit the working-tree read most closely matches.
func (s *Store) AcceptedRevision() (string, error) {
	if s.Accepted == nil {
		return s.Head()
	}
	rev, err := s.Accepted()
	if err != nil {
		return "", fmt.Errorf("statefile: reading accepted revision: %w", err)
	}
	if rev != "" {
		return rev, nil
	}
	head, err := s.Head()
	if err != nil {
		return "", err
	}
	if head != "" {
		return "", ErrNoAcceptedRevision
	}
	return "", nil
}

// Snapshot returns the accepted revision and the resources at it, read
// together so a caller can say which immutable intent it acted on. The
// revision is "" for an empty repository. Without an Accepted source it
// reports HEAD beside the working files, the pre-ADR pairing.
func (s *Store) Snapshot() (string, []resource.Resource, error) {
	if s.Accepted == nil {
		head, err := s.Head()
		if err != nil {
			return "", nil, err
		}
		resources, err := s.LoadWorking()
		return head, resources, err
	}
	rev, err := s.AcceptedRevision()
	if err != nil || rev == "" {
		return "", nil, err
	}
	resources, err := s.LoadAt(rev)
	return rev, resources, err
}

// LoadWorking reads and parses every resource YAML file under Root, sorted
// by (kind, name). The working files are not desired state once Accepted
// is set; this is the pre-ADR read, kept for the stores that have no
// accepted-revision source.
func (s *Store) LoadWorking() ([]resource.Resource, error) {
	var out []resource.Resource
	err := filepath.WalkDir(s.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".yaml" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("statefile: reading %s: %w", path, err)
		}
		r, err := resource.Unmarshal(data)
		if err != nil {
			return fmt.Errorf("statefile: parsing %s: %w", path, err)
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Metadata.Name < out[j].Metadata.Name
	})
	return out, nil
}

// Head returns the current HEAD commit hash, or "" for a repository with
// no commits yet (fresh install). Consumers: the plan-approval flow and
// the out-of-band ingestion loop (ADR-W007 A6) compare it against the
// runtime DB's statefile_head marker.
func (s *Store) Head() (string, error) {
	out, err := s.git(context.Background(), s.Root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		// An unborn branch (no commits yet) is not an error condition for
		// callers; anything else is.
		if strings.Contains(err.Error(), "Needed a single revision") ||
			strings.Contains(err.Error(), "unknown revision") ||
			strings.Contains(err.Error(), "ambiguous argument") {
			return "", nil
		}
		return "", fmt.Errorf("statefile: rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// LoadAt reads and parses every resource YAML file in the tree at commit,
// sorted by (kind, name), without touching the working copy. It is the
// accepted-revision read behind Load, and the ingestion loop's read of
// the last accepted tree and of a candidate commit (ADR-W007 A6).
//
// Two git invocations however many resources there are: one listing, one
// batched blob read. Load runs on every API read of desired state, so a
// process per file would not do.
func (s *Store) LoadAt(commit string) ([]resource.Resource, error) {
	files, err := s.treeFiles(commit)
	if err != nil {
		return nil, err
	}
	out := make([]resource.Resource, 0, len(files))
	for _, f := range files {
		r, err := resource.Unmarshal(f.data)
		if err != nil {
			return nil, fmt.Errorf("statefile: parsing %s at %s: %w", f.path, commit, err)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Metadata.Name < out[j].Metadata.Name
	})
	return out, nil
}

// treeFiles returns the .yaml blobs in commit's tree, from the cache when
// commit is the one last read.
func (s *Store) treeFiles(commit string) ([]treeFile, error) {
	if commit == "" {
		return nil, fmt.Errorf("statefile: commit required")
	}
	s.mu.Lock()
	if s.tree.commit == commit {
		files := s.tree.files
		s.mu.Unlock()
		return files, nil
	}
	s.mu.Unlock()

	ctx := context.Background()
	// --full-tree lists from the repository root whatever the cwd; -z keeps
	// paths verbatim.
	listing, err := s.git(ctx, s.Root, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return nil, fmt.Errorf("statefile: ls-tree %s: %w", commit, err)
	}
	var paths []string
	var request bytes.Buffer
	for _, entry := range strings.Split(listing, "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok || filepath.Ext(path) != ".yaml" {
			continue
		}
		fields := strings.Fields(meta) // mode, type, object
		if len(fields) != 3 || fields[1] != "blob" {
			continue
		}
		paths = append(paths, path)
		request.WriteString(fields[2])
		request.WriteByte('\n')
	}
	var files []treeFile
	if len(paths) > 0 {
		raw, err := s.gitIn(ctx, s.Root, request.Bytes(), "cat-file", "--batch")
		if err != nil {
			return nil, fmt.Errorf("statefile: reading blobs at %s: %w", commit, err)
		}
		blobs, err := parseCatFileBatch(raw, len(paths))
		if err != nil {
			return nil, fmt.Errorf("statefile: reading blobs at %s: %w", commit, err)
		}
		files = make([]treeFile, len(paths))
		for i := range paths {
			files[i] = treeFile{path: paths[i], data: blobs[i]}
		}
	}

	// Only a full object id names one immutable tree; "HEAD" or a branch
	// name would serve a stale cache after the next commit.
	if isObjectID(commit) {
		s.mu.Lock()
		s.tree = treeCache{commit: commit, files: files}
		s.mu.Unlock()
	}
	return files, nil
}

// isObjectID reports whether rev is a full SHA-1 or SHA-256 object id.
func isObjectID(rev string) bool {
	if len(rev) != 40 && len(rev) != 64 {
		return false
	}
	for _, c := range rev {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// parseCatFileBatch splits `git cat-file --batch` output, a sequence of
// "<object> <type> <size>\n<content>\n" records, into want contents.
func parseCatFileBatch(raw []byte, want int) ([][]byte, error) {
	out := make([][]byte, 0, want)
	for len(out) < want {
		nl := bytes.IndexByte(raw, '\n')
		if nl < 0 {
			return nil, fmt.Errorf("truncated header after %d of %d objects", len(out), want)
		}
		header := strings.Fields(string(raw[:nl]))
		if len(header) != 3 {
			return nil, fmt.Errorf("unexpected header %q", raw[:nl])
		}
		size, err := strconv.Atoi(header[2])
		if err != nil || size < 0 || nl+1+size+1 > len(raw) {
			return nil, fmt.Errorf("bad size in header %q", raw[:nl])
		}
		body := raw[nl+1 : nl+1+size]
		out = append(out, body)
		raw = raw[nl+1+size+1:]
	}
	return out, nil
}

// Dirty reports whether the working tree differs from HEAD: uncommitted
// or untracked files. Keystone's own writes commit everything they stage
// (`git add -A`), so a write on a dirty tree would take a hand edit
// nobody reviewed into its commit.
func (s *Store) Dirty() (bool, error) {
	status, err := s.git(context.Background(), s.Root, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("statefile: git status: %w", err)
	}
	return strings.TrimSpace(status) != "", nil
}

// Subject returns a commit's subject line, for naming foreign edits in
// plan records and events.
func (s *Store) Subject(commit string) (string, error) {
	out, err := s.git(context.Background(), s.Root, "log", "-1", "--format=%s", commit)
	if err != nil {
		return "", fmt.Errorf("statefile: log %s: %w", commit, err)
	}
	return strings.TrimSpace(out), nil
}

// RevertTo records a new commit whose tree equals the tree at commit —
// the ADR-W007 A6 response to an invalid out-of-band edit. It never
// rewrites history: the foreign commits stay reachable in the log and the
// human's work stays recoverable. Implemented as restore-tree-and-commit
// rather than `git revert` so a run of foreign commits can never produce
// revert conflicts.
func (s *Store) RevertTo(commit, who, why string) error {
	ctx := context.Background()
	// Drop every tracked file, then restore the target tree over it: the
	// combination handles both modified and newly-added files.
	if _, err := s.git(ctx, s.Root, "rm", "-rq", "--ignore-unmatch", "."); err != nil {
		return fmt.Errorf("statefile: revert rm: %w", err)
	}
	if _, err := s.git(ctx, s.Root, "checkout", commit, "--", "."); err != nil {
		return fmt.Errorf("statefile: revert checkout %s: %w", commit, err)
	}
	return s.Commit(who, "", why)
}

// Commit stages all changes under Root and records a structured commit:
// a subject line followed by trailing "Who:"/"Plan:"/"Why:" metadata
// lines, using a per-invocation git identity so no global git config is
// required (ADR-W004 A2). Returns nil without committing if there is
// nothing staged.
func (s *Store) Commit(who, what, why string) error {
	ctx := context.Background()
	if _, err := s.git(ctx, s.Root, "add", "-A"); err != nil {
		return fmt.Errorf("statefile: git add: %w", err)
	}
	status, err := s.git(ctx, s.Root, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("statefile: git status: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		return nil
	}

	subject := why
	if subject == "" {
		subject = "statefile update"
	}
	msg := fmt.Sprintf("%s\n\nWho: %s\nPlan: %s\nWhy: %s\n", subject, who, what, why)

	args := []string{
		"-c", "user.name=wildstone-keystone",
		"-c", "user.email=keystone@localhost",
		"commit", "-m", msg,
	}
	if _, err := s.git(ctx, s.Root, args...); err != nil {
		return fmt.Errorf("statefile: git commit: %w", err)
	}
	return nil
}
