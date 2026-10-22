package api

import (
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
)

// A param the deployment can answer should arrive answered. Before the
// keystone_url source existed, every operator installing a chat adapter
// had to work out which of the host's addresses a CONTAINER can reach —
// and discover that 127.0.0.1 means the container itself.
func TestResolveParamsFillsWhatTheDeploymentKnows(t *testing.T) {
	facts := deploymentFacts{
		nodes:       []string{"n1"},
		domain:      "home.arpa",
		keystoneURL: "http://100.64.0.1:9441",
	}
	got := resolveParams([]pkgspec.ParamDecl{
		{Name: "node", Source: "node"},
		{Name: "public_domain", Source: "domain"},
		{Name: "keystone_url", Source: "keystone_url"},
		{Name: "freeform"},
	}, facts, nil)

	by := map[string]paramView{}
	for _, v := range got {
		by[v.Name] = v
	}
	// A single node is both the only option and the suggestion.
	if len(by["node"].Options) != 1 || by["node"].Suggested != "n1" {
		t.Fatalf("node = %+v", by["node"])
	}
	if by["public_domain"].Suggested != "home.arpa" {
		t.Fatalf("domain = %+v", by["public_domain"])
	}
	if by["keystone_url"].Suggested != "http://100.64.0.1:9441" {
		t.Fatalf("keystone_url = %+v", by["keystone_url"])
	}
	// A param with no source is the operator's to answer, and must not
	// acquire a suggestion from somewhere.
	if by["freeform"].Suggested != "" || len(by["freeform"].Options) != 0 {
		t.Fatalf("freeform = %+v", by["freeform"])
	}
}

// A deployment that cannot answer leaves the field blank. A wrong
// address prefilled is worse than an empty box, because it looks
// decided.
func TestResolveParamsSuggestsNothingItDoesNotKnow(t *testing.T) {
	got := resolveParams([]pkgspec.ParamDecl{
		{Name: "keystone_url", Source: "keystone_url"},
		{Name: "public_domain", Source: "domain"},
	}, deploymentFacts{}, nil)
	for _, v := range got {
		if v.Suggested != "" {
			t.Fatalf("%s suggested %q from an empty deployment", v.Name, v.Suggested)
		}
	}
}

// Every source a package may declare has to do SOMETHING: either
// resolveParams supplies a value for it, or it is declared as one that
// selects a control instead (pkgspec.ParamSourcesSelectingAControl).
// Otherwise a package can name a source that silently does nothing.
func TestEveryDeclaredParamSourceIsResolved(t *testing.T) {
	facts := deploymentFacts{
		nodes:       []string{"n1"},
		domain:      "home.arpa",
		keystoneURL: "http://n1:9441",
		timezone:    "Europe/London",
		intakeURL:   "http://n1:9441/api/v1/household/events",
	}
	for source := range pkgspec.ParamSources {
		got := resolveParams([]pkgspec.ParamDecl{{Name: "p", Source: source}}, facts, nil)
		if len(got) != 1 {
			t.Fatalf("source %q: resolveParams returned %d params", source, len(got))
		}
		if pkgspec.ParamSourcesSelectingAControl[source] {
			// It picks the form's control; there is no value to resolve.
			continue
		}
		if got[0].Suggested == "" && len(got[0].Options) == 0 {
			t.Fatalf("source %q is declared in ParamSources but resolves to nothing, and is not declared as one that selects a control", source)
		}
	}
}

// A control-selecting source must still be a KNOWN one. Declaring the
// exemption by name is what keeps a typo failing: "directoy" resolves to
// nothing and is not in the control set, so it is still a bug.
func TestAnUnknownSourceStillResolvesToNothing(t *testing.T) {
	got := resolveParams([]pkgspec.ParamDecl{{Name: "p", Source: "directoy"}}, deploymentFacts{}, nil)
	if got[0].Suggested != "" || len(got[0].Options) != 0 {
		t.Fatal("a misspelled source must not resolve to anything")
	}
	if pkgspec.ParamSourcesSelectingAControl["directoy"] {
		t.Fatal("a misspelled source must not be treated as control-selecting")
	}
	if pkgspec.ParamSources["directoy"] {
		t.Fatal("a misspelled source must not be in the closed vocabulary")
	}
}
