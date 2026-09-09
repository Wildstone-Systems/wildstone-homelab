package api

// What the Butler remembers, and who may change it (ADR-W030 A3, work
// package P8).
//
// Two permission rules run through every handler here, and they are not
// the same rule:
//
//   Reading is scoped to the reader. A retrieval or an export returns
//   the caller's own records and the household-shared ones, never
//   anyone else's private material. The store does that filtering in
//   SQL, before content is loaded, and these handlers pass the ACTING
//   member rather than one named in a request.
//
//   Correcting and forgetting follow ownership. Any admitted adult may
//   correct or forget household-shared memory without the original
//   author's approval (A1) — equal adult standing is the decision. A
//   private record belongs to the person it is about, and to the
//   configured administrator; ordinary adult trust does not widen a
//   private source.

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// mayChangeMemory reports whether id may correct or forget this record.
func mayChangeMemory(m runtimedb.MemoryRecord, id principal.Identity) bool {
	if m.Audience == runtimedb.ButlerAudienceShared {
		// Equal adult standing: any admitted adult, no approval from the
		// original author (ADR-W030 A1).
		return true
	}
	return m.Owner == actingMember(id) || id.Role == runtimedb.RoleAdmin
}

func (s *Server) retrieveButlerMemory(r *http.Request, id principal.Identity) (any, int, error) {
	records, err := s.DB.RetrieveMemory(runtimedb.MemoryQuery{
		Reader:  actingMember(id),
		Subject: r.URL.Query().Get("subject"),
		Kind:    r.URL.Query().Get("kind"),
	})
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"records": records}, http.StatusOK, nil
}

// exportButlerMemory is a person's personal export. It is a read
// governed by current permissions and never a bypass: material the
// caller could not retrieve is material the export does not contain.
func (s *Server) exportButlerMemory(_ *http.Request, id principal.Identity) (any, int, error) {
	records, err := s.DB.ExportMemoryFor(actingMember(id))
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"member": actingMember(id), "records": records}, http.StatusOK, nil
}

func (s *Server) correctButlerMemory(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Seq        int64  `json:"seq"`
		Content    string `json:"content"`
		Confidence string `json:"confidence"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	existing, err := s.DB.GetMemory(req.Seq)
	if err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("no such memory record")
	}
	if !mayChangeMemory(existing, id) {
		// 404 rather than 403: confirming that someone else's private
		// record exists is itself a disclosure.
		return nil, http.StatusNotFound, fmt.Errorf("no such memory record")
	}
	seq, err := s.DB.CorrectMemory(req.Seq, runtimedb.MemoryRecord{
		Content: req.Content, Confidence: req.Confidence,
		SourceKind: "correction", SourceID: fmt.Sprintf("member:%s", actingMember(id)),
	}, actingMember(id))
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"seq": seq}, http.StatusOK, nil
}

func (s *Server) forgetButlerMemory(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Seq int64 `json:"seq"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	existing, err := s.DB.GetMemory(req.Seq)
	if err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("no such memory record")
	}
	if !mayChangeMemory(existing, id) {
		return nil, http.StatusNotFound, fmt.Errorf("no such memory record")
	}
	if err := s.DB.ForgetMemory(req.Seq, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no such memory record")
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{
		"forgotten": req.Seq,
		"note": "the source it came from will not teach it again; " +
			"a different source still could, and would be a new decision to correct",
	}, http.StatusOK, nil
}

// listMemoryDigest is what other admitted adults see about SHARED
// memory changes. Private corrections never appear: a digest that leaked
// them would make correcting a private record a disclosure.
func (s *Server) listMemoryDigest(r *http.Request, _ principal.Identity) (any, int, error) {
	immediateOnly := r.URL.Query().Get("immediate") == "true"
	entries, err := s.DB.PendingMemoryDigest(immediateOnly)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"entries": entries}, http.StatusOK, nil
}
