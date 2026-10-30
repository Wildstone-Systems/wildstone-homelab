package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

const platformFactsTimeout = 3 * time.Second

// refuseIfPlatformMismatch rechecks current typed facts immediately before a
// workload write. An absent constraint preserves older Statefile behavior.
func (r *Reconciler) refuseIfPlatformMismatch(ctx context.Context, nodeID string, allowed []string) string {
	if len(allowed) == 0 {
		return ""
	}
	if r.Applier == nil {
		return fmt.Sprintf("platform check for node %s: node facts are unavailable", nodeID)
	}
	probeCtx, cancel := context.WithTimeout(ctx, platformFactsTimeout)
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
	if !slices.Contains(allowed, actual) {
		return fmt.Sprintf("node %s reports %s; workload supports %s", nodeID, actual, strings.Join(allowed, ", "))
	}
	return ""
}
