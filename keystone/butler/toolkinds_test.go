package butler

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// Every tool schema that takes a `kind` must name every kind the gateway
// will accept.
//
// The Butler told an operator it could deploy a container and a volume
// but could not configure one, could not write to volumes, and would
// need a new file-editing tool with its own diff, backup and rollback
// story. None of that was true: diffFromInput validates against
// resource.KnownKind, so ConfigFile and SeedFile were accepted,
// planned, policy-checked and git-rollback-able the whole time.
//
// The model was reading the tool's own input schema, which listed six of
// fourteen kinds — and explain_resource, the tool it would use to learn
// a spec's shape, listed three. A model that believes a capability does
// not exist will not use it, and will file requests for things it can
// already do.
//
// Walking every offered spec rather than the two known ones, because the
// failure is a hand-written list drifting from the registry, and a third
// one would drift the same way (issue #136).
func TestEveryToolSchemaNamesEveryKnownKind(t *testing.T) {
	kinds := resource.KnownKinds()
	if len(kinds) < 10 {
		t.Fatalf("registry looks wrong: %d kinds", len(kinds))
	}

	checked := 0
	for _, readOnly := range []bool{false, true} {
		for _, spec := range NewToolGateway(nil, nil).OfferedSpecs(readOnly) {
			desc, ok := kindPropertyDescription(spec)
			if !ok {
				continue
			}
			checked++

			var named []string
			for _, k := range kinds {
				if strings.Contains(desc, string(k)) {
					named = append(named, string(k))
				}
			}
			// A tool constrained to ONE kind is not hiding anything —
			// dry_run_automation says `Must be "Automation"` and means
			// it. What must never happen is an arbitrary subset: that is
			// a tool which accepts fourteen advertising six, and a model
			// reading the schema has no way to know the difference.
			switch len(named) {
			case 1, len(kinds):
			default:
				missing := make([]string, 0, len(kinds))
				for _, k := range kinds {
					if !strings.Contains(desc, string(k)) {
						missing = append(missing, string(k))
					}
				}
				t.Errorf("tool %q names %d of %d kinds in its `kind` input, missing %s.\n"+
					"A schema either constrains to one kind and says so, or lists them all. An "+
					"arbitrary subset is the model being told a capability does not exist — which is "+
					"exactly what happened: propose_change named six, and the Butler reported it "+
					"could not write a ConfigFile the gateway would have accepted.",
					spec.Name, len(named), len(kinds), strings.Join(missing, ", "))
			}
		}
	}
	if checked == 0 {
		t.Fatal("no offered tool had a `kind` input — this test walked nothing")
	}
}

// kindPropertyDescription pulls the description of a tool's `kind` input,
// if it has one.
func kindPropertyDescription(spec provider.Tool) (string, bool) {
	props, ok := spec.InputSchema["properties"].(map[string]any)
	if !ok {
		return "", false
	}
	kind, ok := props["kind"].(map[string]any)
	if !ok {
		return "", false
	}
	desc, _ := kind["description"].(string)
	return desc, true
}
