package butler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// ObservedResource is the Butler-local view of one resource's latest
// observed health — the injected read-only seam ADR-W017 A5 requires:
// cmd/keystone maps the runtime DB's observed rows into this shape, and
// this package keeps importing neither keystone/reconcile nor
// keystone/runtimedb (structural_test.go pins the former).
type ObservedResource struct {
	Kind             string
	Name             string
	Node             string
	Healthy          bool
	AwaitingApproval bool
	Problem          bool
	Starting         bool
	UpdatedAt        time.Time
	// Detail is agent/reconciler-composed free text — attacker-
	// influenceable, which is why the diagnose tool is registered
	// Untrusted and its whole result rides inside the envelope.
	Detail string
}

// diagnoseSpec is the dependency-chain walker (ADR-W017 A5): the
// difference between "photos is broken" and "immich-postgres is down,
// which is why immich-server and the photos route are failing".
var diagnoseSpec = provider.Tool{
	Name: "diagnose",
	Description: "Diagnose a capability: walk its resources in dependency order (volumes -> " +
		"identity wiring -> containers -> routes -> DNS -> backups) against observed health " +
		"and name the deepest broken dependency as the likely root cause, with downstream " +
		"unhealthy resources listed as consequences.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"capability": map[string]any{
				"type":        "string",
				"description": "Capability name, e.g. photos (owner chain capability/photos).",
			},
		},
		"required": []string{"capability"},
	},
}

// diagnosisEntry is one walked resource: its declared position in the
// dependency order plus what was observed for it.
type diagnosisEntry struct {
	Res      resource.Resource
	Order    int
	Observed *ObservedResource // nil: no observation recorded yet
}

func (e diagnosisEntry) broken() bool {
	return e.Observed == nil || e.Observed.Problem || (!e.Observed.Healthy && !e.Observed.AwaitingApproval)
}

func (e diagnosisEntry) waiting() bool {
	return e.Observed != nil && (e.Observed.AwaitingApproval || e.Observed.Starting)
}

func (e diagnosisEntry) describe() string {
	// Space-separated (not Kind/name): a joined 24+-char [A-Za-z/-] run
	// trips the shared redactor's aggressive base64-shape rule and the
	// resource id would reach the model as [REDACTED].
	id := fmt.Sprintf("%s %s", e.Res.Kind, e.Res.Metadata.Name)
	if e.Observed == nil {
		return id + ": no observed status recorded (never reconciled, or observation lost)"
	}
	withObservedAt := func(s string) string {
		if e.Observed.UpdatedAt.IsZero() {
			return s
		}
		return s + " (observed " + e.Observed.UpdatedAt.UTC().Format(time.RFC3339) + ")"
	}
	if e.Observed.Problem {
		if d := strings.TrimSpace(e.Observed.Detail); d != "" {
			return withObservedAt(fmt.Sprintf("%s: UNHEALTHY — %s", id, d))
		}
		return withObservedAt(id + ": UNHEALTHY")
	}
	if e.Observed.Starting {
		if d := strings.TrimSpace(e.Observed.Detail); d != "" {
			return withObservedAt(fmt.Sprintf("%s: STARTING — %s", id, d))
		}
		return withObservedAt(id + ": STARTING")
	}
	if e.Observed.AwaitingApproval {
		if d := strings.TrimSpace(e.Observed.Detail); d != "" {
			return withObservedAt(fmt.Sprintf("%s: WAITING FOR APPROVAL — %s", id, d))
		}
		return withObservedAt(id + ": WAITING FOR APPROVAL")
	}
	if e.Observed.Healthy {
		return withObservedAt(id + ": healthy")
	}
	if d := strings.TrimSpace(e.Observed.Detail); d != "" {
		return withObservedAt(fmt.Sprintf("%s: UNHEALTHY — %s", id, d))
	}
	return withObservedAt(id + ": UNHEALTHY")
}

// handleDiagnose walks one capability's resources in plan.KindOrder
// against the injected observed seam. Read-only: it loads the Statefile
// and calls gw.Observed, nothing else.
func handleDiagnose(_ context.Context, gw *ToolGateway, input map[string]any) (ToolOutcome, error) {
	cap := strings.TrimSpace(stringInput(input, "capability"))
	if cap == "" {
		return ToolOutcome{Content: "diagnose: capability is required"}, nil
	}
	if gw.Observed == nil {
		return ToolOutcome{Content: "diagnose: no observed-state source is configured on this Keystone"}, nil
	}

	all, err := gw.Store.Load()
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("loading statefile: %w", err)
	}
	owner := "capability/" + cap
	var owned []resource.Resource
	for _, r := range all {
		if r.Metadata.Owner == owner || strings.HasPrefix(r.Metadata.Owner, owner+"/") {
			owned = append(owned, r)
		}
	}
	if len(owned) == 0 {
		return ToolOutcome{Content: fmt.Sprintf("diagnose: no resources owned by %s in the Statefile", owner)}, nil
	}

	observed, err := gw.Observed()
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("reading observed state: %w", err)
	}
	byKey := make(map[string]*ObservedResource, len(observed))
	for i := range observed {
		o := &observed[i]
		byKey[o.Kind+"/"+o.Name] = o
	}

	entries := make([]diagnosisEntry, 0, len(owned))
	for _, r := range owned {
		entries = append(entries, diagnosisEntry{
			Res:      r,
			Order:    plan.KindOrder(r.Kind),
			Observed: byKey[string(r.Kind)+"/"+r.Metadata.Name],
		})
	}
	// Dependency order, then name: the first broken entry IS the deepest
	// broken dependency.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Order != entries[j].Order {
			return entries[i].Order < entries[j].Order
		}
		return entries[i].Res.Metadata.Name < entries[j].Res.Metadata.Name
	})

	var broken, waiting, healthy []diagnosisEntry
	for _, e := range entries {
		switch {
		case e.broken():
			broken = append(broken, e)
		case e.waiting():
			waiting = append(waiting, e)
		default:
			healthy = append(healthy, e)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "diagnosis for capability %s (%d resources):\n", cap, len(entries))
	if len(broken) == 0 {
		if len(waiting) == 0 {
			b.WriteString("all resources healthy — nothing to diagnose.\n")
		} else {
			b.WriteString("no resources are known broken.\n")
			b.WriteString("waiting:\n")
			for _, e := range waiting {
				fmt.Fprintf(&b, "  - %s\n", e.describe())
			}
		}
		return ToolOutcome{Content: b.String()}, nil
	}
	root := broken[0]
	fmt.Fprintf(&b, "LIKELY ROOT CAUSE (deepest broken dependency): %s\n", root.describe())
	if len(broken) > 1 {
		b.WriteString("consequences (unhealthy downstream of the root cause):\n")
		for _, e := range broken[1:] {
			fmt.Fprintf(&b, "  - %s\n", e.describe())
		}
	}
	if len(waiting) > 0 {
		b.WriteString("waiting (not diagnosed as failures):\n")
		for _, e := range waiting {
			fmt.Fprintf(&b, "  - %s\n", e.describe())
		}
	}
	if len(healthy) > 0 {
		names := make([]string, 0, len(healthy))
		for _, e := range healthy {
			names = append(names, fmt.Sprintf("%s %s", e.Res.Kind, e.Res.Metadata.Name))
		}
		fmt.Fprintf(&b, "healthy: %s\n", strings.Join(names, ", "))
	}
	return ToolOutcome{Content: b.String()}, nil
}
