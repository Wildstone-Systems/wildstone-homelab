package reconcile

// Route addressing, delegated to keystone/ingressorigin.
//
// This file is a thin seam on purpose. The rule it applies — where a
// Route is actually served — must be identical to the one capability
// expansion applies when it derives an OIDC issuer, and the only way to
// guarantee that is for both to call the same code. See the package
// comment on ingressorigin for why two implementations would fail
// silently rather than loudly.

import (
	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func assignTailnetPorts(desired []resource.Resource) map[string]int {
	return ingressorigin.Of(desired).Ports
}

// routeSiteAddress is the site address a Route's Caddy fragment opens
// with.
func routeSiteAddress(p ingressPosture, host string) string {
	return ingressorigin.Posture{
		TLS:         p.tls,
		TailnetName: p.tailnetName,
		Ports:       p.tailnetPorts,
	}.SiteAddress(host)
}
