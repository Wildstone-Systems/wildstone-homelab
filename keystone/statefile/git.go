package statefile

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// defaultGitRunner execs the real `git` binary in dir with args, returning
// combined stdout (stderr included on error for diagnostics).
//
// Every invocation forces `-c gc.auto=0`: git otherwise fires an automatic
// `git gc --auto` after commits once enough loose objects accumulate, and
// that gc detaches into a background process that keeps repacking
// .git/objects after the foreground command has returned. For a Statefile
// that a caller may tear down (or a test's t.TempDir may RemoveAll) right
// after a commit, that background writer races the teardown. Disabling
// auto-gc keeps every git operation fully synchronous; the desired-state
// repo is small and never needs opportunistic gc.
func defaultGitRunner(ctx context.Context, dir string, args ...string) (string, error) {
	args = append([]string{"-c", "gc.auto=0", "-c", "core.hooksPath=" + os.DevNull}, args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = isolatedGitEnvironment(nil)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %v: %w: %s", args, err, stderr.String())
	}
	return stdout.String(), nil
}

// defaultGitInputRunner is defaultGitRunner with stdin, returning raw
// stdout bytes (blob contents are not text).
func defaultGitInputRunner(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	args = append([]string{"-c", "gc.auto=0", "-c", "core.hooksPath=" + os.DevNull}, args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = isolatedGitEnvironment(nil)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("git %v: %w: %s", args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// Product and derived repositories must not inherit a developer checkout's
// Git redirection, global hooks, config injection or tracing. Extra variables
// are private plumbing inputs such as the temporary index, never API fields.
func isolatedGitEnvironment(extra []string) []string {
	var out []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			out = append(out, e)
		}
	}
	out = append(out, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	return append(out, extra...)
}
