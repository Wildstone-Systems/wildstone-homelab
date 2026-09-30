package api

import (
	"context"
	"fmt"

	"github.com/chickenrat17/wildstone-homelab/keystone/research"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// ResolveManagedResearchEndpoint reads current desired configuration each time.
// It deliberately does not set ResearchTest: desired state and worker health
// alone cannot prove a reconciled, tested dependency revision. Production
// readiness remains fail-closed until that release integration is verified.
func (s *Server) ResolveManagedResearchEndpoint(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.State == nil {
		return "", fmt.Errorf("provider_unavailable: desired state unavailable")
	}
	current, err := s.State.Load()
	if err != nil {
		return "", fmt.Errorf("provider_unavailable: desired state unavailable")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return research.ManagedEndpoint(current, s.NodeID, ref)
}

// changesResearchService identifies raw-resource and package Plans affecting
// the singleton dependency. Unrelated infrastructure keeps its existing path.
func changesResearchService(stored storedProposal) (bool, error) {
	for _, doc := range stored.Put {
		r, err := resource.Unmarshal([]byte(doc))
		if err != nil {
			return false, err
		}
		if r.Kind == resource.KindContainer && r.Metadata.Name == research.ManagedContainer {
			return true, nil
		}
	}
	for _, d := range stored.Delete {
		if resource.Kind(d.Kind) == resource.KindContainer && d.Name == research.ManagedContainer {
			return true, nil
		}
	}
	return false, nil
}
