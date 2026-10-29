package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// dockerTranslator turns the reconciler's decisions into Docker-shaped
// Wardstone operations: containers, images, volumes and the managed
// network (W050 slice 2, #158). It is the only place in this package that
// builds one; TestDockerOperationsStayInTheTranslator holds that line.
//
// What it does not hold: deciding what changes, its risk and its approval
// (plan, Reconcile), host work every backend needs (managed files, mounts,
// probes, secrets) and the order steps run in. A cluster backend is a
// second translator beside this one, so none of that has to move.
type dockerTranslator struct{}

// docker is the reconciler's one translator; there is no other backend.
var docker dockerTranslator

func (dockerTranslator) inspectContainer(name string) *wsv1.Operation {
	return &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{InspectContainer: &wsv1.InspectContainer{Name: name}}}
}

func (dockerTranslator) containerStats(name string) *wsv1.Operation {
	return &wsv1.Operation{Op: &wsv1.Operation_ContainerStats{ContainerStats: &wsv1.ContainerStats{Name: name}}}
}

func (dockerTranslator) stopContainer(name string) *wsv1.Operation {
	return &wsv1.Operation{Op: &wsv1.Operation_StopContainer{StopContainer: &wsv1.StopContainer{Name: name}}}
}

func (dockerTranslator) removeContainer(name string, force bool) *wsv1.Operation {
	return &wsv1.Operation{Op: &wsv1.Operation_RemoveContainer{RemoveContainer: &wsv1.RemoveContainer{Name: name, Force: force}}}
}

func (dockerTranslator) pullImage(image string) *wsv1.Operation {
	return &wsv1.Operation{Op: &wsv1.Operation_PullImage{PullImage: &wsv1.PullImage{Image: image}}}
}

func (dockerTranslator) ensureVolume(name string) *wsv1.Operation {
	return &wsv1.Operation{Op: &wsv1.Operation_EnsureVolume{EnsureVolume: &wsv1.EnsureVolume{Name: name}}}
}

func (dockerTranslator) ensureNetwork(name string) *wsv1.Operation {
	return &wsv1.Operation{Op: &wsv1.Operation_EnsureDockerNetwork{EnsureDockerNetwork: &wsv1.EnsureDockerNetwork{Name: name}}}
}

func (dockerTranslator) statVolumeFile(volume, path string) *wsv1.Operation {
	return &wsv1.Operation{Op: &wsv1.Operation_StatVolumeFile{StatVolumeFile: &wsv1.StatVolumeFile{Volume: volume, Path: path}}}
}

func (dockerTranslator) seedVolumeFile(volume, path string, content []byte) *wsv1.Operation {
	return &wsv1.Operation{Op: &wsv1.Operation_SeedVolumeFile{SeedVolumeFile: &wsv1.SeedVolumeFile{Volume: volume, Path: path, Content: content}}}
}

