//go:build windows

package agent

import (
	"os/exec"
	"time"
)

// Wardstone is deployed on Linux. Keep cross-compilation working on Windows;
// CommandContext still terminates the direct child and WaitDelay bounds any
// inherited output pipes there.
func configureProcessTreeCancellation(cmd *exec.Cmd) {
	cmd.WaitDelay = 5 * time.Second
}
