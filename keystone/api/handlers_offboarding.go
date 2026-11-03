package api

// Offboarding and restore reconciliation (ADR-W030 A2.1/A10, work
// package P16).
//
// One endpoint denies access everywhere and settles the work; three
// more drive a restored database out of quarantine. Both are
// administrator-only, and the restore ones are the more dangerous pair:
// releasing quarantine early is how a restored backup starts talking to
// a household with last week's idea of who is allowed to.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// offboardMember denies access across every store and settles the work.
//
// It reports what it did per store, because "we offboarded them" is a
// claim about nine stores and a claim about nine stores needs nine
// answers.
func (s *Server) offboardMember(r *http.Request, id principal.Identity) (any, int, error) {
	actor, err := s.activeOwnerConsoleActor(id)
	if err != nil {
		return nil, http.StatusForbidden, err
	}
	member := r.PathValue("member")
	var req struct {
		Administrator string `json:"administrator"`
	}
	if r.ContentLength != 0 {
		if code, err := decodeStrict(r, &req); err != nil {
			return nil, code, err
		}
	}
	if req.Administrator == "" {
		req.Administrator = actor
	}
	admin, err := s.DB.GetButlerMember(req.Administrator)
	if err != nil || !admin.Active() || admin.Member == member {
		return nil, http.StatusBadRequest, fmt.Errorf("shared work needs another active Person")
	}
	result, err := s.DB.OffboardMember(member, actor, req.Administrator)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no butler record for %q", member)
		}
		return nil, http.StatusBadRequest, err
	}
	s.EndConsoleSessions(member)
	// Their Personal credentials were fenced in the store; now prove each
	// copy erased on its node. Anything not proven leaves the connection
	// erasure_unverified and is visible in the result, never assumed.
	erasures := map[string][]erasureOutcome{}
	if connections, err := s.DB.ListConnections(); err == nil {
		for _, c := range connections {
			if c.Owner == member && c.Ownership == runtimedb.OwnershipPersonal {
				if out := s.eraseCredentialCopies(r.Context(), c.Name, false); len(out) > 0 {
					erasures[c.Name] = out
				}
				s.refileAdapterRender(c.Name, actingMember(id))
			}
		}
	}
	records, err := s.DB.ListOffboardingRecords(member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	resources, err := s.DB.ListOffboardingResourceDispositions(member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{
		"result":             result,
		"records":            records,
		"resources":          resources,
		"credentialErasures": erasures,
		"note": "access is denied now. Personal work is paused and nothing was deleted; shared work " +
			"moved to the administrator and kept its original author; desired-state cleanup follows " +
			"as plans, and a failure there never restores access",
	}, http.StatusOK, nil
}

func (s *Server) getOffboarding(r *http.Request, id principal.Identity) (any, int, error) {
	if _, err := s.activeOwnerConsoleActor(id); err != nil {
		return nil, http.StatusForbidden, err
	}
	member := r.PathValue("member")
	person, err := s.DB.GetButlerMember(member)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, err
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	records, err := s.DB.ListOffboardingRecords(member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	resources, err := s.DB.ListOffboardingResourceDispositions(member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"member": person, "records": records, "resources": resources}, http.StatusOK, nil
}

// getRestoreState reports whether this database is quarantined and how
// far reconciliation has got.
func (s *Server) getRestoreState(_ *http.Request, _ principal.Identity) (any, int, error) {
	state, err := s.DB.GetRestoreState()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{
		"state": state,
		"note": "quarantine holds retrieval, dispatch, ingestion and proactive delivery. " +
			"Reconciliation requires signed terminal journal evidence independently retained from the snapshot, " +
			"exact snapshot-prefix continuity and replay. An Owner acknowledgement alone is not freshness evidence",
	}, http.StatusOK, nil
}

// enterRestoreQuarantine marks this database as restored.
func (s *Server) enterRestoreQuarantine(r *http.Request, _ principal.Identity) (any, int, error) {
	var req struct {
		Reason     string `json:"reason"`
		SnapshotAt string `json:"snapshotAt"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	var snapshot time.Time
	if req.SnapshotAt != "" {
		var err error
		if snapshot, err = time.Parse(time.RFC3339, req.SnapshotAt); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("snapshotAt must be RFC3339: %w", err)
		}
	}
	if err := s.DB.EnterRestoreQuarantine(req.Reason, snapshot); err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"quarantined": true}, http.StatusOK, nil
}

// reconcileRestore verifies independently retained, signed terminal journal
// evidence and replays it. It deliberately cannot release quarantine; #285
// owns that separate authorized decision after this evidence is inspectable.
func (s *Server) reconcileRestore(r *http.Request, id principal.Identity) (any, int, error) {
	actor, err := s.activeOwnerConsoleActor(id)
	if err != nil {
		return nil, http.StatusForbidden, err
	}
	var req struct {
		Evidence json.RawMessage `json:"evidence"`
		Release  bool            `json:"release"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if req.Release {
		return nil, http.StatusBadRequest, fmt.Errorf("journal reconciliation never releases restore quarantine; release is a separate Owner operation")
	}
	if len(req.Evidence) == 0 || len(s.RecoveryJournalPublicKey) == 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("signed recovery journal evidence and the preserved trusted signing identity are required")
	}
	result, err := s.DB.ReconcileRestoreEvidence(req.Evidence, s.RecoveryJournalPublicKey, actor, s.now())
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	out := map[string]any{"reconciliation": result, "quarantined": true}
	// A replay that re-fenced a Personal credential has only said it is
	// revoked; the copies still have to be proven gone, and a newer
	// version the snapshot never heard of may be sitting on a node.
	// Erasing only removes access, so it runs even while quarantined.
	erasures := map[string][]erasureOutcome{}
	if connections, err := s.DB.ListConnections(); err == nil {
		for _, c := range connections {
			if c.Ownership != runtimedb.OwnershipPersonal {
				continue
			}
			if res := s.eraseCredentialCopies(r.Context(), c.Name, true); len(res) > 0 {
				erasures[c.Name] = res
			}
			// The snapshot's desired state may still render an adapter
			// the replay has just fenced (or be missing one it did not).
			// Re-file the render so an Owner reviews the placement that
			// matches the replayed authority; like every render it waits
			// for approval, and the reconciler applies nothing while the
			// database is quarantined (epic #326 S08).
			s.refileAdapterRender(c.Name, actingMember(id))
		}
	}
	out["credentialErasures"] = erasures
	out["note"] = "verified and replayed; restore quarantine remains active pending a separate authorized release"
	return out, http.StatusOK, nil
}
