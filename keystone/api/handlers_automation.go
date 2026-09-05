package api

// Household automations (ADR-W022 H3): replaying a rule against history
// with no side effects — the only way to test a rule before approving it
// (A7), and what the Butler will use to check its own drafts (H7).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// maxReplayEvents bounds one replay's read of the stream.
const maxReplayEvents = 5000

type replayRequest struct {
	// Name replays an approved Automation from the Statefile.
	Name string `json:"name,omitempty"`
	// Rule replays a draft: one Automation YAML document, validated and
	// policy-checked but never stored.
	Rule string `json:"rule,omitempty"`
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
	if (req.Name == "") == (req.Rule == "") {
		return nil, http.StatusBadRequest, fmt.Errorf("exactly one of name (an approved rule) or rule (a draft YAML document) is required")
	}

	current, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	var rule resource.Resource
	var violations []policy.Violation
	draft := req.Rule != ""
	if draft {
		parsed, err := resource.Unmarshal([]byte(req.Rule))
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("parsing rule: %w", err)
		}
		if parsed.Kind != resource.KindAutomation {
			return nil, http.StatusBadRequest, fmt.Errorf("rule must be kind Automation, got %s", parsed.Kind)
		}
		if err := parsed.Validate(); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("invalid rule: %w", err)
		}
		violations, _ = s.Policy.Validate(policy.Change{
			Proposed: []resource.Resource{parsed}, Current: current, Principal: id.Name, Role: string(id.Role),
		})
		rule = parsed
	} else {
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
	// A disabled rule replays as if enabled: the question is what it
	// WOULD do, which is why one would replay it before turning it on.
	// Copied, so the Statefile's loaded resource is never mutated.
	spec := *rule.Automation
	enabled := !spec.Disabled
	spec.Disabled = false
	rule.Automation = &spec

	to := req.To
	if to.IsZero() {
		to = time.Now()
	}
	from := req.From
	if from.IsZero() {
		from = to.Add(-7 * 24 * time.Hour)
	}
	if !from.Before(to) {
		return nil, http.StatusBadRequest, fmt.Errorf("from must be before to")
	}
	limit := req.Limit
	if limit <= 0 || limit > maxReplayEvents {
		limit = maxReplayEvents
	}
	decisions, err := s.Automation.Replay(r.Context(), []resource.Resource{rule}, from, to, limit)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := replayResponse{
		Rule: rule.Metadata.Name, From: from, To: to, Draft: draft, Enabled: enabled,
		Total: len(decisions), Verdicts: decisions, Violations: violations,
	}
	for _, d := range decisions {
		if d.Fired {
			out.Fired++
		}
	}
	return out, http.StatusOK, nil
}
