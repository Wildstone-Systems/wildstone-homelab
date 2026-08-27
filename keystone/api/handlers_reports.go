package api

import (
	"net/http"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/report"
)

// morningReport serves the deterministic morning report (ADR-W017 A5) —
// built fresh on every request from runtime-DB reads, no LLM involved, so
// it works identically whatever mode the Butler is in.
func (s *Server) morningReport(r *http.Request, _ principal.Identity) (any, int, error) {
	rep, err := report.Morning(s.DB, time.Now())
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return rep, http.StatusOK, nil
}
