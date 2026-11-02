package localgit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// MirrorHead is a bounded read of a dedicated delivery repository. It says
// nothing about SSH availability, Flux reconciliation or application readiness.
func MirrorHead(root string) (string, error) {
	if err := CheckLocalPath(root); err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("local source: private mirror unavailable")
	}
	marker, err := os.ReadFile(filepath.Join(root, mirrorMarker))
	if err != nil || string(marker) != "1\n" {
		return "", fmt.Errorf("local source: dedicated mirror unavailable")
	}
	for _, name := range []string{"objects/info/alternates", "objects/info/http-alternates", "shallow"} {
		if _, err := os.Lstat(filepath.Join(root, name)); err == nil {
			return "", fmt.Errorf("local source: shared/partial mirror refused")
		}
	}
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + root, "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		for _, e := range os.Environ() {
			if !strings.HasPrefix(e, "GIT_") {
				cmd.Env = append(cmd.Env, e)
			}
		}
		cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	bare, err := run("rev-parse", "--is-bare-repository")
	if err != nil || bare != "true" {
		return "", fmt.Errorf("local source: bare mirror required")
	}
	remotes, err := run("remote")
	if err != nil || remotes != "" {
		return "", fmt.Errorf("local source: mirror remotes refused")
	}
	refs, err := run("for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/deploy")
	if err != nil {
		return "", fmt.Errorf("local source: mirror ref unavailable")
	}
	for _, line := range strings.Split(refs, "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[0] == "refs/heads/deploy" {
			return parts[1], nil
		}
	}
	return "", nil
}
