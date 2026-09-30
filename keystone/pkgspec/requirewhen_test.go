package pkgspec

import (
	"strings"
	"testing"
)

// requirementBehind is a package offering an optional integration: its
// mqtt:broker requirement is switched by one param.
func requirementBehind(when string, params []ParamDecl) *Package {
	return &Package{
		ApiVersion: ApiVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "cameras-fixture", Version: "1.0.0", Capability: "cameras"},
		Requires:   []Requirement{{Trait: "mqtt", Mode: "broker", When: when}},
		Params:     params,
		Templates: []string{`apiVersion: wildstone.dev/v1
kind: Volume
metadata: { name: fixture, node: n1 }
spec: { name: fixture }
`},
	}
}

func TestConditionalRequirementNamesASwitch(t *testing.T) {
	for _, shape := range []string{"enum:true|false", "enum:false|true"} {
		pkg := requirementBehind("mqtt_enabled", []ParamDecl{{Name: "mqtt_enabled", Default: "false", Validate: shape}})
		if err := pkg.Validate(); err != nil {
			t.Errorf("%s: a true/false switch must validate: %v", shape, err)
		}
	}
	for name, params := range map[string][]ParamDecl{
		"undeclared": nil,
		"free-form":  {{Name: "mqtt_enabled", Default: "false"}},
		"not bool":   {{Name: "mqtt_enabled", Default: "off", Validate: "enum:on|off"}},
		"three-way":  {{Name: "mqtt_enabled", Default: "false", Validate: "enum:true|false|auto"}},
	} {
		err := requirementBehind("mqtt_enabled", params).Validate()
		if err == nil || !strings.Contains(err.Error(), "requires[0].when") {
			t.Errorf("%s: want a requires[0].when error, got %v", name, err)
		}
	}
}

// Only the literal "true" turns a requirement on; no `when` is always on.
func TestRequirementAppliesTo(t *testing.T) {
	always := Requirement{Trait: "mqtt", Mode: "broker"}
	if !always.AppliesTo(nil) {
		t.Error("a requirement with no `when` must always apply")
	}
	switched := Requirement{Trait: "mqtt", Mode: "broker", When: "mqtt_enabled"}
	for v, want := range map[string]bool{"true": true, "false": false, "": false, "True": false, "yes": false} {
		if got := switched.AppliesTo(map[string]string{"mqtt_enabled": v}); got != want {
			t.Errorf("mqtt_enabled=%q: AppliesTo = %v, want %v", v, got, want)
		}
	}
}
