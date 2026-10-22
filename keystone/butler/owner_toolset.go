package butler

// OwnerToolset: an Owner Agent's tools are the operator's AND the
// household's (ADR-W033 B2/B3/B5).
//
// "Member tools may be granted to either Agent class. Owner tools may be
// granted only to Owner Agents", and a new Owner Agent "receives all
// currently available Member and Owner tools by default". The grants
// already said so — seedAgentTools gives an Owner Agent both classes — but
// a session was built from ONE gateway chosen by surface (ADR-W026 A1),
// so an Owner Agent asked to add milk to the shopping list had no
// do_household_action to do it with, and a Member tool it had been granted
// was never offered. This is the gateway half of that design catching up.
//
// It is a UNION OF TWO GATEWAYS, not one gateway with a filter, which is
// what ADR-W026 A1 rejected: each gateway keeps its own tool map, its own
// authorization (the Agent's grants, the Person's authority, the
// connection ceiling) and its own dispatch. OwnerToolset only routes a
// call to the gateway that owns the tool, and a Member Agent never gets
// one of these at all — its sessions are still built from the household
// gateway alone, so nothing here can hand an Owner tool to a household
// surface.

import (
	"context"
	"sort"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
)

// OwnerToolset is an Owner Agent's session toolset.
type OwnerToolset struct {
	Owner  *ToolGateway
	Member *HouseholdGateway
}

// NewOwnerToolset joins an operator gateway and a household gateway built
// for the same session. Either may be nil on a deployment that wired only
// one surface; the toolset is then that gateway's tools alone.
func NewOwnerToolset(owner *ToolGateway, member *HouseholdGateway) *OwnerToolset {
	return &OwnerToolset{Owner: owner, Member: member}
}

var _ Toolset = (*OwnerToolset)(nil)

// ownerTool reports whether name is the operator gateway's. On a name both
// gateways register, the operator's wins for dispatch — describe_capabilities
// excepted, which OwnerToolset answers itself from the merged manifest.
func (o *OwnerToolset) ownerTool(name string) bool {
	if o.Owner == nil {
		return false
	}
	_, ok := o.Owner.tools[name]
	return ok
}

func (o *OwnerToolset) memberTool(name string) bool {
	if o.Member == nil {
		return false
	}
	_, ok := o.Member.tools[name]
	return ok
}

