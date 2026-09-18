package butler

// The effective capability inventory (ADR-W030 A7/A11, work package P2).
//
// "What can you actually do here?" had no honest answer. The model was
// handed a tool list and a prompt; the resource kinds it could propose
// lived in one schema string, the household actions this deployment
// wired lived behind a seam, the QA-only verdict lived on the session,
// and nothing put them together. So the Butler could promise work it
// had no executor for, and a person could not find out what it was
// allowed to do without reading the source.
//
// A manifest is that answer, computed rather than written down: the
// ACTIVE Toolset's offered tools, the resource kinds the propose path
// actually accepts, the executors this deployment actually wired, and
// the model verdict currently in force. Each surface builds its own, so
// a household manifest cannot mention an operator tool — it is built
// from the household gateway's own tool map, which does not contain one.
//
// The manifest is also the version stamp: Fingerprint is what a work
// trace records as manifest_version (ADR-W030 A11), so a later
// before/after comparison can tell "the model behaved differently"
// from "the model was offered different tools".

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// CapabilityProfile names which Butler a manifest describes (ADR-W030
// A1). The set is closed and is decided by the gateway type, never by a
// request field: a HouseholdGateway reports household because it is one.
type CapabilityProfile string

const (
	ProfileOperator  CapabilityProfile = "operator"
	ProfileHousehold CapabilityProfile = "household"
)

// Model verdicts (ADR-W017 A4). "qa-only" is the reduced surface a model
// that failed the tool-reliability probe runs under.
const (
	VerdictFull   = "full"
	VerdictQAOnly = "qa-only"
)

// CapabilityTool is one tool as the manifest reports it.
//
// Available is separate from presence on purpose. A tool whose seam this
// deployment never wired is listed as unavailable WITH a reason rather
// than silently omitted, because "I can do that" and "nobody here has
// set that up" are different answers and a person asking what the Butler
// can do deserves the second one.
type CapabilityTool struct {
	Name     string `json:"name"`
	Summary  string `json:"summary"`
	Mutating bool   `json:"mutating,omitempty"`
	// GrantRequired marks a tool that needs a work-bound grant even when
	// it is present and not disabled (ADR-W030 A8).
	GrantRequired bool   `json:"grantRequired,omitempty"`
	Untrusted     bool   `json:"untrusted,omitempty"`
	Available     bool   `json:"available"`
	Reason        string `json:"reason,omitempty"`
}

// CapabilityAction is one executor this deployment wired — a domestic
// action a household session can actually ask for.
type CapabilityAction struct {
	Tool string `json:"tool"`
	Tier string `json:"tier"`
}

// CapabilityManifest is one surface's effective inventory for one
// session's constraints.
type CapabilityManifest struct {
	Profile      CapabilityProfile `json:"profile"`
	ModelVerdict string            `json:"modelVerdict"`
	Tools        []CapabilityTool  `json:"tools"`
	// ProposableKinds is the resource-kind registry as the propose path
	// accepts it, and is empty on any surface or session that has no
	// propose path — a QA-only operator session included. Advertising a
	// kind a session cannot propose is the same failure as advertising a
	// tool it cannot call.
	ProposableKinds []string `json:"proposableKinds,omitempty"`
	// Actions is what the deployment actually wired. Empty means no
	// executor is reachable, not "unknown".
	Actions []CapabilityAction `json:"actions,omitempty"`
	// Unavailable notes deployment gaps in one place, so a reader does
	// not have to scan Tools for Available=false.
	Unavailable []string `json:"unavailable,omitempty"`
}

// Fingerprint is a short, stable digest of everything in the manifest
// that changes what the model may do. Two sessions offered the same
// capabilities produce the same fingerprint on any host, because the
// render below is sorted and carries no timestamps or addresses.
func (m CapabilityManifest) Fingerprint() string {
	sum := sha256.Sum256([]byte(m.render()))
	return hex.EncodeToString(sum[:8])
}

// render is the canonical serialization the fingerprint is taken over
// and the self-capability tool shows. Deliberately terse and stable:
// adding a descriptive sentence to a tool must not change the
// fingerprint, so summaries are excluded and only the enforceable facts
// — names, flags, kinds, tiers — are hashed.
func (m CapabilityManifest) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "profile=%s verdict=%s\n", m.Profile, m.ModelVerdict)
	for _, t := range m.Tools {
		fmt.Fprintf(&b, "tool %s available=%t mutating=%t\n", t.Name, t.Available, t.Mutating)
	}
	for _, k := range m.ProposableKinds {
		fmt.Fprintf(&b, "kind %s\n", k)
	}
	for _, a := range m.Actions {
		fmt.Fprintf(&b, "action %s tier=%s\n", a.Tool, a.Tier)
	}
	return b.String()
}

