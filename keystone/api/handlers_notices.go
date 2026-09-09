package api

// Proactive communication policy and delivery (ADR-W030 A10/A10.1,
// work package P15).
//
// The endpoints here let a person say when not to be interrupted, let
// an administrator declare which notices may wake the house, and make
// the answer to "was I ever actually told?" visible.
//
// That last one matters more than it looks. A notice that expired
// without reaching anyone is the case where a household believes the
// Butler is quiet and the Butler believes it has spoken. Showing it as
// undelivered is the only way either of them finds out.

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// getNoticePreferences returns a person's own quiet hours and
// thresholds. A caller reads their own; an administrator may read
// anyone's.
func (s *Server) getNoticePreferences(r *http.Request, id principal.Identity) (any, int, error) {
	member := actingMember(id)
	if asked := r.URL.Query().Get("member"); asked != "" && asked != member {
		if id.Role != runtimedb.RoleAdmin {
			return nil, http.StatusNotFound, fmt.Errorf("no such member")
		}
		member = asked
	}
	prefs, err := s.DB.GetMemberNoticePreferences(member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{
		"preferences":      prefs,
		"hasOwnQuietHours": prefs.HasOwnQuietHours(),
		"note": "with no window of your own you follow the household's, which is the point of " +
			"the household having one",
	}, http.StatusOK, nil
}

// setNoticePreferences records a person's own overrides. A person sets
// their own; an administrator may set anyone's.
func (s *Server) setNoticePreferences(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Member         string `json:"member"`
		QuietStartMin  int    `json:"quietStartMin"`
		QuietEndMin    int    `json:"quietEndMin"`
		Timezone       string `json:"timezone"`
		UrgentOverride bool   `json:"urgentOverride"`
		MinRelevance   int    `json:"minRelevance"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	member := actingMember(id)
	if req.Member != "" && req.Member != member {
		if id.Role != runtimedb.RoleAdmin {
			return nil, http.StatusForbidden,
				fmt.Errorf("you can set your own notification preferences, not somebody else's")
		}
		member = req.Member
	}
	if err := s.DB.SetMemberNoticePreferences(runtimedb.MemberNoticePreferences{
		Member: member, QuietStartMin: req.QuietStartMin, QuietEndMin: req.QuietEndMin,
		Timezone: req.Timezone, UrgentOverride: req.UrgentOverride, MinRelevance: req.MinRelevance,
	}); err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"member": member}, http.StatusOK, nil
}

// setNoticeKind declares whether a class of notice may claim the urgent
// exception.
//
// Administrator-only, and deliberately not a flag a sender sets: a
// sender that can mark its own message urgent will, and then quiet hours
// mean nothing.
func (s *Server) setNoticeKind(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Kind        string `json:"kind"`
		Urgent      bool   `json:"urgent"`
		Relevance   int    `json:"relevance"`
		Description string `json:"description"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.SetNoticeKind(runtimedb.NoticeKind{
		Kind: req.Kind, Urgent: req.Urgent, Relevance: req.Relevance, Description: req.Description,
	}, actingMember(id)); err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"kind": req.Kind, "urgent": req.Urgent}, http.StatusOK, nil
}

// listNotices shows what was sent, held, missed or answered. A caller
// sees their own; an administrator sees the deployment's.
func (s *Server) listNotices(r *http.Request, id principal.Identity) (any, int, error) {
	member := actingMember(id)
	if id.Role == runtimedb.RoleAdmin && r.URL.Query().Get("all") == "true" {
		member = ""
	}
	notices, err := s.DB.ListNotices(member, 100)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	missed := 0
	for _, n := range notices {
		if !n.UndeliveredAt.IsZero() {
			missed++
		}
	}
	return map[string]any{
		"notices": notices,
		"missed":  missed,
		"note": "a missed notice means nobody was told. It changes nothing about what actually " +
			"happened, and it is shown because otherwise nobody finds out",
	}, http.StatusOK, nil
}

// acknowledgeNotice records that a person saw one — separate from
// delivery, because "we sent it" and "they know" are different claims.
func (s *Server) acknowledgeNotice(r *http.Request, _ principal.Identity) (any, int, error) {
	var req struct {
		Seq int64 `json:"seq"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.AcknowledgeNotice(req.Seq); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no such notice, or it was already acknowledged")
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"acknowledged": req.Seq}, http.StatusOK, nil
}

// answerNotice records a human's reply to a question a notice asked.
func (s *Server) answerNotice(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Seq    int64  `json:"seq"`
		Answer string `json:"answer"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	approved, err := s.DB.AnswerNotice(req.Seq, req.Answer, actingMember(id))
	switch {
	case errors.Is(err, runtimedb.ErrNotFound):
		return nil, http.StatusNotFound, fmt.Errorf("no such notice")
	case errors.Is(err, runtimedb.ErrNoticeNotAnswerable):
		// The message is the whole value here: a person who wrote
		// "maybe" needs to know it did nothing, or they will assume it
		// did.
		return nil, http.StatusBadRequest, err
	case err != nil:
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"seq": req.Seq, "approved": approved}, http.StatusOK, nil
}

// expireNotices sweeps everything past its expiry. It is an endpoint as
// well as a background pass so a Console can force one and see the
// count.
func (s *Server) expireNotices(_ *http.Request, _ principal.Identity) (any, int, error) {
	n, err := s.DB.ExpireNotices(time.Now())
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"expired": n}, http.StatusOK, nil
}
