package reconcile

import (
	"fmt"
	"path"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// routeFragmentPath returns the managed-file path Route/<name> is rendered
// to: <managedPrefix>/routes/<name>.caddy (ADR-W004 A1's Route apply
// description). managedPrefix is expected to be one of the target agent's
// configured ManagedPrefixes.
func routeFragmentPath(managedPrefix, name string) string {
	return path.Join(managedPrefix, "routes", name+".caddy")
}

// renderRouteFragment deterministically renders a RouteSpec to a managed
// Caddy config fragment. Deterministic rendering matters here beyond
// style: the reconciler's Route observe step (route.go / reconcile.go)
// compares the live file's bytes against a fresh render of the desired
// spec to decide NoOp vs Update, so any non-determinism (e.g. map
// iteration order) would show as permanent drift.
//
// This is a minimal, self-contained fragment — not a full Caddyfile. The
// real Caddy integration (auth directives wired to Authelia, TLS, etc.)
// lands in a later E-series work package; S2b's job is the reconcile loop
// and the WriteManagedFile plumbing around it, not caddy fragment fidelity.
func renderRouteFragment(spec *resource.RouteSpec) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# managed by wildstone-keystone — do not edit\n")
	fmt.Fprintf(&b, "%s {\n", spec.Host)
	fmt.Fprintf(&b, "\treverse_proxy %s\n", spec.Upstream)
	if spec.Auth != "" && spec.Auth != "none" {
		fmt.Fprintf(&b, "\t# auth: %s\n", spec.Auth)
	}
	fmt.Fprintf(&b, "\t# classification: %s\n", spec.Classification)
	fmt.Fprintf(&b, "}\n")
	return []byte(b.String())
}
