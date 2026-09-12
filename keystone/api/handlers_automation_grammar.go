package api

// The closed grammar a household rule is written in (ADR-W024 A5).
//
// A routine's trigger must name an event something actually emits, and
// its action must name a tool at a tier policy will accept. Both
// vocabularies already exist — packages declare `publishes` and
// `agentTools`, resource/automation exports the condition and context
// kinds, policy exports the tiers a rule may carry — and none of them
// reached the Console, so a form had nothing to build a <select> from.
//
// Offering free text instead would be worse than offering nothing: a
// routine that triggers on an event no package emits never fires, and
// nothing tells its author why. Same for a tier — the
// automation-action-tier rule refuses anything above domestic, so
// offering "high" is offering a rejection.

import (
	"net/http"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// grammarEvent is one event type a rule may trigger on.
type grammarEvent struct {
	Type        string `json:"type"`
	Package     string `json:"package"`
	Capability  string `json:"capability,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Description string `json:"description,omitempty"`
}

// grammarAction is one tool a rule may perform.
type grammarAction struct {
	// Tool is "<capability>.<tool>", the form the schema requires.
	Tool       string `json:"tool"`
	Tier       string `json:"tier"`
	Package    string `json:"package"`
	Capability string `json:"capability,omitempty"`
}

type automationGrammar struct {
	EventTypes []grammarEvent  `json:"eventTypes"`
	Actions    []grammarAction `json:"actions"`
	// The vocabularies validation itself enforces, read from the schema
	// rather than restated, so a form cannot drift from the validator.
	ConditionKinds []string `json:"conditionKinds"`
	ContextKinds   []string `json:"contextKinds"`
	Tiers          []string `json:"tiers"`
	Weekdays       []string `json:"weekdays"`
}

// automationGrammar answers what a household rule may say.
//
// Only INSTALLED packages contribute. A catalogue entry nobody has
// installed emits no events and exposes no tools, so offering its
// vocabulary would offer a rule that can never fire.
func (s *Server) automationGrammar(_ *http.Request, _ principal.Identity) (any, int, error) {
	out := automationGrammar{
		EventTypes:     []grammarEvent{},
		Actions:        []grammarAction{},
		ConditionKinds: resource.AutomationConditionKinds(),
		ContextKinds:   resource.AutomationContextKinds(),
		Tiers:          policy.AutomationTiers(),
		Weekdays:       resource.AutomationWeekdays(),
	}
	if s.Catalog == nil {
		// No catalogue is not an error: the closed vocabularies above are
		// still the truth, and a form can still render its condition and
		// tier choices. It simply has no events or tools to offer.
		return out, http.StatusOK, nil
	}
	current, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	seenEvent := map[string]bool{}
	seenTool := map[string]bool{}
	for _, p := range s.Catalog.List() {
		if !packageIsInstalled(p, current) {
			continue
		}
		if p.Publishes != nil {
			for _, e := range p.Publishes.Events {
				if e.Type == "" || seenEvent[e.Type] {
					continue
				}
				seenEvent[e.Type] = true
				out.EventTypes = append(out.EventTypes, grammarEvent{
					Type: e.Type, Package: p.Metadata.Name,
					Capability: p.Metadata.Capability,
					// "none" is the schema's way of saying this event is
					// about the house rather than a person, and it is
					// worth showing: a rule cannot match a subject on it.
					Subject: e.Subject, Description: e.Description,
				})
			}
		}
		for _, t := range p.AgentTools {
			// A tool's risk IS the tier a rule would carry, so anything
			// policy refuses is filtered here rather than offered and
			// rejected later.
			if !allowedTier(t.Risk, out.Tiers) {
				continue
			}
			tool := qualifiedTool(p, t)
			if tool == "" || seenTool[tool] {
				continue
			}
			seenTool[tool] = true
			out.Actions = append(out.Actions, grammarAction{
				Tool: tool, Tier: t.Risk, Package: p.Metadata.Name,
				Capability: p.Metadata.Capability,
			})
		}
	}
	return out, http.StatusOK, nil
}

func allowedTier(tier string, allowed []string) bool {
	for _, a := range allowed {
		if a == tier {
			return true
		}
	}
	return false
}

// qualifiedTool renders a declared agentTool as the schema writes it:
// "<capability>.<tool>". A package that already qualifies its tool name
// is left alone.
func qualifiedTool(p *pkgspec.Package, t pkgspec.AgentToolSpec) string {
	if t.Name == "" {
		return ""
	}
	if strings.Contains(t.Name, ".") {
		return t.Name
	}
	if p.Metadata.Capability == "" {
		return ""
	}
	return p.Metadata.Capability + "." + t.Name
}
