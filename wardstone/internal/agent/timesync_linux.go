//go:build linux

package agent

// TimeSyncStatus (ADR-W003 W6d). Reads kernel clock synchronization state
// via golang.org/x/sys/unix.Adjtimex (pre-approved dependency, §A4.5) — no
// shell-out to timedatectl/chronyc.

import (
	"golang.org/x/sys/unix"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// adjtimexFn is the seam over the syscall so tests can fake both TIME_OK
// and TIME_ERROR without depending on the real kernel clock state.
var adjtimexFn = func() (int, error) {
	var buf unix.Timex
	state, err := unix.Adjtimex(&buf)
	return state, err
}

func (a *Agent) timeSyncStatus(_ *wsv1.TimeSyncStatus) (*wsv1.OpResult_TimeSyncStatus, *wsv1.OpError) {
	state, err := adjtimexFn()
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "adjtimex: %v", err)
	}
	synchronized := state != unix.TIME_ERROR
	detail := "clock synchronized"
	if !synchronized {
		detail = "clock not synchronized (TIME_ERROR)"
	}
	return &wsv1.OpResult_TimeSyncStatus{TimeSyncStatus: &wsv1.TimeSyncStatusResult{
		Synchronized: synchronized,
		Detail:       detail,
	}}, nil
}
