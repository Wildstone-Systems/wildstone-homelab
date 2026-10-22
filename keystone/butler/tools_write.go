package butler

import (
	"context"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// kindDescription names every kind the gateway will accept, generated
// from the registry that does the accepting.
//
// Hand-written, this said "e.g. Container, Volume, Route, DnsRecord,
// OidcClient, LdapGroup" — six of fourteen. Asked what stopped it doing
// its job, the operator Butler answered that it could deploy a container
// and a volume but could not configure one, could not write to volumes,
// and would need a new file-editing tool with its own diff, backup and
// rollback story. All of that already exists: diffFromInput validates
// against resource.KnownKind, so ConfigFile and SeedFile were accepted,
// planned, policy-checked and git-rollback-able the whole time. The
// schema was the only thing saying otherwise, and the model believed it.
//
// So the sentence is generated. A kind added to the registry appears
// here without anyone remembering to say so, which is the only version
// of this that stays true (issue #136).
func kindDescription() string {
	kinds := resource.KnownKinds()
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, string(k))
	}
	return "Resource kind. One of: " + strings.Join(names, ", ") +
		". Use explain_resource on an existing resource of a kind to see the shape of its spec."
}

// specDiffSchema is the shared input schema for get_plan and
// propose_change: a proposed resource envelope + kind-specific spec.
var specDiffSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"kind":  map[string]any{"type": "string", "description": kindDescription()},
		"name":  map[string]any{"type": "string", "description": "Resource name."},
		"owner": map[string]any{"type": "string", "description": "Ownership chain, e.g. capability/photos or system."},
		"node":  map[string]any{"type": "string", "description": "Wardstone node id, for node-scoped kinds."},
		"spec":  map[string]any{"type": "object", "description": "Kind-specific spec fields (e.g. host/upstream/auth/classification for Route)."},
	},
	"required": []string{"kind", "name", "owner", "spec"},
}

// proposeChangeSpec is the Butler's ONE write tool (ADR-W006 A3): it
// produces a plan.Plan from a requested change and returns it. It never
// applies anything — see doc.go for the structural guarantee.
var proposeChangeSpec = provider.Tool{
	Name: "propose_change",
	Description: "Propose a resource change (create/update). Produces a Plan from the " +
		"requested change against the current Statefile and returns it for human review — " +
		"this NEVER applies the change. A policy-invalid proposal (e.g. disabling auth on " +
		"a public route) is rejected outright, before any human approval step.",
	InputSchema: specDiffSchema,
}

// diffFromInput builds the resource.Resource a spec-diff tool call
// describes, then produces (current, proposed) resource lists suitable for
// plan.Diff: current is everything presently in the Statefile; proposed is
// the same set with the requested resource created or replaced by
// (kind,name). Nothing is written to the Statefile.
func diffFromInput(gw *ToolGateway, input map[string]any) (res resource.Resource, current, proposed []resource.Resource, err error) {
	res, err = resourceFromSpecDiffInput(input)
	if err != nil {
		return resource.Resource{}, nil, nil, err
	}
	current, err = gw.Store.Load()
	if err != nil {
		return resource.Resource{}, nil, nil, fmt.Errorf("loading statefile: %w", err)
	}
	proposed = make([]resource.Resource, 0, len(current)+1)
	replaced := false
	for _, r := range current {
		if r.Kind == res.Kind && r.Metadata.Name == res.Metadata.Name {
			proposed = append(proposed, res)
			replaced = true
			continue
		}
		proposed = append(proposed, r)
	}
	if !replaced {
		proposed = append(proposed, res)
	}
	return res, current, proposed, nil
}

// resourceFromSpecDiffInput decodes a spec-diff tool-call input into a
// validated resource.Resource, reusing resource's own YAML decode/Validate
// path (the same one the Statefile itself uses) rather than a bespoke
// decoder, so a proposal is held to exactly the rules a human's `wild`
// invocation would be.
func resourceFromSpecDiffInput(input map[string]any) (resource.Resource, error) {
	kind := strings.TrimSpace(stringInput(input, "kind"))
	name := strings.TrimSpace(stringInput(input, "name"))
	owner := strings.TrimSpace(stringInput(input, "owner"))
	if kind == "" || name == "" {
		return resource.Resource{}, fmt.Errorf("kind and name are required")
	}
	if !resource.KnownKind(resource.Kind(kind)) {
		return resource.Resource{}, fmt.Errorf("unknown kind %q", kind)
	}
	doc := map[string]any{
		"apiVersion": resource.ApiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name":  name,
			"owner": owner,
			"node":  stringInput(input, "node"),
		},
		"spec": mapInput(input, "spec"),
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return resource.Resource{}, fmt.Errorf("marshaling proposed spec: %w", err)
	}
	res, err := resource.Unmarshal(data)
	if err != nil {
		return resource.Resource{}, fmt.Errorf("decoding proposed spec: %w", err)
	}
	if err := res.Validate(); err != nil {
		return resource.Resource{}, err
	}
	return res, nil
}

// diffPlan runs plan.Diff(proposed, current) — this is the ONLY place in
// this package that touches keystone/plan's Diff, and it is read-only: it
// builds a Plan value, nothing more. Neither get_plan nor propose_change
// (its only two callers) writes to the Statefile or dispatches any
// Wardstone operation.
func diffPlan(proposed, current []resource.Resource) *plan.Plan {
	return plan.Diff(proposed, current)
}

// handleProposeChange is the Butler's only write path. It builds the
// proposed resource, runs the policy check, and returns a Plan — it never
// calls gw.Store.Put/Commit and never touches a Wardstone Applier/
// Reconciler (this package doesn't import keystone/reconcile at all).
// Approval and apply are unchanged: a human, `wild`, or the Console,
// working from Outcome.Content and Outcome.Plan.
func handleProposeChange(_ context.Context, gw *ToolGateway, input map[string]any) (ToolOutcome, error) {
	res, current, proposed, err := diffFromInput(gw, input)
	if err != nil {
		return ToolOutcome{Content: fmt.Sprintf("propose_change: invalid spec: %v", err)}, nil
	}

	p := diffPlan(proposed, current)

	if perr := policyValidate(res, current); perr != nil {
		return ToolOutcome{
			Content: fmt.Sprintf(
				"REJECTED by policy — %v\n\nThe following plan was produced for audit purposes ONLY; it is rejected and cannot be approved or applied as-is:\n%s",
				perr, formatPlan(p),
			),
			Plan:     p,
			Rejected: true,
		}, nil
	}

	doc, err := resource.Marshal(res)
	if err != nil {
		return ToolOutcome{Content: fmt.Sprintf("propose_change: encoding proposal: %v", err)}, nil
	}
	return ToolOutcome{
		Content: fmt.Sprintf(
			"Proposed. This plan has NOT been applied — it requires human approval (via wild/Console) before anything changes:\n%s",
			formatPlan(p),
		),
		Plan:         p,
		ResourceYAML: string(doc),
	}, nil
}

// formatPlan renders a plan.Plan as human/model-readable text.
func formatPlan(p *plan.Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "plan: %s (overall risk: %s)\n", p.Summary, p.OverallRisk)
	if len(p.Steps) == 0 {
		b.WriteString("  (no changes: proposed spec matches current Statefile)\n")
	}
	for _, s := range p.Steps {
		fmt.Fprintf(&b, "  - %s [%s] risk=%s: %s\n", s.Description, s.Action, s.Risk, s.BlastRadius)
	}
	return b.String()
}
