package research

import (
	"fmt"
	"strconv"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

const ManagedPackage = "research-ddgs"
const ManagedContainer = "ddgs"
const ManagedEndpointRef = "service/ddgs"

// ManagedReleaseImage is deliberately empty until a real worker artifact has
// passed release verification. A shape-only fixture or Owner-supplied digest
// must not silently become the supported production release.
const ManagedReleaseImage = ""

// ManagedEndpoint resolves only the singleton package's desired endpoint on
// Keystone's own node. This is configuration, not proof of reconciliation or
// upstream readiness, and does not grant authority to call it.
func ManagedEndpoint(current []resource.Resource, node, ref string) (string, error) {
	unavailable := func() (string, error) {
		return "", fmt.Errorf("provider_unavailable: install research-ddgs on Keystone's node")
	}
	if node == "" || ref != ManagedEndpointRef {
		return unavailable()
	}
	var endpoint string
	for _, r := range current {
		if r.Kind != resource.KindContainer || r.Metadata.Name != ManagedContainer {
			continue
		}
		c := r.Container
		if endpoint != "" || c == nil || r.Metadata.Package != ManagedPackage || r.Metadata.Node != node || len(c.Ports) != 1 {
			return unavailable()
		}
		p, ok := resource.ParsePortMapping(c.Ports[0])
		if !ok || p.HostIP != "127.0.0.1" || p.HostPort == 0 || p.ContainerPort != 8080 || p.Protocol != "tcp" {
			return unavailable()
		}
		if err := r.Validate(); err != nil {
			return unavailable()
		}
		endpoint = "http://127.0.0.1:" + strconv.FormatUint(uint64(p.HostPort), 10)
	}
	if endpoint == "" {
		return unavailable()
	}
	return endpoint, nil
}
