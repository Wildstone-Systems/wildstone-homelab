package pkgspec

import "testing"

// RouteLabels answers "what will this service be called" before any
// param is supplied, which is when an operator is deciding whether to
// install it. Getting it wrong means the Services page tells them a
// service is unpinned when it is not, or the reverse.
func TestRouteLabelsResolveParamDefaults(t *testing.T) {
	pkg := &Package{
		Params: []ParamDecl{{Name: "media_subdomain", Default: "jellyfin"}},
		Templates: []string{
			`kind: Route
metadata: { name: media }
spec:
  host: "{{ .Params.media_subdomain }}.{{ .Params.public_domain }}"`,
		},
	}
	got := RouteLabels(pkg)
	if len(got) != 1 || got[0] != "jellyfin" {
		t.Errorf("want [jellyfin], got %v — a param reference must resolve to its default", got)
	}
}

func TestRouteLabelsReadsLiteralHosts(t *testing.T) {
	pkg := &Package{Templates: []string{`spec:
  host: "auth.{{ .Params.public_domain }}"`}}
	if got := RouteLabels(pkg); len(got) != 1 || got[0] != "auth" {
		t.Errorf("want [auth], got %v", got)
	}
}

// A package with no Routes contributes no labels rather than an empty
// string, which would otherwise look like an unpinned service.
func TestRouteLabelsIgnoresPackagesWithoutRoutes(t *testing.T) {
	if got := RouteLabels(&Package{Templates: []string{`kind: Volume`}}); len(got) != 0 {
		t.Errorf("want none, got %v", got)
	}
}
