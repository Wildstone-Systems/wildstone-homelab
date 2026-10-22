package domestic

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// HouseholdCapability is Keystone's own: the household's settings, which
// no package provides. Its tools are declared by BuiltinAgentTools.
const HouseholdCapability = "household"

// BuiltinAgentTools are the actions Keystone offers itself rather than a
// package, with their tiers, so the gateway's declaration check covers
// them exactly as it covers a package's agentTools.
var BuiltinAgentTools = []AgentToolSource{{
	Capability: HouseholdCapability,
	Tools:      map[string]string{"propose_timezone": TierDomestic},
}}

// TimezoneProposer is household.propose_timezone: an Agent noticing the
// household's clock is wrong proposes the right zone, and an Owner
// approves it (ADR-W033 E2: Agents propose; human Owners activate). It
// changes nothing itself.
type TimezoneProposer struct {
	// Propose records the proposal for Owner review and returns its plan
	// id and the zone the household keeps now.
	Propose func(ctx context.Context, zone, reason, proposedBy string) (plan, current string, err error)
}

func (p *TimezoneProposer) Name() string { return HouseholdCapability + ".propose_timezone" }

func (p *TimezoneProposer) Usage() string {
	return "Propose changing the household's timezone, when the household tells you its clock is wrong " +
		"(times land hours off, or they say where they live). params: zone (required, an IANA name like " +
		"America/Chicago), reason (optional, in the household's words). It changes nothing until an Owner " +
		"approves it in the Console; tell the person so."
}

func (p *TimezoneProposer) Execute(ctx context.Context, c Call) (Result, error) {
	if p.Propose == nil {
		return Result{}, &RefusalError{Tool: c.Tool, Reason: "timezone proposals are not wired on this control plane"}
	}
	zone := strings.TrimSpace(c.Params["zone"])
	if zone == "" {
		return Result{}, fmt.Errorf("param %q is required: an IANA zone like America/Chicago", "zone")
	}
	if _, err := time.LoadLocation(zone); err != nil || zone == "Local" {
		return Result{}, fmt.Errorf("%q is not a time zone name; use an IANA name like America/Chicago", zone)
	}
	plan, current, err := p.Propose(ctx, zone, strings.TrimSpace(c.Params["reason"]), c.Subject)
	if err != nil {
		return Result{}, err
	}
	return Result{ProposalConfirmed: true,
		Summary: fmt.Sprintf("proposed changing the household timezone from %s to %s (plan %s); "+
			"nothing changes until an Owner approves it in the Console", current, zone, plan),
		Detail: map[string]string{"zone": zone, "from": current, "plan": plan},
	}, nil
}
