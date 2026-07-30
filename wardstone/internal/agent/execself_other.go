//go:build !linux

package agent

import "log/slog"

// realExecSelf has no portable re-exec primitive outside linux
// (syscall.Exec is linux/unix-specific and wardstoned's production target
// is linux); this stub just logs so the promote/revert paths still make
// progress (the state file is still written/cleared correctly) without
// crashing on other platforms. The process itself must be restarted by
// whatever supervises it there.
func realExecSelf(log *slog.Logger, binaryPath string) {
	log.Warn("self-update re-exec not implemented on this platform; restart the process manually",
		"binary", binaryPath)
}

// defaultExecSelf is the Agent-bound form used after a promoted self-update.
func (a *Agent) defaultExecSelf() {
	realExecSelf(a.log, a.cfg.BinaryPath)
}
