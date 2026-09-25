package api

// The runtime inventory across every Agent (ADR-W039 §3.3; design doc
// §9 H9; issue #355).
//
// The Butlers roster answers "what can each Agent do"; this answers
// "what is reasoning for each Agent right now" — one screen a Console
// holder can scan for a runtime gone offline or a credential nobody
// rotated, rather than opening every Agent's drawer in turn. It is a
// read of the same field the Agent's own runtime route reads
// (handlers_butler_runtime.go's butlerRuntime), just every Agent's at
// once, in the Agent API's contract shape (§3.3) so the Console never
// grows a second opinion of what "connected" means.

import (
	"net/http"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
)

// runtimeInventoryRow is one line of the inventory: which Agent, its
// class (needed to explain a host-admin profile that only an Owner
// Agent may actually use — TabRuntime's own guard), and its runtime
// record in the Agent API's shape.
type runtimeInventoryRow struct {
	Butler     string                 `json:"butler"`
	AgentClass string                 `json:"agentClass"`
	Runtime    agentapi.RuntimeRecord `json:"runtime"`
}

// listAgentRuntimes is GET /api/v1/runtimes: every live Agent's runtime
// record, one query (runtimedb.ListButlerRuntimes) rather than the
// roster plus one runtime fetch per Agent. Read-only, and the same verb
// as GET /api/v1/butlers (VerbRead) — an inventory of what is already
// on every Agent's own runtime route needs no wider a gate than the
// roster it augments.
func (s *Server) listAgentRuntimes(_ *http.Request, _ principal.Identity) (any, int, error) {
	rows, err := s.DB.ListButlerRuntimes()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := make([]runtimeInventoryRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, runtimeInventoryRow{
			Butler:     r.Butler,
			AgentClass: r.AgentClass,
			Runtime:    r.Runtime.Record(),
		})
	}
	return out, http.StatusOK, nil
}
