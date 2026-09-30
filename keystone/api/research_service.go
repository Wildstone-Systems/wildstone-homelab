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

// invalidateResearchServicePlan runs before the first desired-state mutation.
// Even a partial/failed apply must never retain a successful provider test.
// Both raw-resource and package Plans pass here. A DB error prevents the write.
func (s *Server) invalidateResearchServicePlan(stored storedProposal) error {
	affected := false
	for _, doc := range stored.Put {
		r, err := resource.Unmarshal([]byte(doc))
		if err != nil {
			return err
		}
		if r.Kind == resource.KindContainer && r.Metadata.Name == research.ManagedContainer {
			affected = true
		}
	}
	for _, d := range stored.Delete {
		if resource.Kind(d.Kind) == resource.KindContainer && d.Name == research.ManagedContainer {
			affected = true
		}
	}
	if !affected {
		return nil
	}
	return s.DB.InvalidateSearchService(research.ManagedEndpointRef)
}
