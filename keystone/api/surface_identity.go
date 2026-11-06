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
