//go:build !linux

package agent

import (
	"os"
	"runtime"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// Wardstone targets Linux; this stub only keeps the module compiling on
// developer machines.
func (a *Agent) hostFacts() (*wsv1.OpResult_HostFacts, *wsv1.OpError) {
	hostname, _ := os.Hostname()
	return &wsv1.OpResult_HostFacts{HostFacts: &wsv1.HostFactsResult{
		Hostname:     hostname,
		Os:           runtime.GOOS,
		NumCpu:       uint32(runtime.NumCPU()),
		AgentVersion: a.cfg.Version,
	}}, nil
}
