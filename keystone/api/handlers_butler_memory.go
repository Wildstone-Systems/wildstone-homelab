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

// serviceRoles are principals that are not a person.
//
// A1's "any admitted adult" is a statement about PEOPLE. The Butler
// holds read and propose so it can learn and suggest; correcting or
// forgetting household memory is an adult's act, and a role that is not
// an adult performing it is the Butler editing the household's record
// of itself with nobody having decided anything.
var serviceRoles = map[runtimedb.Role]bool{
	runtimedb.RoleButler:    true,
	runtimedb.RolePublisher: true,
	runtimedb.RoleConcierge: true,
}

// mayChangeMemory reports whether id may correct or forget this record.
func mayChangeMemory(m runtimedb.MemoryRecord, id principal.Identity) bool {
	// Checked FIRST, and before the audience: a shared record is the
	// broadest case, so an audience test that returns true early is
	// exactly where a service principal slips through.
	if serviceRoles[id.Role] {
		return false
	}
	if m.Audience == runtimedb.ButlerAudienceShared {
		// Equal adult standing: any admitted adult, no approval from the
		// original author (ADR-W030 A1).
		return true
	}
	return m.Owner == actingMember(id) || id.Role == runtimedb.RoleAdmin
}

func (s *Server) retrieveButlerMemory(r *http.Request, id principal.Identity) (any, int, error) {
	// The Butler scope defaults to EVERY Butler here, not to the shared
	// pool. This endpoint is how a person finds their own material in
	// order to correct or forget it (ADR-W030 A3), and after W031 A4
	// most of it is held by a named Butler — so a pool-only default
	// would hide nearly everything from the only screen that can act on
	// it, while the reader's own permission filter still decides what
	// they may see.
	//
	// ?butler=<name> narrows to one, for a Console showing a single
	// Butler's material.
	butler := r.URL.Query().Get("butler")
	if butler == "" {
		butler = runtimedb.AllButlers
	}
	records, err := s.DB.RetrieveMemory(runtimedb.MemoryQuery{
		Reader:  actingMember(id),
		Butler:  butler,
		Subject: r.URL.Query().Get("subject"),
		Kind:    r.URL.Query().Get("kind"),
	})
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	visible := records[:0]
	for _, record := range records {
		if err := s.checkAPIMemoryDisclosure(id, record.Seq); err != nil {
			if errors.Is(err, runtimedb.ErrPrivacy) {
				continue
			}
			return nil, http.StatusInternalServerError, err
		}
		visible = append(visible, record)
	}
	records = visible
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
	visible := records[:0]
	for _, record := range records {
		if err := s.checkAPIMemoryDisclosure(id, record.Seq); err != nil {
			if errors.Is(err, runtimedb.ErrPrivacy) {
				continue
			}
			return nil, http.StatusInternalServerError, err
		}
		visible = append(visible, record)
	}
	records = visible
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
func (s *Server) listMemoryDigest(r *http.Request, id principal.Identity) (any, int, error) {
	immediateOnly := r.URL.Query().Get("immediate") == "true"
	entries, err := s.DB.PendingMemoryDigest(immediateOnly)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	visible := entries[:0]
	for _, entry := range entries {
		if err := s.checkAPIMemoryDisclosure(id, entry.Memory); err != nil {
			if errors.Is(err, runtimedb.ErrPrivacy) {
				continue
			}
			return nil, http.StatusInternalServerError, err
		}
		visible = append(visible, entry)
	}
	entries = visible
	return map[string]any{"entries": entries}, http.StatusOK, nil
}

func (s *Server) agentMemoryRestrictions(r *http.Request, _ principal.Identity) (any, int, error) {
	scopes, err := s.DB.AgentMemoryRestrictions(r.PathValue("name"))
	return map[string]any{"restricted": scopes}, http.StatusOK, err
}

// Both adding and removing a restriction change reach; Console requires fresh
// Owner authentication through consequential. The store independently checks role.
func (s *Server) setAgentMemoryRestriction(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Scope      string `json:"scope"`
		Restricted bool   `json:"restricted"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.SetAgentMemoryRestriction(r.PathValue("name"), req.Scope, req.Restricted, actingMember(id)); err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"scope": req.Scope, "restricted": req.Restricted}, http.StatusOK, nil
}

// These are inspection reads, not model recall: they may span Agents, but still
// need a disclosure destination. A bearer token cannot assert a household screen.
func (s *Server) checkAPIMemoryDisclosure(id principal.Identity, memory int64) error {
	destination := apiMemoryDestination(id)
	return s.DB.CheckMemoryPrivacy(memory, runtimedb.ModelLocal, destination)
}

func apiMemoryDestination(id principal.Identity) runtimedb.MemoryDestination {
	return runtimedb.MemoryDestination{BuiltIn: id.AuthTime != nil && !serviceRoles[id.Role]}
}
func (s *Server) checkAPITaskDisclosure(r *http.Request, id principal.Identity, task string) error {
	_, guard, err := s.DB.BeginPrivacyTurn(r.Context(), runtimedb.ModelLocal, apiMemoryDestination(id), "")
	if err != nil {
		return err
	}
	if err = guard.SetTask(task); err != nil {
		return err
	}
	return guard.Check()
}