func (dockerTranslator) ensureContainer(name string, spec *resource.ContainerSpec, network, cfgDigest string, aliases []string) *wsv1.Operation {
	var ports []*wsv1.PortBinding
	for _, p := range spec.Ports {
		m, ok := resource.ParsePortMapping(p)
		if !ok {
			continue // malformed mapping; not this package's job to re-validate
		}
		// TCP is spelled explicitly rather than left UNSPECIFIED. Both
		// mean TCP to the agent; the explicit value is what makes a
		// spec that SAYS tcp distinguishable on the wire from one that
		// predates the field (protocol minor 12).
		proto := wsv1.PortProtocol_PORT_PROTOCOL_TCP
		if m.Protocol == "udp" {
			proto = wsv1.PortProtocol_PORT_PROTOCOL_UDP
		}
		ports = append(ports, &wsv1.PortBinding{HostIp: m.HostIP, HostPort: m.HostPort, ContainerPort: m.ContainerPort, Protocol: proto})
	}
	var mounts []*wsv1.Mount
	for _, m := range spec.Mounts {
		mounts = append(mounts, &wsv1.Mount{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	var devices []*wsv1.DeviceMapping
	for _, d := range spec.Devices {
		devices = append(devices, &wsv1.DeviceMapping{HostPath: d.Source, ContainerPath: d.Target})
	}
	// H1 hardening fields (protocol minor 18): a pre-minor-18 agent
	// ignores them, which is exactly why refuseIfAgentTooOld stops a spec
	// that sets any of them from ever reaching one.
	var resources *wsv1.ContainerResources
	if r := spec.Resources; r != nil {
		resources = &wsv1.ContainerResources{Memory: r.Memory, Cpus: r.Cpus, PidsLimit: int64(r.PidsLimit)}
	}
	var runtimeIdentity *wsv1.ContainerRuntimeIdentity
	if id := spec.RuntimeIdentity; id != nil {
		runtimeIdentity = &wsv1.ContainerRuntimeIdentity{Uid: *id.UID, Gid: id.GID}
	}
	// Host networking (protocol minor 23): refuseIfAgentTooOld keeps a
	// host-mode spec away from an agent that would ignore this and start
	// the container on the bridge instead.
	networkMode := wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_UNSPECIFIED
	if spec.HostNetwork() {
		networkMode = wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST
	}
	return &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{EnsureContainer: &wsv1.EnsureContainer{
		Name: name, Image: spec.Image, Env: spec.Env, Ports: ports, Mounts: mounts, RestartPolicy: spec.Restart,
		Command:         spec.Command,
		Network:         network,
		Devices:         devices,
		CapAdd:          spec.CapAdd,
		HostAliases:     aliases,
		ReadOnlyRootfs:  spec.ReadOnlyRootfs,
		CapDrop:         spec.CapDrop,
		NoNewPrivileges: spec.NoNewPrivileges,
		Resources:       resources,
		ShmSize:         spec.ShmSize,
		RuntimeIdentity: runtimeIdentity,
		NetworkMode:     networkMode,
		Labels:          map[string]string{specHashLabel: specHash(spec, network, cfgDigest, aliases)},
	}}}
}

// specHash covers the network as well as the spec so that attaching an
// existing fleet to ContainerNetwork for the first time actually replaces
// those containers. The network is not part of ContainerSpec, so hashing
// the spec alone would leave every pre-minor-8 container running
// unattached with a matching hash — converged on paper, unreachable by
// name in fact.
//
// cfgDigest extends that reasoning to the managed configuration files a
// container mounts, which are not part of ContainerSpec either: without
// it, editing an app's config leaves the container running the bytes it
// read at start, with a matching hash. See configdigest.go for what is
// included and what is deliberately not.
//
// An empty cfgDigest contributes NOTHING to the hash, rather than
// hashing the empty string: that keeps every container which reads no
// managed config on the hash it already has, so this dimension replaces
// only the containers it actually applies to.
func specHash(spec *resource.ContainerSpec, network, cfgDigest string, aliases []string) string {
	data, err := json.Marshal(spec)
	if err != nil {
		// A ContainerSpec is plain data; Marshal cannot fail on it. Guard
		// anyway: an empty hash never matches, which degrades to "always
		// replace" — converging, never wedging.
		return ""
	}
	data = append(data, []byte("\x00net="+network)...)
	// Earlier agents sent PortBindings without Config.ExposedPorts. Docker
	// accepted those containers but published none of their host ports.
	// Change the hash only for port-bearing containers so an upgrade
	// replaces those broken instances with correctly published ones.
	if len(spec.Ports) > 0 {
		data = append(data, []byte("\x00port-publishing=exposed-ports-v1")...)
	}
	if cfgDigest != "" {
		data = append(data, []byte("\x00cfg="+cfgDigest)...)
	}
	// Host aliases are part of a container's effective configuration: one
	// that cannot resolve its own issuer is not the same container as one
	// that can. Folding them in means a posture change recreates the
	// containers that depend on it, rather than leaving them with a
	// stale /etc/hosts nobody can see.
	for _, a := range aliases {
		data = append(data, []byte("\x00alias="+a)...)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}
