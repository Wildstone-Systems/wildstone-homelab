package statefile

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// CheckLocalInput rejects source checkouts, shared Git metadata, remotes and
// non-resource tracked files before the opt-in Helm publisher uses this store.
// Canonical Statefile owns its own .git; only its ancestors are source-forbidden.
func (s *Store) CheckLocalInput(revision string) error {
	root := filepath.Clean(s.Root)
	if !filepath.IsAbs(root) {
		return fmt.Errorf("deployment: absolute local Statefile required")
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil || real != root {
		return fmt.Errorf("deployment: symlinked Statefile refused")
	}
	for dir := filepath.Dir(root); ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return fmt.Errorf("deployment: Statefile is inside a source checkout")
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	info, err := os.Lstat(filepath.Join(root, ".git"))
	if err != nil || !info.IsDir() {
		return fmt.Errorf("deployment: dedicated local Statefile Git directory required")
	}
	for _, path := range []string{"go.mod", "package.json", "CONTRIBUTING.md", ".github", ".git/objects/info/alternates", ".git/objects/info/http-alternates", ".git/shallow"} {
		if _, err := os.Lstat(filepath.Join(root, path)); err == nil {
			return fmt.Errorf("deployment: source/shared Statefile repository refused")
		}
	}
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root, "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		for _, e := range os.Environ() {
			if !strings.HasPrefix(e, "GIT_") {
				cmd.Env = append(cmd.Env, e)
			}
		}
		cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	remotes, err := run("remote")
	if err != nil || remotes != "" {
		return fmt.Errorf("deployment: Statefile remotes refused")
	}
	if revision == "" {
		head, err := s.Head()
		if err != nil {
			return err
		}
		if head != "" {
			return ErrNoAcceptedRevision
		}
		return nil
	}
	files, err := run("ls-tree", "-r", "--name-only", revision)
	if err != nil {
		return fmt.Errorf("deployment: accepted Statefile unavailable")
	}
	kinds := map[string]bool{}
	for _, k := range resource.KnownKinds() {
		kinds[strings.ToLower(string(k))] = true
	}
	for _, p := range strings.Split(files, "\n") {
		if p == "" {
			continue
		}
		parts := strings.Split(p, "/")
		if len(parts) != 2 || !kinds[parts[0]] || !strings.HasSuffix(parts[1], ".yaml") {
			return fmt.Errorf("deployment: only canonical resource files may be published")
		}
	}
	return nil
}
