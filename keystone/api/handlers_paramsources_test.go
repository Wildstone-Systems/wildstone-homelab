package api

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
)

// A param the deployment can answer should arrive answered. Before the
// keystone_url source existed, every operator installing a chat adapter
// had to work out which of the host's addresses a CONTAINER can reach —
// and discover that 127.0.0.1 means the container itself.
func TestResolveParamsFillsWhatTheDeploymentKnows(t *testing.T) {
	facts := deploymentFacts{
		nodes:            []string{"n1"},
		domain:           "home.arpa",
		keystoneURL:      "http://100.64.0.59:9441",
		chatAdapterImage: "reg:5000/chat-adapter@sha256:abc",
	}
	got := resolveParams([]pkgspec.ParamDecl{
		{Name: "node", Source: "node"},
		{Name: "public_domain", Source: "domain"},
		{Name: "keystone_url", Source: "keystone_url"},
		{Name: "adapter_image", Source: "chat_adapter_image"},
		{Name: "freeform"},
	}, facts)

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
	if by["keystone_url"].Suggested != "http://100.64.0.59:9441" {
		t.Fatalf("keystone_url = %+v", by["keystone_url"])
	}
	if by["adapter_image"].Suggested != "reg:5000/chat-adapter@sha256:abc" {
		t.Fatalf("adapter_image = %+v", by["adapter_image"])
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
		{Name: "adapter_image", Source: "chat_adapter_image"},
		{Name: "public_domain", Source: "domain"},
	}, deploymentFacts{})
	for _, v := range got {
		if v.Suggested != "" {
			t.Fatalf("%s suggested %q from an empty deployment", v.Name, v.Suggested)
		}
	}
}

// Every source a package may declare has to be one resolveParams
// actually handles, or a package can name a source that silently does
// nothing.
func TestEveryDeclaredParamSourceIsResolved(t *testing.T) {
	facts := deploymentFacts{
		nodes:            []string{"n1"},
		domain:           "home.arpa",
		keystoneURL:      "http://n1:9441",
		chatAdapterImage: "reg/img@sha256:abc",
	}
	for source := range pkgspec.ParamSources {
		got := resolveParams([]pkgspec.ParamDecl{{Name: "p", Source: source}}, facts)
		if len(got) != 1 {
			t.Fatalf("source %q: resolveParams returned %d params", source, len(got))
		}
		if got[0].Suggested == "" && len(got[0].Options) == 0 {
			t.Fatalf("source %q is declared in ParamSources but resolves to nothing", source)
		}
	}
}
