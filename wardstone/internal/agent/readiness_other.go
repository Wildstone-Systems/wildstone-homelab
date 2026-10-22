//go:build !linux

package agent

import (
	"context"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// Wardstone targets Linux; there is no procfs, sysfs or BlueZ to read
// here, so every prerequisite answers UNSUPPORTED — a successful result
// Keystone can tell apart from "missing", not an error.
func (a *Agent) homeAutomationReadiness(_ context.Context) (*wsv1.OpResult_HomeAutomationReadiness, *wsv1.OpError) {
	res := &wsv1.HomeAutomationReadinessResult{PlatformSupported: false}
	for _, name := range readinessChecks {
		res.Checks = append(res.Checks, readinessCheck(name, wsv1.ReadinessState_READINESS_STATE_UNSUPPORTED, "", "", "not a Linux node"))
	}
	return &wsv1.OpResult_HomeAutomationReadiness{HomeAutomationReadiness: res}, nil
}
