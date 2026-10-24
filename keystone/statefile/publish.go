package statefile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// Publication plumbing (contract D2/D3, ADR-W007 A8).
//
// A candidate revision is built as a commit object from the accepted
// tree, through a private index, without touching the working files: an
// uncommitted edit can never ride into it, and building one never moves
// the branch. Only after the runtime DB has accepted a candidate does the
// branch catch up, and only by fast-forward, so a human's commits are
// never rewound or overwritten.

// PublicationRefPrefix is the reserved namespace that keeps every
// prepared or accepted candidate reachable whatever happens to the
// human-facing branch. `git bundle create --all` carries it.
const PublicationRefPrefix = "refs/wildstone/publications/"

// ErrDiverged is Mirror's answer when the branch holds commits the
// accepted revision does not descend from: a human's history Keystone
// will not overwrite.
var ErrDiverged = errors.New("statefile: the branch has diverged from the accepted revision")

const zeroID = "0000000000000000000000000000000000000000"

// git runs git with extra environment and stdin. It is the plumbing
// runner: unlike the injectable seam it never needs faking, because every
// caller is exercised against a real repository.
func (s *Store) gitWith(ctx context.Context, env []string, stdin []byte, args ...string) (string, error) {
	args = append([]string{"-c", "gc.auto=0"}, args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = s.Root
	cmd.Env = append(os.Environ(), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %v: %w: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// relPath is the in-repository path for (kind, name), matching Put.
func relPath(kind resource.Kind, name string) string {
	return strings.ToLower(string(kind)) + "/" + name + ".yaml"
}

// Tree returns the tree object id of commit: the canonical digest of its
// resource paths and bytes.
func (s *Store) Tree(commit string) (string, error) {
	return s.gitWith(context.Background(), nil, nil, "rev-parse", "--verify", commit+"^{tree}")
}

// BuildCandidate creates a commit whose tree is base's with puts written
// and deletes removed, parented on base ("" for an empty repository), and
// returns it. The working tree, the index and the branch are untouched.
// When the change leaves the tree as it was, base is returned and no
// commit is made.
func (s *Store) BuildCandidate(base string, puts []resource.Resource, deletes []ResourceRef, who, planRef, why string) (string, error) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "wildstone-index-")
	if err != nil {
		return "", fmt.Errorf("statefile: candidate index: %w", err)
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}

	if base != "" {
		if _, err := s.gitWith(ctx, env, nil, "read-tree", base); err != nil {
			return "", fmt.Errorf("statefile: candidate from %s: %w", base, err)
		}
	} else if _, err := s.gitWith(ctx, env, nil, "read-tree", "--empty"); err != nil {
		return "", fmt.Errorf("statefile: empty candidate: %w", err)
	}
	for _, r := range puts {
		if err := r.Validate(); err != nil {
			return "", fmt.Errorf("statefile: candidate: %w", err)
		}
		data, err := resource.Marshal(r)
		if err != nil {
			return "", fmt.Errorf("statefile: marshaling %s/%s: %w", r.Kind, r.Metadata.Name, err)
		}
		blob, err := s.gitWith(ctx, nil, data, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", fmt.Errorf("statefile: writing %s/%s: %w", r.Kind, r.Metadata.Name, err)
		}
		if _, err := s.gitWith(ctx, env, nil, "update-index", "--add", "--cacheinfo",
			"100644,"+blob+","+relPath(r.Kind, r.Metadata.Name)); err != nil {
			return "", fmt.Errorf("statefile: staging %s/%s: %w", r.Kind, r.Metadata.Name, err)
		}
	}
	for _, d := range deletes {
		if _, err := s.gitWith(ctx, env, nil, "update-index", "--force-remove", relPath(d.Kind, d.Name)); err != nil {
			return "", fmt.Errorf("statefile: removing %s/%s: %w", d.Kind, d.Name, err)
		}
	}
	tree, err := s.gitWith(ctx, env, nil, "write-tree")
	if err != nil {
		return "", fmt.Errorf("statefile: candidate tree: %w", err)
	}
	if base != "" {
		baseTree, err := s.Tree(base)
		if err != nil {
			return "", err
		}
		if baseTree == tree {
			return base, nil
		}
	}
	return s.commitTree(tree, base, commitMessage(who, planRef, why))
}

// BuildRevert creates a commit on top of head whose tree is accepted's: the
// ADR-W007 A6 restoring commit, built without rewriting the working files.
func (s *Store) BuildRevert(accepted, head, who, why string) (string, error) {
	tree, err := s.Tree(accepted)
	if err != nil {
		return "", err
	}
	return s.commitTree(tree, head, commitMessage(who, "", why))
}

func (s *Store) commitTree(tree, parent, msg string) (string, error) {
	args := []string{"-c", "user.name=wildstone-keystone", "-c", "user.email=keystone@localhost", "commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	sha, err := s.gitWith(context.Background(), nil, []byte(msg), args...)
	if err != nil {
		return "", fmt.Errorf("statefile: candidate commit: %w", err)
	}
	return sha, nil
}

func commitMessage(who, what, why string) string {
	subject := why
	if subject == "" {
		subject = "statefile update"
	}
	return fmt.Sprintf("%s\n\nWho: %s\nPlan: %s\nWhy: %s\n", subject, who, what, why)
}

// CreateRef points a new reserved ref at sha. It refuses to move an
// existing one: a publication id names one candidate forever.
func (s *Store) CreateRef(name, sha string) error {
	if _, err := s.gitWith(context.Background(), nil, nil, "update-ref", name, sha, zeroID); err != nil {
		return fmt.Errorf("statefile: creating %s: %w", name, err)
	}
	return nil
}

// RefTarget returns what name points at, or "" when it does not exist.
func (s *Store) RefTarget(name string) (string, error) {
	out, err := s.gitWith(context.Background(), nil, nil, "for-each-ref", "--format=%(objectname)", name)
	if err != nil {
		return "", fmt.Errorf("statefile: reading %s: %w", name, err)
	}
	return out, nil
}

// DeleteRef removes name if it still points at sha.
func (s *Store) DeleteRef(name, sha string) error {
	if _, err := s.gitWith(context.Background(), nil, nil, "update-ref", "-d", name, sha); err != nil {
		return fmt.Errorf("statefile: deleting %s: %w", name, err)
	}
	return nil
}

// HasCommit reports whether rev resolves to a commit object here.
func (s *Store) HasCommit(rev string) bool {
	_, err := s.gitWith(context.Background(), nil, nil, "cat-file", "-e", rev+"^{commit}")
	return err == nil
}

// IsAncestor reports whether ancestor is reachable from descendant ("" is
// the ancestor of everything: an empty repository).
func (s *Store) IsAncestor(ancestor, descendant string) (bool, error) {
	if ancestor == "" || ancestor == descendant {
		return true, nil
	}
	if descendant == "" {
		return false, nil
	}
	_, err := s.gitWith(context.Background(), nil, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("statefile: comparing %s and %s: %w", ancestor, descendant, err)
}

// Mirror brings the branch and working files to accepted by fast-forward.
// It never rewinds: a branch holding commits accepted does not contain is
// ErrDiverged, and is left as it is. Local edits survive where they do
// not conflict (`git reset --keep`); a conflicting one stops the mirror,
// which recovery retries.
func (s *Store) Mirror(accepted string) error {
	head, err := s.Head()
	if err != nil {
		return err
	}
	if head == accepted {
		return nil
	}
	// A branch already ahead of accepted holds commits a human made on
	// top: ingestion's to check, nothing for a mirror to do.
	if head != "" && accepted != "" {
		ahead, err := s.IsAncestor(accepted, head)
		if err != nil {
			return err
		}
		if ahead {
			return nil
		}
	}
	ok, err := s.IsAncestor(head, accepted)
	if err != nil {
		return err
	}
	if !ok {
		return ErrDiverged
	}
	ctx := context.Background()
	if head == "" {
		// An unborn branch has no HEAD for reset to keep against: check
		// the tree out with a one-way merge, which refuses to overwrite an
		// untracked file, then create the branch only if it still does not
		// exist.
		if _, err := s.gitWith(ctx, nil, nil, "read-tree", "-m", "-u", accepted); err != nil {
			return fmt.Errorf("statefile: mirroring %s: %w", accepted, err)
		}
		if _, err := s.gitWith(ctx, nil, nil, "update-ref", "HEAD", accepted, zeroID); err != nil {
			return fmt.Errorf("statefile: mirroring %s: %w", accepted, err)
		}
		return nil
	}
	if mirrorRace != nil {
		mirrorRace()
	}
	// The branch moves only if it still holds the head checked above: a
	// person may commit at any moment, and the publisher's lock does not
	// cover them. The ref moves first, conditionally, and the files follow
	// with a two-way merge that keeps local edits (as reset --keep would);
	// if the files cannot follow, the ref goes back the same way.
	if _, err := s.gitWith(ctx, nil, nil, "update-ref", "HEAD", accepted, head); err != nil {
		if now, herr := s.Head(); herr == nil && now != head {
			return ErrDiverged
		}
		return fmt.Errorf("statefile: mirroring %s: %w", accepted, err)
	}
	_, _ = s.gitWith(ctx, nil, nil, "update-index", "-q", "--refresh")
	if _, err := s.gitWith(ctx, nil, nil, "read-tree", "-m", "-u", head, accepted); err != nil {
		if _, rerr := s.gitWith(ctx, nil, nil, "update-ref", "HEAD", head, accepted); rerr != nil {
			return fmt.Errorf("statefile: mirroring %s: %w (and restoring the branch: %v)", accepted, err, rerr)
		}
		return fmt.Errorf("statefile: mirroring %s: %w", accepted, err)
	}
	return nil
}

// mirrorRace, when set by a test, runs between Mirror's ancestry check and
// its branch update: where a concurrent human commit would land.
var mirrorRace func()
