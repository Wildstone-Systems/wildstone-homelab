package resource

import (
	"strings"
	"testing"
)

func routeWithUpstream(up string) Resource {
	return Resource{
		Kind:     KindRoute,
		Metadata: Metadata{Name: "r1", Owner: "system", Node: "node-a"},
		Route: &RouteSpec{
			Host:           "ha.example.internal",
			Upstream:       up,
			Auth:           "app_oidc",
			Classification: "family_tailscale",
		},
	}
}

func TestParseHostUpstream(t *testing.T) {
	for up, want := range map[string]int{
		"host/1":     1,
		"host/8123":  8123,
		"host/65535": 65535,
	} {
		port, isHost, err := ParseHostUpstream(up)
		if !isHost || err != nil || port != want {
			t.Errorf("ParseHostUpstream(%q) = %d, %v, %v; want %d, true, nil", up, port, isHost, err, want)
		}
	}
	for _, up := range []string{"container/x:80", "10.0.0.4:5000", "", "hostname:80", "hosts/80"} {
		if _, isHost, err := ParseHostUpstream(up); isHost || err != nil {
			t.Errorf("ParseHostUpstream(%q) claimed the host form (%v, %v)", up, isHost, err)
		}
	}
}

// TestValidateRouteHostUpstreamGrammar pins the closed grammar: every
// looser reading of "host/..." is refused rather than guessed at.
func TestValidateRouteHostUpstreamGrammar(t *testing.T) {
	for _, ok := range []string{"host/1", "host/8123", "host/65535"} {
		r := routeWithUpstream(ok)
		if err := r.Validate(); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"host/", "host/0", "host/00", "host/08123", "host/65536", "host/999999",
		"host/-1", "host/+80", "host/80/", "host/8123/api", "host/:8123",
		"host/localhost:8123", "host/127.0.0.1:8123", "host/ 80", "host/80 ",
		"HOST/8123", "Host/8123", "host/http", "host/0x50",
	} {
		r := routeWithUpstream(bad)
		err := r.Validate()
		ve, ok := err.(*ValidationError)
		if !ok || ve.Field != "spec.upstream" {
			t.Errorf("%q: want a spec.upstream validation error, got %v", bad, err)
		}
	}
}

// TestValidateRouteRefusesAmbiguousHostLiterals: the names the host form
// renders to are refused as literals, so a Route cannot reach the host
// without the alias being wired up.
func TestValidateRouteRefusesAmbiguousHostLiterals(t *testing.T) {
	for _, bad := range []string{
		"host:8123", "HOST:8123", "host.docker.internal:8123", "host.docker.internal.:8123",
		"host-gateway:8123", "host",
	} {
		r := routeWithUpstream(bad)
		err := r.Validate()
		ve, ok := err.(*ValidationError)
		if !ok || ve.Field != "spec.upstream" || !strings.Contains(ve.Msg, "host/<port>") {
			t.Errorf("%q: want a spec.upstream error pointing at host/<port>, got %v", bad, err)
		}
	}
}

// TestValidateRouteExistingUpstreamFormsUnchanged: the two forms that
// predate host/<port> validate exactly as before.
func TestValidateRouteExistingUpstreamFormsUnchanged(t *testing.T) {
	for _, ok := range []string{
		"container/immich-server:2283", "container/x", "10.0.0.4:5000", "127.0.0.1:9441",
		"192.168.1.10:9441", "nas.example.internal:5000", "hostname.example.internal:80", "",
	} {
		r := routeWithUpstream(ok)
		if err := r.Validate(); err != nil {
			t.Errorf("%q newly refused: %v", ok, err)
		}
	}
}
