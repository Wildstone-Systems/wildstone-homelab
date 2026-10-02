package api

// Mail event candidates and their private review (epic #326 S06, #459;
// ADR-W030 A5.7). One Person-scoped API serves the Console, the CLI,
// the Homepage and — for reading — an Agent acting for that Person:
//
//	GET  /api/v1/mail/candidates               the caller's own candidates
//	GET  /api/v1/mail/candidates/{id}          one, with its review
//	POST /api/v1/mail/candidates/{id}/act      {token} | {revision, action, fields?}
//	POST /api/v1/mail/act                      {token}
//
// and the same under /api/v1/home/ for the Homepage session.
//
// The store scopes every read and write to the active Person who owns
// the candidate: anybody else — another Person, an Owner, an Agent, a
// machine principal — gets 404, the same answer as "no such candidate".
// Acting is further restricted to the Person's own Console or CLI
// credential (the admin role, which alone holds manage) or their
// Homepage session: a Butler/Agent principal, an operator or any other
// machine credential is refused before the store is asked, whoever it
// says it acts for — so no Agent approves anything, whatever it is
// told. Review tokens and the excerpt are returned only to those same
// credentials, never to an Agent reading on the Person's behalf.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/mailreview"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// errMailActorDelegated refuses an action from anything but the
// Person's own identity.
var errMailActorDelegated = errors.New("a mail review is answered only by its Person, never by an Agent or on their behalf")

// mailDirect reports whether id is the Person's own Console/CLI
// credential rather than an Agent or machine acting for them.
func mailDirect(id principal.Identity) bool {
	return id.Role == runtimedb.RoleAdmin
}

// mailCandidateSummary is one row of the list: the candidate and a
// summary of its latest review.
type mailCandidateSummary struct {
	runtimedb.MailCandidate
	Review *runtimedb.MailReview `json:"review,omitempty"`
}

func (s *Server) mailCandidates(actor string, r *http.Request) (any, int, error) {
	f := runtimedb.MailCandidateFilter{Connection: r.URL.Query().Get("connection")}
	if st := strings.TrimSpace(r.URL.Query().Get("state")); st != "" {
		f.States = strings.Split(st, ",")
	}
	list, err := s.DB.ListMailCandidates(actor, f)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := make([]mailCandidateSummary, 0, len(list))
	for _, c := range list {
		row := mailCandidateSummary{MailCandidate: c}
		if rv, err := s.DB.MailReviewFor(actor, c.ID); err == nil {
			row.Review = &rv
		}
		out = append(out, row)
	}
	return map[string]any{"candidates": out}, http.StatusOK, nil
}

func (s *Server) mailCandidate(actor string, direct bool, r *http.Request) (any, int, error) {
	id := r.PathValue("id")
	c, err := s.DB.GetMailCandidate(actor, id)
	if err != nil {
		return nil, mailStatus(err), err
	}
	out := map[string]any{"candidate": c}
	var vault *runtimedb.MailExcerptVault
	if s.MailReview != nil {
		vault = s.MailReview.Vault
	}
	var tokens map[string]string
	if direct {
		rv, toks, err := s.DB.MailReviewTokens(vault, actor, id, time.Now())
		if err == nil {
			out["review"], tokens = rv, toks
		}
		if ex, err := s.DB.ReadMailCandidateExcerpt(vault, actor, id, time.Now()); err == nil && ex != "" {
			out["excerpt"] = ex
		}
	} else if rv, err := s.DB.MailReviewFor(actor, id); err == nil {
		out["review"] = rv
	}
	effects, err := s.DB.ListMailEffects(actor, id)
	if err != nil {
		return nil, mailStatus(err), err
	}
	out["effects"] = effects
	if tokens == nil {
		return out, http.StatusOK, nil
	}
	return mailCandidateResponse{body: out, tokens: tokens}, http.StatusOK, nil
}

// mailCandidateResponse carries a candidate to its own Person with the
// review's action tokens. The tokens are the one carve-out from
// response redaction here — a token is the bearer of exactly one
// approval, derived on demand and returned only to the Person's own
// Console, CLI or Homepage session — and everything else in the body is
// redacted as usual.
type mailCandidateResponse struct {
	body   map[string]any
	tokens map[string]string
}

func (m mailCandidateResponse) redacted() ([]byte, error) {
	raw, err := json.Marshal(m.body)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(redact.String(string(raw))), &fields); err != nil {
		return nil, err
	}
	if fields["tokens"], err = json.Marshal(m.tokens); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

// mailActRequest is the body of an action.
type mailActRequest struct {
	Token    string                         `json:"token,omitempty"`
	Revision int                            `json:"revision,omitempty"`
	Action   string                         `json:"action,omitempty"`
	Fields   *runtimedb.MailCandidateFields `json:"fields,omitempty"`
}

