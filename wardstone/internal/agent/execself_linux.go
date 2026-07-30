//go:build linux

package agent

import (
	"log/slog"
	"os"
	"syscall"
)

// realExecSelf re-execs the current process image in place via
// syscall.Exec, preserving argv and environment (ADR-W003 W5). This is what
// makes promotion and revert take effect without depending on systemd to
// notice a crash and restart the unit — the running process becomes the new
// (or restored) binary directly. If syscall.Exec fails, the process
// continues running the old image and the failure is logged at Error; a
// caller in the promote path still has the outer watchdog
// (packaging/wardstoned.service, Restart=always) as a fallback.
func realExecSelf(log *slog.Logger, binaryPath string) {
	log.Info("re-executing self", "binary", binaryPath)
	if err := syscall.Exec(binaryPath, os.Args, os.Environ()); err != nil {
		log.Error("re-exec failed", "error", err, "binary", binaryPath)
	}
}

// defaultExecSelf is the Agent-bound form used after a promoted self-update
// (see agent.go's session send-path comment for the ordering guarantee).
func (a *Agent) defaultExecSelf() {
	realExecSelf(a.log, a.cfg.BinaryPath)
}
