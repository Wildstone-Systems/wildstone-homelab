//go:build !linux

package agent

import wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"

// Wardstone targets Linux; this stub only keeps the module compiling on
// developer machines (matches facts_other.go's pattern).
func (a *Agent) listPorts() (*wsv1.OpResult_ListPorts, *wsv1.OpError) {
	return &wsv1.OpResult_ListPorts{ListPorts: &wsv1.ListPortsResult{}}, nil
}
