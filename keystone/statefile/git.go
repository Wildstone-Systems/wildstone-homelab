package statefile

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// defaultGitRunner execs the real `git` binary in dir with args, returning
// combined stdout (stderr included on error for diagnostics).
func defaultGitRunner(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %v: %w: %s", args, err, stderr.String())
	}
	return stdout.String(), nil
}
