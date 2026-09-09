package api

// Retrieval index health and compaction (ADR-W030 A3/A9.1, work
// package P9).
//
// The endpoints here are about the index as a THING THAT COSTS
// something: what it measured, how big it is, and a way to make it
// smaller. Searching it is not exposed as an endpoint at all — the
// Butler retrieves through the store, on a reader's behalf, and an
// endpoint that let a caller query the index directly would be a second
// retrieval path with its own permission story to get wrong.

import (
	"net/http"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
)

// listIndexMeasurements reports what the retrieval fixtures actually
// measured. A claim about recall with no measurement behind it is a
// preference, and this is where the measurement lives.
func (s *Server) listIndexMeasurements(_ *http.Request, _ principal.Identity) (any, int, error) {
	measurements, err := s.DB.ListIndexMeasurements()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"measurements": measurements}, http.StatusOK, nil
}

// compactButlerIndex prunes what should no longer be searchable and
// rebuilds the FTS structures.
//
// It reports what it removed, because compaction that reports nothing is
// compaction nobody can tell ran — and the number that matters is how
// many forgotten or corrected records were still searchable, which is a
// bug count, not a housekeeping statistic.
func (s *Server) compactButlerIndex(r *http.Request, _ principal.Identity) (any, int, error) {
	var req struct {
		MaxAgeDays int `json:"maxAgeDays"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	var maxAge time.Duration
	if req.MaxAgeDays > 0 {
		maxAge = time.Duration(req.MaxAgeDays) * 24 * time.Hour
	}
	pruned, aged, err := s.DB.CompactIndex(maxAge)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{
		"prunedForgotten": pruned,
		"agedOut":         aged,
		"note": "pruned entries were forgotten or corrected records that were still searchable; " +
			"aged-out entries simply passed the age you asked for",
	}, http.StatusOK, nil
}
