package api

// Household automations (ADR-W024 H3): replaying a rule against history
// with no side effects — the only way to test a rule before approving it
// (A7), and what the Butler will use to check its own drafts (H7).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// maxReplayEvents bounds one replay's read of the stream.
const maxReplayEvents = 5000

type replayRequest struct {
	// Name replays an approved Automation from the Statefile.
	Name string `json:"name,omitempty"`
	// Rule replays a draft: one Automation YAML document, validated and
	// policy-checked but never stored.
	Rule string `json:"rule,omitempty"`
	// Routine and Spec replay a draft held as a parsed spec rather than
	// as a document: the named household routine supplies the envelope —
	// the rule's name and owner — and Spec replaces what it does.
	//
	// This is the same bargain the edit route makes. A caller holding a
	// structured spec and no YAML writer would otherwise have to grow
	// one, and a second writer of this grammar is a second thing that can
	// disagree with the engine about what a rule means.
	Routine string                   `json:"routine,omitempty"`
	Spec    *resource.AutomationSpec `json:"spec,omitempty"`
	// From/To bound the events read (occurred_at). To defaults to now;
	// From to 7 days before To.
	From time.Time `json:"from,omitzero"`
	To   time.Time `json:"to,omitzero"`
	// Limit bounds the events read (default and max maxReplayEvents).
	Limit int `json:"limit,omitempty"`
}

type replayResponse struct {
	Rule     string                `json:"rule"`
	From     time.Time             `json:"from"`
	To       time.Time             `json:"to"`
	Draft    bool                  `json:"draft"`
	Enabled  bool                  `json:"enabled"`
	Fired    int                   `json:"fired"`
	Total    int                   `json:"evaluated"`
	Verdicts []automation.Decision `json:"decisions"`
	// Violations are policy findings on a draft — what a proposal of
	// this rule would be refused for. Informational here: a replay is
	// not a proposal.
	Violations []policy.Violation `json:"violations,omitempty"`
}

// replayAutomation runs a rule over history. A read verb: nothing is
// written, nothing acts, and a draft in the body is discarded after.
func (s *Server) replayAutomation(r *http.Request, id principal.Identity) (any, int, error) {
	if s.Automation == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("automation engine not wired")
	}
	var req replayRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxProposalBytes)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing request: %w", err)
	}
	// Exactly one way of naming what to replay.
	ways := 0
	for _, given := range []bool{req.Name != "", req.Rule != "", req.Routine != "" || req.Spec != nil} {
		if given {
			ways++
		}
	}
	if ways != 1 {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"exactly one of name (an approved rule), rule (a draft YAML document), or routine+spec (a draft held as a parsed spec) is required")
	}
	if (req.Routine == "") != (req.Spec == nil) {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"routine and spec go together: the routine supplies the rule's envelope and the spec replaces what it does")
	}

	current, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	var rule resource.Resource
	draft := req.Rule != "" || req.Spec != nil
	switch {
	case req.Rule != "":
		parsed, perr := automation.ParseDraft(req.Rule)
		if perr != nil {
			return nil, http.StatusBadRequest, perr
		}
		rule = parsed
	case req.Spec != nil:
		routine, rerr := s.DB.GetHouseholdRoutine(req.Routine)
		if rerr != nil {
			if errors.Is(rerr, runtimedb.ErrNotFound) {
				return nil, http.StatusNotFound, fmt.Errorf("no routine named %q", req.Routine)
			}
			return nil, http.StatusInternalServerError, rerr
		}
		envelope, perr := automation.ParseDraft(routine.Document)
		if perr != nil {
			return nil, http.StatusBadRequest, fmt.Errorf(
				"this routine's current document does not parse, so a spec has no envelope to go in: %w", perr)
		}
		envelope.Automation = req.Spec
		// Round-tripped rather than used directly: a spec that will not
		// survive being written and read back is not a rule, and finding
		// that out here is better than replaying something the engine
		// would refuse.
		out, merr := resource.Marshal(envelope)
		if merr != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("writing the rule: %w", merr)
		}
		parsed, perr := automation.ParseDraft(string(out))
		if perr != nil {
			return nil, http.StatusBadRequest, perr
		}
		rule = parsed
	default:
		found := false
		for _, res := range current {
			if res.Kind == resource.KindAutomation && res.Metadata.Name == req.Name {
				rule, found = res, true
				break
			}
		}
		if !found {
			return nil, http.StatusNotFound, fmt.Errorf("no Automation named %q", req.Name)
		}
	}

	to := req.To
	if to.IsZero() {
		to = time.Now()
	}
	from := req.From
	if from.IsZero() {
		from = to.Add(-automation.DefaultDraftWindow)
	}
	if !from.Before(to) {
		return nil, http.StatusBadRequest, fmt.Errorf("from must be before to")
	}
	limit := req.Limit
	if limit <= 0 || limit > maxReplayEvents {
		limit = maxReplayEvents
	}

	// One implementation, shared with the Butler's dry_run_automation
	// tool (ADR-W024 H7): a rule the Butler tried is a rule the operator
	// can try identically, and neither gets a different verdict.
	res, err := s.Automation.ReplayDraft(r.Context(), rule, s.Policy, id.Name, string(id.Role), from, to, limit)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := replayResponse{
		Rule: res.Rule, From: res.From, To: res.To, Draft: draft, Enabled: res.Enabled,
		Total: res.Evaluated, Fired: res.Fired, Verdicts: res.Decisions, Violations: res.Violations,
	}
	return out, http.StatusOK, nil
}
