package api

import (
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
)

// WorkspaceSettings is the per-call desired-state seam for workspace storage
// and canonical tool inventory. Never retain it across calls: removal, mount
// loss and reconfiguration must take effect without restarting Keystone.
// Readiness is a dependency check, never a tool or resource grant.
func (s *Server) WorkspaceSettings() (workspace.Settings, error) {
	if s == nil || s.State == nil {
		return workspace.Settings{}, workspace.ErrUnavailable
	}
	current, err := s.State.Load()
	if err != nil {
		return workspace.Settings{}, workspace.ErrUnavailable
	}
	settings, err := workspace.Read(current, s.NodeID)
	if err != nil {
		return workspace.Settings{}, err
	}
	if err := settings.Ready(); err != nil {
		return workspace.Settings{}, err
	}
	return settings, nil
}

func (s *Server) workspaceBlockers(current []resource.Resource) []string {
	settings, err := workspace.Read(current, s.NodeID)
	if err == nil {
		err = settings.Ready()
	}
	if err != nil {
		return []string{err.Error()}
	}
	return nil
}
