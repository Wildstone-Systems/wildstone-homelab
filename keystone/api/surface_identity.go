package api

import (
	"context"
	"errors"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

type surfaceGenerationKey struct{}

func withSurfaceGeneration(ctx context.Context, generation int64) context.Context {
	return context.WithValue(ctx, surfaceGenerationKey{}, generation)
}

func surfaceGeneration(ctx context.Context) int64 {
	generation, _ := ctx.Value(surfaceGenerationKey{}).(int64)
	return generation
}

func (s *Server) resolveRequestSurface(id principal.Identity) (runtimedb.ButlerMember, error) {
	person := actingMember(id)
	if person == "" || id.SurfaceLinkType == "" || id.SurfaceExternalID == "" {
		return runtimedb.ButlerMember{}, runtimedb.ErrNotAdmitted
	}
	linked, err := s.DB.ResolveSurfaceIdentity(id.SurfaceLinkType, id.SurfaceExternalID)
	if err != nil {
		return runtimedb.ButlerMember{}, err
	}
	claimed, err := s.DB.GetButlerMember(person)
	if errors.Is(err, runtimedb.ErrNotFound) || err == nil && claimed.PersonID != linked.PersonID {
		return runtimedb.ButlerMember{}, runtimedb.ErrNotAdmitted
	}
	if err != nil {
		return runtimedb.ButlerMember{}, err
	}
	return linked, nil
}

// admittedGeneration returns the identity-link generation a turn was
// admitted under: the one ingress resolved, or (for trusted internal
// callers with none) the member's current one. Every runner checks it
// before the model call and again after, so a link revoked mid-call
// cannot publish work under the newer generation.
func (s *Server) admittedGeneration(ctx context.Context, member string) (int64, error) {
	generation := surfaceGeneration(ctx)
	if generation == 0 {
		m, err := s.DB.GetButlerMember(member)
		if err != nil || !m.Active() {
			return 0, runtimedb.ErrNotAdmitted
		}
		generation = m.Generation
	}
	if err := s.DB.CheckButlerMemberGeneration(member, generation); err != nil {
		return 0, runtimedb.ErrNotAdmitted
	}
	return generation, nil
}