// OfferedSpecs implements Toolset: the operator's offered tools, then the
// household's that the operator does not already offer under that name.
func (o *OwnerToolset) OfferedSpecs(readOnly bool) []provider.Tool {
	var out []provider.Tool
	seen := map[string]bool{}
	if o.Owner != nil {
		for _, t := range o.Owner.OfferedSpecs(readOnly) {
			seen[t.Name] = true
			out = append(out, t)
		}
	}
	if o.Member != nil {
		for _, t := range o.Member.OfferedSpecs(readOnly) {
			if !seen[t.Name] {
				seen[t.Name] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// Execute implements Toolset by routing to the gateway that owns the tool.
func (o *OwnerToolset) Execute(ctx context.Context, name string, input map[string]any, readOnly bool) (ToolOutcome, error) {
	if name == describeCapabilitiesSpec.Name {
		if ok, why := o.Permits(name, readOnly); !ok {
			return ToolOutcome{Content: why, Rejected: true}, nil
		}
		return ToolOutcome{Content: o.Capabilities(readOnly).Describe()}, nil
	}
	switch {
	case o.ownerTool(name):
		return o.Owner.Execute(ctx, name, input, readOnly)
	case o.memberTool(name):
		return o.Member.Execute(ctx, name, input, readOnly)
	case o.Owner != nil:
		// Unknown to both: the operator gateway gives the refusal it
		// always has for a tool that is not on this surface.
		return o.Owner.Execute(ctx, name, input, readOnly)
	case o.Member != nil:
		return o.Member.Execute(ctx, name, input, readOnly)
	}
	return ToolOutcome{Content: name + " is not a tool on this surface", Rejected: true}, nil
}

// Permits implements Toolset by asking the gateway that owns the tool.
func (o *OwnerToolset) Permits(name string, readOnly bool) (bool, string) {
	switch {
	case o.ownerTool(name):
		return o.Owner.Permits(name, readOnly)
	case o.memberTool(name):
		return o.Member.Permits(name, readOnly)
	case o.Owner != nil:
		return o.Owner.Permits(name, readOnly)
	case o.Member != nil:
		return o.Member.Permits(name, readOnly)
	}
	return false, name + " is not a tool on this surface"
}

// SystemPrompt implements Toolset: the operator's prompt, and — when this
// session also holds the household's tools — what an Owner Agent is to do
// with them.
func (o *OwnerToolset) SystemPrompt() string {
	switch {
	case o.Owner == nil && o.Member != nil:
		return o.Member.SystemPrompt()
	case o.Member == nil:
		return SystemPrompt
	}
	return SystemPrompt + "\n\n" + OwnerHouseholdPrompt
}

// Capabilities implements Toolset: both manifests, merged. The operator's
// class and verdict stand; tools and actions are the union, the
// household's added where the operator has no tool of that name.
func (o *OwnerToolset) Capabilities(readOnly bool) CapabilityManifest {
	var m CapabilityManifest
	if o.Owner != nil {
		m = o.Owner.Capabilities(readOnly)
	}
	if o.Member == nil {
		return m
	}
	h := o.Member.Capabilities(readOnly)
	if o.Owner == nil {
		return h
	}
	have := map[string]bool{}
	for _, t := range m.Tools {
		have[t.Name] = true
	}
	for _, t := range h.Tools {
		if !have[t.Name] {
			m.Tools = append(m.Tools, t)
		}
	}
	m.Actions = append(m.Actions, h.Actions...)
	m.Unavailable = mergeSorted(m.Unavailable, h.Unavailable)
	return m
}

func mergeSorted(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// The session binding reaches both gateways: one authorizer (the Agent's
// grants, under the Person's authority and the connection ceiling), one
// task, one caller.

func (o *OwnerToolset) SetAuthorizer(a ToolAuthorizer) {
	if o.Owner != nil {
		o.Owner.SetAuthorizer(a)
	}
	if o.Member != nil {
		o.Member.SetAuthorizer(a)
	}
}

func (o *OwnerToolset) SetSessionTask(id string) {
	if o.Owner != nil {
		o.Owner.SetSessionTask(id)
	}
	if o.Member != nil {
		o.Member.SetSessionTask(id)
	}
}

func (o *OwnerToolset) SetCaller(kind CallerKind, person, connection string) {
	if o.Owner != nil {
		o.Owner.SetCaller(kind, person, connection)
	}
	if o.Member != nil {
		o.Member.SetCaller(kind, person, connection)
	}
}

// MemoryPreamble is the household gateway's: what this Agent already
// knows about the person it is answering.
func (o *OwnerToolset) MemoryPreamble(ctx context.Context) string {
	if o.Member == nil {
		return ""
	}
	return o.Member.MemoryPreamble(ctx)
}

// OwnerHouseholdPrompt is what an Owner Agent is told about the
// household's tools it holds beside the operator's (ADR-W033 B3). It is
// appended to SystemPrompt, and it narrows that prompt's "your only write
// capability is propose_change" to infrastructure.
const OwnerHouseholdPrompt = `You also act for the household. As an Owner Agent you hold the household's tools as well as the operator's (ADR-W033 B3), and the household's are how household requests are done — "add milk to the shopping list", "what's on tomorrow", "rename that file" — never by proposing an infrastructure change:
  - list_household_actions tells you what this household has wired up: lists, the calendar, files, notifications and whatever else an installed capability offers.
  - do_household_action performs one of those actions. It happens immediately and someone sees the result, so do it when you have been asked to, not to find out what it does. An action that asks the person first (a delete, for instance) says so; relay the question rather than answering it yourself.
  - get_household_events looks up what has happened in the house, and get_upkeep, get_activity, get_notifications, get_machine_stats, remember and recall are offered where this household has them — the same tools, and the same rules, a household assistant has.
Your only write capability over INFRASTRUCTURE is still propose_change, and a Plan still needs a human to approve it. Household actions are not infrastructure changes and need no Plan.
Capabilities such as lists, files and the calendar are served by Keystone itself or by an app it runs; a capability with no containers of its own (lists has only a directory group) is not missing — its actions are in list_household_actions. Check there before telling anybody a household thing cannot be done.`