func (s *Server) mailAct(actor, via string, r *http.Request) (any, int, error) {
	if s.MailReview == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("mail review is not running on this control plane")
	}
	var req mailActRequest
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if req.Token == "" && (r.PathValue("id") == "" || req.Revision <= 0 || req.Action == "") {
		return nil, http.StatusBadRequest, fmt.Errorf("send the review token, or the exact revision and action")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	res, err := s.MailReview.Act(ctx, mailreview.ActRequest{
		Actor: actor, Via: via, Token: req.Token, Candidate: r.PathValue("id"),
		Revision: req.Revision, Action: req.Action, Fields: req.Fields,
	})
	if err != nil {
		return nil, mailStatus(err), err
	}
	return res, http.StatusOK, nil
}

func mailStatus(err error) int {
	switch {
	case errors.Is(err, runtimedb.ErrMailCandidateNotFound), errors.Is(err, runtimedb.ErrNoMailReview):
		return http.StatusNotFound
	case errors.Is(err, runtimedb.ErrMailCandidateStale), errors.Is(err, runtimedb.ErrMailReviewRefused),
		errors.Is(err, mailreview.ErrCalendarRefused), errors.Is(err, runtimedb.ErrBackgroundUseDenied):
		return http.StatusConflict
	case errors.Is(err, errMailActorDelegated):
		return http.StatusForbidden
	}
	if strings.HasPrefix(err.Error(), "runtimedb: ") {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// --- Console / CLI (and, for reads, an Agent acting for its Person).

func (s *Server) consoleMailCandidates(r *http.Request, id principal.Identity) (any, int, error) {
	return s.mailCandidates(actingMember(id), r)
}

func (s *Server) consoleMailCandidate(r *http.Request, id principal.Identity) (any, int, error) {
	return s.mailCandidate(actingMember(id), mailDirect(id), r)
}

func (s *Server) consoleMailAct(r *http.Request, id principal.Identity) (any, int, error) {
	if !mailDirect(id) {
		return nil, http.StatusForbidden, errMailActorDelegated
	}
	via := "console"
	if id.AuthTime == nil {
		via = "cli"
	}
	return s.mailAct(actingMember(id), via, r)
}

// --- Homepage (the Person's own session).

func (s *Server) homeMailCandidates(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	return s.mailCandidates(member, r)
}

func (s *Server) homeMailCandidate(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	return s.mailCandidate(member, true, r)
}

func (s *Server) homeMailAct(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	return s.mailAct(member, "home", r)
}

// --- Chat: /mail <add|apply|dismiss> <candidate> <revision>.
//
// The text fallback ADR-W030 A8.1 allows: it names the exact candidate
// and revision, arrives from an authenticated one-to-one binding, and
// passes the same Person, route, expiry and single-use checks as a
// token. It is answered here, deterministically — no model reads it —
// and never in a group.
func (s *Server) chatMail(c chatCommandCtx) (string, error) {
	if s.MailReview == nil {
		return "Mail review is not running here.", nil
	}
	fields := strings.Fields(c.Arg)
	if len(fields) == 0 {
		return "Reply with the exact words a review gave you, like /mail add mc_… 1. " +
			"Edit a proposed event in the household dashboard.", nil
	}
	action := strings.ToLower(fields[0])
	if action == runtimedb.MailActionEdit {
		return "Editing an event needs its new details: open the household dashboard to edit it.", nil
	}
	if len(fields) != 3 {
		return "Name the exact event and revision, like /mail " + action + " mc_… 1.", nil
	}
	var revision int
	if _, err := fmt.Sscanf(fields[2], "%d", &revision); err != nil || revision <= 0 {
		return "The last word is the revision number the review gave you.", nil
	}
	via := fmt.Sprintf("chat:%d", c.Binding.ID)
	if c.Web != nil {
		via = "web"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	res, err := s.MailReview.Act(ctx, mailreview.ActRequest{
		Actor: c.Identity, Via: via, Group: c.Group,
		Candidate: fields[1], Revision: revision, Action: action,
	})
	switch {
	case err == nil:
		return res.Message, nil
	case errors.Is(err, runtimedb.ErrMailCandidateNotFound), errors.Is(err, runtimedb.ErrNoMailReview):
		return "I have no event of yours waiting under that name.", nil
	case errors.Is(err, runtimedb.ErrMailCandidateStale):
		return "That event has changed since you were asked. Look at the latest review.", nil
	case errors.Is(err, runtimedb.ErrMailReviewRefused), errors.Is(err, mailreview.ErrCalendarRefused):
		msg := err.Error()
		if i := strings.LastIndex(msg, ": "); i >= 0 {
			msg = msg[i+2:]
		}
		return "Nothing was changed: " + msg + ".", nil
	}
	return "", err
}
