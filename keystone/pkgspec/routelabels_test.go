package pkgspec

import "testing"

// RouteLabels and ContainerNames read the template AS YAML. The first
// attempt used regexes and understood only the shipped catalog's own
// formatting, which meant a package written in ordinary YAML was
// silently skipped — no label, so no tailnet blocker; no container, so
// every profile of a capability was attributed the installed one's
// resources.

func TestRouteLabelsResolveParamDefaults(t *testing.T) {
	pkg := &Package{
		Params: []ParamDecl{{Name: "media_subdomain", Default: "jellyfin"}},
		Templates: []string{`apiVersion: wildstone.dev/v1
kind: Route
metadata: { name: media }
spec:
  host: "{{ .Params.media_subdomain }}.{{ .Params.public_domain }}"
`},
	}
	if got := RouteLabels(pkg); len(got) != 1 || got[0] != "jellyfin" {
		t.Errorf("want [jellyfin], got %v — a param reference must resolve to its default", got)
	}
}

// The reviewer's case: an unquoted host is valid YAML and was invisible.
func TestRouteLabelsAcceptsUnquotedHosts(t *testing.T) {
	pkg := &Package{Templates: []string{`kind: Route
metadata: { name: app }
spec:
  host: app.example.com
`}}
	if got := RouteLabels(pkg); len(got) != 1 || got[0] != "app" {
		t.Errorf("want [app], got %v — an unquoted host is valid YAML", got)
	}
}

func TestRouteLabelsAcceptsSingleQuotedHosts(t *testing.T) {
	pkg := &Package{Templates: []string{`kind: Route
metadata: { name: app }
spec:
  host: 'app.example.com'
`}}
	if got := RouteLabels(pkg); len(got) != 1 || got[0] != "app" {
		t.Errorf("want [app], got %v — single quotes are valid YAML", got)
	}
}

func TestRouteLabelsIgnoresNonRouteDocuments(t *testing.T) {
	pkg := &Package{Templates: []string{`kind: Volume
metadata: { name: data }
spec: { name: data }
`}}
	if got := RouteLabels(pkg); len(got) != 0 {
		t.Errorf("want none, got %v", got)
	}
}

// The other reviewer case: conventional block metadata was invisible, so
// the package looked containerless and therefore installed.
func TestContainerNamesAcceptsBlockMetadata(t *testing.T) {
	pkg := &Package{Templates: []string{`kind: Container
metadata:
  name: caddy-tailnet-ts
  node: n1
spec:
  image: "example@sha256:abc"
`}}
	if got := ContainerNames(pkg); len(got) != 1 || got[0] != "caddy-tailnet-ts" {
		t.Errorf("want [caddy-tailnet-ts], got %v — block metadata is the conventional style", got)
	}
}

func TestContainerNamesAcceptsInlineMetadata(t *testing.T) {
	pkg := &Package{Templates: []string{`kind: Container
metadata: { name: homepage, node: n1 }
spec: { image: "example@sha256:abc" }
`}}
	if got := ContainerNames(pkg); len(got) != 1 || got[0] != "homepage" {
		t.Errorf("want [homepage], got %v", got)
	}
}

// A template that cannot parse yields nothing rather than a wrong
// answer — silence is recoverable, a confident wrong label is not.
func TestUnparsableTemplateYieldsNothing(t *testing.T) {
	pkg := &Package{Templates: []string{`{{ if .Resolved.X }}
kind: Route
{{ end }}
  : : not yaml
`}}
	if got := RouteLabels(pkg); len(got) != 0 {
		t.Errorf("want none from an unparsable template, got %v", got)
	}
}
