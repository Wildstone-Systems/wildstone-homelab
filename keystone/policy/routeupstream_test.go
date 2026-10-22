package policy

import (
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

func hostNetContainer(name string, host bool) resource.Resource {
	c := container(name, nil)
	if host {
		c.Container.NetworkMode = resource.NetworkModeHost
	}
	return c
}

func routeTo(name, upstream string) resource.Resource {
	r := route(name, "family_private", "app_oidc")
	r.Route.Upstream = upstream
	return r
}

func upstreamViolations(vs []Violation) []Violation {
	var out []Violation
	for _, v := range vs {
		if v.Rule == RuleRouteUpstreamResolvable {
			out = append(out, v)
		}
	}
	return out
}

// TestContainerUpstreamToAHostNetworkedContainerIsRefused (#433): a
// host-networked container has no name on the Wildstone network, so
// container/<name>:<port> could never resolve. The message points at the
// form that works, with the port carried over.
func TestContainerUpstreamToAHostNetworkedContainerIsRefused(t *testing.T) {
	vs, _ := NewEngine(nil).Validate(Change{Proposed: []resource.Resource{
		hostNetContainer("home-assistant", true),
		routeTo("home", "container/home-assistant:8123"),
	}})
	got := upstreamViolations(vs)
	if len(got) != 1 || got[0].Name != "home" {
		t.Fatalf("want one %s violation on Route home, got %v", RuleRouteUpstreamResolvable, vs)
	}
	for _, want := range []string{"networkMode: host", `"host/8123"`, "home-assistant"} {
		if !strings.Contains(got[0].Msg, want) {
			t.Errorf("message %q does not say %q", got[0].Msg, want)
		}
	}
}

func TestRouteUpstreamFormsThatResolvePass(t *testing.T) {
	for name, proposed := range map[string][]resource.Resource{
		"host form to a host container":     {hostNetContainer("home-assistant", true), routeTo("home", "host/8123")},
		"container form to a bridge one":    {hostNetContainer("immich", false), routeTo("photos", "container/immich:2283")},
		"container form to an unknown name": {routeTo("x", "container/elsewhere:80")},
		"literal address":                   {hostNetContainer("home-assistant", true), routeTo("nas", "10.0.0.4:5000")},
	} {
		vs, _ := NewEngine(nil).Validate(Change{Proposed: proposed})
		if got := upstreamViolations(vs); len(got) != 0 {
			t.Errorf("%s: refused: %v", name, got)
		}
	}
}

// TestMovingAContainerToHostUnderAnUntouchedRouteIsRefused: the change
// that breaks the Route may be the Container's, not the Route's.
func TestMovingAContainerToHostUnderAnUntouchedRouteIsRefused(t *testing.T) {
	r := routeTo("home", "container/home-assistant:8123")
	current := []resource.Resource{hostNetContainer("home-assistant", false), r}
	proposed := []resource.Resource{hostNetContainer("home-assistant", true), r}
	vs, _ := NewEngine(nil).Validate(Change{Current: current, Proposed: proposed})
	if got := upstreamViolations(vs); len(got) != 1 || got[0].Name != "home" {
		t.Fatalf("want the untouched Route refused, got %v", vs)
	}
	// Neither side touched: legacy state does not wedge unrelated changes.
	legacy := []resource.Resource{hostNetContainer("home-assistant", true), r}
	vs, _ = NewEngine(nil).Validate(Change{Current: legacy, Proposed: append(append([]resource.Resource{}, legacy...), container("other", nil))})
	if got := upstreamViolations(vs); len(got) != 0 {
		t.Errorf("an unrelated change was blocked by legacy state: %v", got)
	}
}
