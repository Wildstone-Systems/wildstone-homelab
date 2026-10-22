package reconcile

// A host-networked container's port, checked before the old one goes (#440).
//
// A container with networkMode: host (#432) binds its ports on the node
// itself, and nothing in its spec says which: `ports` is exactly what host
// mode excludes. When the port is already taken, nothing fails where
// Wildstone can see it. Docker starts the container, the app logs that it
// could not bind, and Home Assistant in particular keeps running with its
// HTTP server down — so the health gate sees a running container, the
// Route gate's probe of http://127.0.0.1:<port>/ answers from whatever
// really owns the port, and the household loses the service it had a
// moment ago, replaced by a converged-looking stranger.
//
// The port IS known, though, from the one place a package has to state it:
// its "host/<port>" Route (#433). So before a host-networked container is
// created or replaced, every host Route its own package declares on the
// same node names a port, and the node's listening TCP sockets are read
// (ListPorts, /proc/net/tcp{,6}). A port someone already listens on
// refuses the step before any pull or EnsureContainer, which leaves the
// existing container — a 1.x bridge-mode Home Assistant during an
// upgrade — running untouched.
//
// Narrow on purpose:
//
//   - Only host Routes of the same package on the same node are read, the
//     pairing homepageservices.go's routeServiceContainer already uses to
//     say which container a host Route serves.
//   - The check is skipped when the container being replaced is ALREADY
//     host-networked: the listener on that port is then most likely the
//     container itself, which the replacement stops. Telling the two apart
//     would need the socket's owning process, which the agent does not
//     report.
//   - A port published by a bridge container through iptables alone
//     (userland-proxy disabled) has no listening socket and is not seen.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// hostRoutePorts returns the ports of the "host/<port>" Routes that the
// package owning container declares on the container's node, sorted, with
// the Route that names each.
func hostRoutePorts(container resource.Resource, desiredByKey map[resKey]resource.Resource) (ports []int, routeFor map[int]string) {
	if container.Metadata.Package == "" {
		return nil, nil
	}
	routeFor = map[int]string{}
	for _, res := range desiredByKey {
		if res.Kind != resource.KindRoute || res.Route == nil ||
			res.Metadata.Package != container.Metadata.Package || res.Metadata.Node != container.Metadata.Node {
			continue
		}
		port, isHost, err := resource.ParseHostUpstream(res.Route.Upstream)
		if !isHost || err != nil {
			continue
		}
		if _, seen := routeFor[port]; !seen {
			ports = append(ports, port)
		}
		routeFor[port] = res.Metadata.Name
	}
	sort.Ints(ports)
	return ports, routeFor
}

// refuseIfHostPortTaken returns a refusal when the host-networked container
// about to be created or replaced would need a port something on the node
// already listens on; "" to go ahead.
func (r *Reconciler) refuseIfHostPortTaken(ctx context.Context, nodeID string, want resource.Resource, desiredByKey map[resKey]resource.Resource) string {
	if want.Container == nil || !want.Container.HostNetwork() {
		return ""
	}
	ports, routeFor := hostRoutePorts(want, desiredByKey)
	if len(ports) == 0 {
		return ""
	}

	// Already running on the host's network: the listener is presumably
	// itself. A stopped container cannot be the listener, so something else
	// holding its port must still be caught. Any failure to inspect (not
	// found, foreign, transport) means there is no host-networked container
	// of ours to credit it to.
	if res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{
		InspectContainer: &wsv1.InspectContainer{Name: want.Metadata.Name},
	}}); err == nil && res.GetError() == nil && res.GetInspectContainer().GetRunning() &&
		res.GetInspectContainer().GetNetworkMode() == resource.NetworkModeHost {
		return ""
	}

	res, err := r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ListPorts{ListPorts: &wsv1.ListPorts{}}})
	if err == nil && res.GetError() != nil {
		err = fmt.Errorf("%s: %s", res.GetError().GetCode(), res.GetError().GetMessage())
	}
	if err != nil {
		// Unknown is not free. Going ahead would risk exactly the silent
		// replacement this check exists to stop, and the next pass retries.
		return fmt.Sprintf("Container/%s runs with networkMode: host and needs port(s) %s on node %s, and the node's "+
			"listening ports could not be read (%v); refused before the existing container is touched",
			want.Metadata.Name, joinPorts(ports), nodeID, err)
	}
	listening := map[int][]string{}
	for _, p := range res.GetListPorts().GetPorts() {
		listening[int(p.GetPort())] = append(listening[int(p.GetPort())], p.GetLocalIp())
	}
	var taken []string
	for _, port := range ports {
		if addrs, ok := listening[port]; ok {
			sort.Strings(addrs)
			taken = append(taken, fmt.Sprintf("%d (Route %s; already listening on %s)", port, routeFor[port], strings.Join(addrs, ", ")))
		}
	}
	if len(taken) == 0 {
		return ""
	}
	return fmt.Sprintf("Container/%s runs with networkMode: host and binds its ports on node %s itself, but something "+
		"there already listens on %s. Refused before the existing container is touched: started anyway it would run "+
		"without its port while the other process answered in its place. Stop whatever holds the port on %s, or "+
		"install on another machine",
		want.Metadata.Name, nodeID, strings.Join(taken, "; "), nodeID)
}

func joinPorts(ports []int) string {
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = fmt.Sprint(p)
	}
	return strings.Join(s, ", ")
}
