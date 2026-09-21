package api

import (
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"net/http"
)

func (s *Server) changeMemoryPrivacy(r *http.Request, actor string, personalOnly bool) (any, int, error) {
	var req struct {
		Seq int64 `json:"seq"`
		runtimedb.PrivacyPatch
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if personalOnly {
		record, err := s.DB.GetMemory(req.Seq)
		if err != nil || record.Scope == runtimedb.MemoryScopeHousehold || record.Owner != actor {
			return nil, http.StatusNotFound, fmt.Errorf("no such personal memory")
		}
	}
	if err := s.DB.PatchMemoryPrivacy(actor, req.Seq, req.PrivacyPatch); err != nil {
		return nil, http.StatusNotFound, err
	}
	p, err := s.DB.MemoryPrivacy(req.Seq)
	return p, http.StatusOK, err
}
func (s *Server) setMemoryPrivacy(r *http.Request, id principal.Identity) (any, int, error) {
	if id.Role != runtimedb.RoleAdmin || id.AuthTime == nil {
		return nil, http.StatusForbidden, fmt.Errorf("change privacy in Console with a fresh Owner sign-in")
	}
	return s.changeMemoryPrivacy(r, actingMember(id), false)
}
func (s *Server) assistantMemoryPrivacy(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	return s.changeMemoryPrivacy(r, member, true)
}
