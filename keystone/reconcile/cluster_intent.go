package reconcile

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

var clusterName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ClusterIntent checks the first stateless subset before a Plan is filed
// and constructs the same typed body used by observe and apply. Service
// ports come from the Container's bounded internal-only expose declaration.
// Accepted Routes also contribute their target port so the translation stays
// deterministic before the separate coexistence slice makes those Routes
// reachable. Docker host-port mappings would change exposure semantics and
// are refused instead of silently reinterpreted.
func ClusterIntent(want resource.Resource, all []resource.Resource) (*WorkloadSpec, error) {
	if want.Kind != resource.KindContainer || want.Container == nil || want.Container.Backend != "k3s" {
		return nil, fmt.Errorf("cluster: explicit k3s Container required")
	}
	if !clusterName.MatchString(want.Metadata.Name) || !clusterName.MatchString(want.Metadata.Package) {
		return nil, fmt.Errorf("cluster: workload and package names must be DNS labels")
	}
	// The policy is one pinned copy. The package declaration may allow more,
	// but only a future reviewed policy choice can request it.
	req, err := cluster.applyWorkload(want, pkgspec.ComponentDecl{Name: want.Metadata.Name}, WorkloadPolicy{}, nil,
		Authority{AcceptedRevision: "preflight", PlanID: "preflight", StepDigest: "preflight", LeaderEpoch: 1})
	if err != nil {
		return nil, err
	}
	ports := map[uint32]bool{}
	for _, p := range want.Container.Expose {
		ports[p] = true
	}
	for _, r := range all {
		if r.Kind != resource.KindRoute || r.Route == nil || r.Metadata.Owner != want.Metadata.Owner || r.Metadata.Package != want.Metadata.Package {
			continue
		}
		prefix := "container/" + want.Metadata.Name + ":"
		if !strings.HasPrefix(r.Route.Upstream, prefix) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(r.Route.Upstream, prefix))
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("cluster: Route/%s has an invalid workload port", r.Metadata.Name)
		}
		ports[uint32(n)] = true
	}
	values := make([]int, 0, len(ports))
	for p := range ports {
		values = append(values, int(p))
	}
	sort.Ints(values)
	for _, p := range values {
		req.Workload.ServicePorts = append(req.Workload.ServicePorts, WorkloadPort{Name: fmt.Sprintf("port-%d", p), ContainerPort: uint32(p), ServicePort: uint32(p), Protocol: "tcp"})
	}
	if _, err := wireWorkload(req.Workload); err != nil {
		return nil, err
	}
	return req.Workload, nil
}

// ClusterRouteConflict refuses a k3s opt-in that would leave an existing
// Docker Caddy Route pointing at Docker container-name DNS. The reachable
// cluster upstream is the separate coexistence networking slice (#212).
func ClusterRouteConflict(want resource.Resource, all []resource.Resource) error {
	if want.Kind != resource.KindContainer || want.Container == nil || want.Container.Backend != "k3s" {
		return nil
	}
	prefix := "container/" + want.Metadata.Name + ":"
	for _, r := range all {
		if r.Kind == resource.KindRoute && r.Route != nil && strings.HasPrefix(r.Route.Upstream, prefix) {
			return fmt.Errorf("cluster: Route/%s still uses Docker container DNS for Container/%s; routed k3s installs need the coexistence upstream from #212", r.Metadata.Name, want.Metadata.Name)
		}
	}
	return nil
}
