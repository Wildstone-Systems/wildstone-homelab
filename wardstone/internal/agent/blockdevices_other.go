//go:build !linux

package agent

import wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"

// Wardstone targets Linux; this stub only keeps the module compiling on
// developer machines (matches facts_other.go's pattern). No block devices
// enumerated on non-Linux — an empty list, not an error, since this op is
// informational rather than security-relevant.
func (a *Agent) listBlockDevices() (*wsv1.OpResult_ListBlockDevices, *wsv1.OpError) {
	return &wsv1.OpResult_ListBlockDevices{ListBlockDevices: &wsv1.ListBlockDevicesResult{}}, nil
}
