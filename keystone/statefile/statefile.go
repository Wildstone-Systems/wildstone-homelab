// Package statefile is Keystone's git-backed desired-state store
// (ADR-W004 A2). Every accepted resource change is a file write plus a
// structured git commit; status is never written — the Statefile only ever
// holds resource.Resource, which has no status field.
package statefile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// gitRunner execs the git binary against a working directory. Tests can
// inject a fake; the zero-value Store uses defaultGitRunner, which runs the
// real os/exec git (present in CI and on the VM per ADR-W004 A2).
type gitRunner func(ctx context.Context, dir string, args ...string) (stdout string, err error)

// Store is a git repository at Root holding one YAML file per resource at
// <kind-lowercased>/<name>.yaml.
type Store struct {
	Root string
	git  gitRunner
}

// New opens (or initializes) a Statefile store rooted at root. If root is
// not already a git repository, `git init` is run.
func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("statefile: creating root %s: %w", root, err)
	}
	s := &Store{Root: root, git: defaultGitRunner}
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

// Load reads and parses every resource YAML file under Root, sorted by
// (kind, name) for determinism.
func (s *Store) Load() ([]resource.Resource, error) {
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
// sorted by (kind, name) — the historical sibling of Load, used by the
// out-of-band ingestion loop (ADR-W007 A6) to diff a foreign edit against
// the last accepted tree without touching the working copy.
func (s *Store) LoadAt(commit string) ([]resource.Resource, error) {
	ctx := context.Background()
	listing, err := s.git(ctx, s.Root, "ls-tree", "-r", "--name-only", commit)
	if err != nil {
		return nil, fmt.Errorf("statefile: ls-tree %s: %w", commit, err)
	}
	var out []resource.Resource
	for _, path := range strings.Split(listing, "\n") {
		path = strings.TrimSpace(path)
		if path == "" || filepath.Ext(path) != ".yaml" {
			continue
		}
		data, err := s.git(ctx, s.Root, "show", commit+":"+path)
		if err != nil {
			return nil, fmt.Errorf("statefile: show %s:%s: %w", commit, path, err)
		}
		r, err := resource.Unmarshal([]byte(data))
		if err != nil {
			return nil, fmt.Errorf("statefile: parsing %s at %s: %w", path, commit, err)
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
