//go:build linux

package agent

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const processTreeWaitDelay = 5 * time.Second

// configureProcessTreeCancellation puts Ansible and every process it starts
// in a dedicated process group. CommandContext's cancellation then kills the
// whole group, not only ansible-playbook; WaitDelay also bounds cleanup if a
// descendant inherited an output pipe or cannot be reaped promptly.
func configureProcessTreeCancellation(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = processTreeWaitDelay
}
