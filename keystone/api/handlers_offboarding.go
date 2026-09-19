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
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// offboardMember denies access across every store and settles the work.
//
// It reports what it did per store, because "we offboarded them" is a
// claim about nine stores and a claim about nine stores needs nine
// answers.
func (s *Server) offboardMember(r *http.Request, id principal.Identity) (any, int, error) {
	member := r.PathValue("member")
	var req struct {
		Administrator string `json:"administrator"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if req.Administrator == "" {
		req.Administrator = actingMember(id)
	}
	result, err := s.DB.OffboardMember(member, actingMember(id), req.Administrator)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no butler record for %q", member)
		}
		return nil, http.StatusBadRequest, err
	}
	s.EndConsoleSessions(member)
	records, err := s.DB.ListOffboardingRecords(member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{
		"result":  result,
		"records": records,
		"note": "access is denied now. Personal work is paused and nothing was deleted; shared work " +
			"moved to the administrator and kept its original author; desired-state cleanup follows " +
			"as plans, and a failure there never restores access",
	}, http.StatusOK, nil
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
			"Releasing it takes an assertion that the deletion journal is current AND a replay of " +
			"it — an acknowledgement alone is not evidence that missing deletions were recovered",
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

// reconcileRestore asserts the journal is current and replays it.
//
// The two happen in one call because doing them separately invites
// doing only the first, which is precisely the failure A10 names.
func (s *Server) reconcileRestore(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		JournalCurrent bool `json:"journalCurrent"`
		Release        bool `json:"release"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if !req.JournalCurrent {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"reconciliation needs journalCurrent: somebody has to state that the deletion journal " +
				"has been brought up to date from a source newer than the snapshot. Replaying an " +
				"equally old journal proves nothing")
	}
	if err := s.DB.AssertJournalCurrent(actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusBadRequest, fmt.Errorf("this database is not in restore quarantine")
		}
		return nil, http.StatusInternalServerError, err
	}
	applied, err := s.DB.ReconcileRestore()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := map[string]any{"replayed": applied}
	if req.Release {
		if err := s.DB.ReleaseRestoreQuarantine(actingMember(id)); err != nil {
			return nil, http.StatusBadRequest, err
		}
		out["released"] = true
	} else {
		out["note"] = "reconciled but still quarantined. Release it separately once you have " +
			"looked at what the replay did"
	}
	return out, http.StatusOK, nil
}