// Describe renders the manifest for a model or a person: what is
// available, what is not and why, and what it can propose or perform.
func (m CapabilityManifest) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Profile: %s (model verdict: %s, manifest %s)\n", m.Profile, m.ModelVerdict, m.Fingerprint())

	var available, withheld []CapabilityTool
	for _, t := range m.Tools {
		if t.Available {
			available = append(available, t)
			continue
		}
		withheld = append(withheld, t)
	}
	b.WriteString("\nTools you can call now:\n")
	if len(available) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, t := range available {
		mark := ""
		if t.Mutating {
			mark = " [changes something]"
		}
		fmt.Fprintf(&b, "  %s%s — %s\n", t.Name, mark, t.Summary)
	}
	if len(withheld) > 0 {
		b.WriteString("\nNot available in this session:\n")
		for _, t := range withheld {
			fmt.Fprintf(&b, "  %s — %s\n", t.Name, t.Reason)
		}
	}
	if len(m.ProposableKinds) > 0 {
		fmt.Fprintf(&b, "\nResource kinds you can propose (for human approval): %s\n",
			strings.Join(m.ProposableKinds, ", "))
	}
	if len(m.Actions) > 0 {
		b.WriteString("\nHousehold actions wired on this deployment:\n")
		for _, a := range m.Actions {
			fmt.Fprintf(&b, "  %s (tier %s)\n", a.Tool, a.Tier)
		}
	}
	for _, note := range m.Unavailable {
		fmt.Fprintf(&b, "\nNote: %s\n", note)
	}
	return b.String()
}

// describeCapabilitiesSpec is the read-only self-capability tool
// (ADR-W030 P2). It is registered on BOTH surfaces and reports only the
// surface it was called on, so asking the household Butler what it can
// do can never enumerate the operator's tools.
var describeCapabilitiesSpec = provider.Tool{
	Name: "describe_capabilities",
	Description: "List exactly what you can do in THIS session: the tools available now, the " +
		"ones that are not and why, and what this deployment has actually wired up. Reads " +
		"nothing about the household and changes nothing. Call it when you are unsure whether " +
		"you can do what someone asked — saying you will do something you have no tool for is " +
		"worse than saying you cannot.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
}

// verdict maps a session's read-only constraint onto the recorded model
// verdict. Read-only IS the QA-only verdict at this layer (ADR-W017 A4);
// there is no third state a gateway can observe.
func verdict(readOnly bool) string {
	if readOnly {
		return VerdictQAOnly
	}
	return VerdictFull
}

// Capabilities implements Toolset for the operator surface.
//
// The proposable kinds come from resource.KnownKinds() — the same
// registry the propose path validates against — and are omitted entirely
// for a QA-only session, which has no propose path to use them with.
func (gw *ToolGateway) Capabilities(readOnly bool) CapabilityManifest {
	m := CapabilityManifest{Profile: ProfileOperator, ModelVerdict: verdict(readOnly)}
	names := make([]string, 0, len(gw.tools))
	for name := range gw.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := gw.tools[name]
		tool := CapabilityTool{
			Name:          name,
			Summary:       firstSentence(entry.Spec.Description),
			Mutating:      entry.Mutating,
			Untrusted:     entry.Untrusted,
			GrantRequired: entry.GrantRequired,
			Available:     true,
		}
		switch {
		case readOnly && entry.Mutating:
			tool.Available = false
			tool.Reason = "this session runs read-only, so tools that change anything are withheld"
		case !gw.operatorWired(name):
			tool.Available = false
			tool.Reason = "this deployment has not wired the source behind it"
		default:
			// The manifest reports what the resolver would actually
			// allow, so a description and an enforcement point cannot
			// disagree about the same tool (ADR-W030 P7).
			if d := authorize(gw.Authorizer, string(ProfileOperator), name, true,
				entry.GrantRequired, gw.SessionTask, ""); !d.Allowed {
				tool.Available = false
				tool.Reason = d.Reason
			}
		}
		if !tool.Available {
			m.Unavailable = append(m.Unavailable,
				fmt.Sprintf("%s is unavailable: %s", name, tool.Reason))
		}
		m.Tools = append(m.Tools, tool)
	}
	if !readOnly {
		for _, k := range resource.KnownKinds() {
			m.ProposableKinds = append(m.ProposableKinds, string(k))
		}
	}
	return m
}

