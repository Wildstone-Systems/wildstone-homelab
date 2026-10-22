package resource

// What a resource spec looks like on the wire as JSON.
//
// Specs are authored and stored as YAML, and the Console's TypeScript is
// generated from the YAML tags (resource/gen). A field whose json tag
// disagrees with its yaml tag is therefore served under a name no
// generated client reads.
//
// This repository has paid for that once already. render-check.tsx opens
// by describing it: pkgspec.ParamDecl carried yaml tags only, the API
// served {"Name","Required","Default"}, the Console read
// name/required/default, and the generated install form rendered every
// field blank on a 200 response. The routine detail read is the first
// place a resource SPEC crosses the wire as JSON rather than as a YAML
// string, so the same trap was reachable again.
//
// Two rules, because they carry different risk.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// specTypes is every spec in the schema.
func specTypes() map[string]reflect.Type {
	return map[string]reflect.Type{
		"AutomationSpec": reflect.TypeOf(AutomationSpec{}),
		"ContainerSpec":  reflect.TypeOf(ContainerSpec{}),
		"VolumeSpec":     reflect.TypeOf(VolumeSpec{}),
		"RouteSpec":      reflect.TypeOf(RouteSpec{}),
		"DnsRecordSpec":  reflect.TypeOf(DnsRecordSpec{}),
		"OidcClientSpec": reflect.TypeOf(OidcClientSpec{}),
		"SecretSpec":     reflect.TypeOf(SecretSpec{}),
		"JobSpec":        reflect.TypeOf(JobSpec{}),
		"SeedFileSpec":   reflect.TypeOf(SeedFileSpec{}),
		"ConfigFileSpec": reflect.TypeOf(ConfigFileSpec{}),
		"LdapGroupSpec":  reflect.TypeOf(LdapGroupSpec{}),
		"IngressSpec":    reflect.TypeOf(IngressSpec{}),
		"LdapUserSpec":   reflect.TypeOf(LdapUserSpec{}),
		"BackupPlanSpec": reflect.TypeOf(BackupPlanSpec{}),
	}
}

// servedAsJSON are the spec types an API hands to a client as JSON.
// Those must name every field, because a field with no json tag goes out
// under its Go name and arrives as undefined.
//
// Automation is here because GET /api/v1/household/routines/{name}
// serves the parsed spec so the Console needs no YAML parser of its own.
// Add a type here when an endpoint starts serving it, and tag its fields
// in the same change.
var servedAsJSON = map[string]bool{"AutomationSpec": true}

// TestSpecJSONTagsAgreeWithYAML is the rule that holds everywhere: a
// json tag that disagrees with its yaml tag is a field served under a
// name the generated client does not read.
//
// It does NOT require a json tag. On ContainerSpec a missing tag is
// deliberate and load-bearing: the reconciler's spec hash is
// json.Marshal of that struct, stored as a container label, so adding
// tags would move the hash of every container in the fleet and replace
// all of them once for nothing. See the note on ContainerSpec.Devices
// and TestContainerWithNoManagedConfigIsUnaffected. Tag a spec only when
// something actually serves it, and expect to think about the hash if
// that spec is ContainerSpec.
func TestSpecJSONTagsAgreeWithYAML(t *testing.T) {
	for name, typ := range specTypes() {
		for i := range typ.NumField() {
			f := typ.Field(i)
			yamlName := strings.Split(f.Tag.Get("yaml"), ",")[0]
			jsonName := strings.Split(f.Tag.Get("json"), ",")[0]
			if yamlName == "" || yamlName == "-" || jsonName == "" {
				continue
			}
			if jsonName != yamlName {
				t.Errorf("%s.%s is yaml:%q but json:%q — the generated client reads the yaml name",
					name, f.Name, yamlName, jsonName)
			}
		}
	}
}

// TestSpecsServedAsJSONNameEveryField is the stricter rule for the specs
// that actually reach a browser.
func TestSpecsServedAsJSONNameEveryField(t *testing.T) {
	types := specTypes()
	for name := range servedAsJSON {
		typ, ok := types[name]
		if !ok {
			t.Fatalf("servedAsJSON names %s, which is not a spec type", name)
		}
		walkSpecFields(t, name, typ)
	}
}

// walkSpecFields checks a spec and the structs it contains, since a
// nested type served under a tagged field is just as reachable.
func walkSpecFields(t *testing.T, name string, typ reflect.Type) {
	t.Helper()
	for i := range typ.NumField() {
		f := typ.Field(i)
		yamlName := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if yamlName == "" || yamlName == "-" {
			continue
		}
		if strings.Split(f.Tag.Get("json"), ",")[0] == "" {
			t.Errorf("%s.%s has yaml:%q and no json tag, so it is served as %q — a name no client reads",
				name, f.Name, yamlName, f.Name)
		}
		ft := f.Type
		for ft.Kind() == reflect.Ptr || ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && strings.HasPrefix(ft.Name(), "Automation") {
			walkSpecFields(t, ft.Name(), ft)
		}
	}
}

// TestAutomationSpecServesTheNamesTheConsoleReads is the same guarantee
// from the outside: marshal a real spec and look at the keys a browser
// would actually receive.
func TestAutomationSpecServesTheNamesTheConsoleReads(t *testing.T) {
	doc := `apiVersion: wildstone.dev/v1
kind: Automation
metadata: { name: mail-check, owner: household }
spec:
  description: Ask whether the mail was collected
  trigger: { eventType: person.arrived_home }
  conditions:
    - { kind: timeOfDay, after: "12:00" }
  action: { tool: notifications.notify, tier: domestic, params: { message: "Did you get the mail?" } }
`
	r, err := Unmarshal([]byte(doc))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	b, err := json.Marshal(r.Automation)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal json: %v", err)
	}
	for _, key := range []string{"description", "trigger", "conditions", "action"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("the spec serves no %q key; a client reading it gets undefined:\n%s", key, b)
		}
	}
	trigger, _ := got["trigger"].(map[string]any)
	if trigger["eventType"] != "person.arrived_home" {
		t.Fatalf("trigger.eventType is not where a client looks for it: %s", b)
	}
	action, _ := got["action"].(map[string]any)
	if action["tier"] != "domestic" || action["tool"] != "notifications.notify" {
		t.Fatalf("action.tool/tier are not where a client looks for them: %s", b)
	}
	// The capitalised Go field names must not be what goes out.
	if strings.Contains(string(b), `"Description"`) || strings.Contains(string(b), `"EventType"`) {
		t.Fatalf("the spec is serving Go field names:\n%s", b)
	}
}
