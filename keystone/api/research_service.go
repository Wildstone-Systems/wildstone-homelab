package api

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/reconcile"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/research"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// ResolveManagedResearchEndpoint reads current desired configuration each time.
// It does not itself prove reconciliation or upstream readiness.
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

// WireManagedResearch installs the explicit Owner-test path, never an automatic
// probe or grant. An absent verified release pin keeps production unavailable.
// Call at startup, before serving requests.
func (s *Server) WireManagedResearch(rec *reconcile.Reconciler) {
	if rec == nil {
		s.ResearchTest = nil
		return
	}
	s.ResearchTest = s.managedResearchTest(rec.VerifyRunningContainer, research.ManagedReleaseImage)
}

func (s *Server) managedResearchTest(check func(context.Context, string, string, []resource.Resource) error, releaseImage string) func(context.Context, research.Provider) error {
	return func(ctx context.Context, p research.Provider) error {
		unavailable := fmt.Errorf("provider_unavailable: managed dependency not verified")
		if releaseImage == "" || s.State == nil || check == nil {
			return unavailable
		}
		config, err := p.Config.Normalize()
		if err != nil || config.Kind != research.DDGS || config.CredentialRef != "" {
			return unavailable
		}
		ctx, cancel := context.WithTimeout(ctx, time.Duration(config.Limits.RequestTimeoutSeconds)*time.Second)
		defer cancel()
		head, err := s.State.Head()
		if err != nil || ctx.Err() != nil {
			return unavailable
		}
		desired, err := s.State.LoadAt(head)
		if err != nil {
			return unavailable
		}
		endpoint, err := research.ManagedEndpoint(desired, s.NodeID, config.EndpointRef)
		if err != nil {
			return unavailable
		}
		for _, res := range desired {
			if res.Kind == resource.KindContainer && res.Metadata.Name == research.ManagedContainer && res.Container.Image != releaseImage {
				return unavailable
			}
		}
		unchanged := func() error {
			current, err := s.State.Head()
			if err != nil || current != head || ctx.Err() != nil {
				return unavailable
			}
			working, err := s.State.Load()
			if err != nil || !reflect.DeepEqual(working, desired) {
				return unavailable
			}
			return nil
		}
		verify := func() error {
			if err := unchanged(); err != nil {
				return err
			}
			if err := check(ctx, s.NodeID, research.ManagedContainer, desired); err != nil {
				return err
			}
			return unchanged()
		}
		if err := verify(); err != nil {
			return unavailable
		}
		factory := research.Factory{ResolveEndpoint: func(context.Context, string) (string, error) { return endpoint, nil }}
		if err := factory.Test(ctx, p); err != nil {
			return err
		}
		if err := verify(); err != nil {
			return unavailable
		}
		return nil
	}
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
