package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// refuseIfPlatformMismatch checks the current node facts before any workload
// mutation. An absent constraint preserves the behavior of older statefiles.
func (r *Reconciler) refuseIfPlatformMismatch(ctx context.Context, nodeID string, allowed string) string {
	if allowed == "" {
		return ""
	}
	platforms := strings.Split(allowed, ",")
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := r.Applier.Apply(probeCtx, nodeID, &wsv1.Operation{
		Op: &wsv1.Operation_HostFacts{HostFacts: &wsv1.HostFacts{}},
	})
	if err != nil {
		return fmt.Sprintf("platform check for node %s: %v", nodeID, err)
	}
	if res == nil {
		return fmt.Sprintf("platform check for node %s returned no result", nodeID)
	}
	if opErr := res.GetError(); opErr != nil {
		return fmt.Sprintf("platform check for node %s: %s", nodeID, opErr.GetMessage())
	}
	facts := res.GetHostFacts()
	if facts == nil || facts.GetOs() == "" || facts.GetArchitecture() == "" {
		return fmt.Sprintf("node %s did not report a canonical OS/architecture", nodeID)
	}
	actual := facts.GetOs() + "/" + facts.GetArchitecture()
	if !slices.Contains(platforms, actual) {
		return fmt.Sprintf("node %s reports %s; workload supports %s", nodeID, actual, strings.Join(platforms, ", "))
	}
	return ""
}