// operatorWired reports whether the seam behind an operator tool exists
// on this deployment — the same question HouseholdGateway.wired asks,
// asked here so the manifest and the dispatch path cannot disagree about
// which tools actually work.
func (gw *ToolGateway) operatorWired(name string) bool {
	switch name {
	case "get_health", "get_container_logs":
		return gw.Reader != nil
	case "diagnose":
		return gw.Observed != nil
	case "get_automation_schema":
		return gw.AutomationVocabulary != nil
	case "dry_run_automation":
		return gw.DryRunAutomation != nil
	case "get_system_overview", "get_capability_status", "get_plan", "explain_resource", "propose_change":
		return gw.Store != nil
	}
	// describe_capabilities and anything else read purely from what the
	// gateway already holds.
	return true
}

// Capabilities implements Toolset for the family surface.
//
// It reports no resource kinds at all, on any verdict: this gateway has
// no Statefile and no propose path, so a kind list here would describe a
// capability that structurally does not exist. Its actions come from the
// same Vocabulary seam list_household_actions uses, so the manifest and
// the action list cannot drift.
func (gw *HouseholdGateway) Capabilities(readOnly bool) CapabilityManifest {
	m := CapabilityManifest{Profile: ProfileHousehold, ModelVerdict: verdict(readOnly)}
	names := make([]string, 0, len(gw.tools))
	for name := range gw.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := gw.tools[name]
		tool := CapabilityTool{
			Name:          name,
			Summary:       firstSentence(entry.Spec.Description),
			Mutating:      entry.Mutating,
			Untrusted:     entry.Untrusted,
			GrantRequired: entry.GrantRequired,
			Available:     true,
		}
		switch {
		case readOnly && entry.Mutating:
			tool.Available = false
			tool.Reason = "this session runs read-only, so tools that change anything are withheld"
		case !gw.wired(name):
			tool.Available = false
			tool.Reason = "this deployment has not wired the source behind it"
		default:
			if d := authorize(gw.Authorizer, string(ProfileHousehold), name, true,
				entry.GrantRequired, gw.SessionTask, ""); !d.Allowed {
				tool.Available = false
				tool.Reason = d.Reason
			}
		}
		if !tool.Available {
			m.Unavailable = append(m.Unavailable,
				fmt.Sprintf("%s is unavailable: %s", name, tool.Reason))
		}
		m.Tools = append(m.Tools, tool)
	}
	if gw.seams.Vocabulary != nil {
		vocab, err := gw.seams.Vocabulary()
		if err != nil {
			m.Unavailable = append(m.Unavailable,
				"the list of household actions could not be read just now")
		} else {
			for _, a := range vocab.Actions {
				// A tier this surface cannot dispatch is not a
				// capability of this surface, so it is left out rather
				// than listed and refused.
				if !householdActionTiers(a.Tier) {
					continue
				}
				m.Actions = append(m.Actions, CapabilityAction{Tool: a.Tool, Tier: a.Tier})
			}
			sort.Slice(m.Actions, func(i, j int) bool { return m.Actions[i].Tool < m.Actions[j].Tool })
		}
	}
	return m
}

// describe_capabilities takes the session's own constraints rather than
// an input, because a model must not be able to ask what it COULD do in
// a session it is not in. The dispatch path supplies readOnly; there is
// no field in the tool's input schema that could name it.
//
// It is a Describer rather than an ordinary handler so that exactly one
// tool on each surface sees the session constraint, instead of widening
// every handler's signature to carry a flag eleven of them ignore.
func describeCapabilities(gw *ToolGateway, readOnly bool) ToolOutcome {
	return ToolOutcome{Content: gw.Capabilities(readOnly).Describe()}
}

func describeHouseholdCapabilities(gw *HouseholdGateway, readOnly bool) ToolOutcome {
	return ToolOutcome{Content: gw.Capabilities(readOnly).Describe()}
}

// firstSentence trims a tool description to its opening claim — enough
// for an inventory line, without the paragraph of guidance the model
// already has in the tool spec itself.
func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}
