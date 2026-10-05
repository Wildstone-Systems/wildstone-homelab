// Package fakenode is an in-memory fleet of Wardstone agents for
// keystone's end-to-end tests (#452): the reconciler, the reconcile loop
// and the API's probes all dispatch to it through the same
// (ctx, node, *wsv1.Operation) seam the gateway serves in production
// (reconcile.Applier), so a test can drive a deployment from an empty
// Statefile to a running stack, then restart, disconnect and break it,
// with no Docker daemon, no radios and no network.
//
// It models the agent's CONTRACT rather than Docker: what each op
// answers, which rules the real agent enforces on its side
// (wardstone/internal/agent/ops.go) and which protocol minor introduced
// which op. The rules are the ones a fake that ignored them would let a
// broken plan pass:
//
//   - images must be digest-pinned (repo@sha256:...), or FORBIDDEN;
//   - host network mode excludes published ports and a bridge network;
//   - a mapped device must exist on the node: a by-id alias or a devPath
//     from the node's radio inventory, or one of the node's always-present
//     character devices (/dev/net/tun), or NOT_FOUND before the old
//     container is touched;
//   - a secret/<name> reference, in env or in a materialized file, must
//     name a secret the node holds, or NOT_FOUND;
//   - an op newer than the node's protocol minor is answered the way an
//     older agent answers it: INVALID "unknown operation".
//
// A disconnected node fails every op with the gateway's own "is not
// connected" error, and reports itself absent from LiveNodes, so the loop
// defers rather than fails and the API reports node_offline.
//
// Nothing here is for production; it is a non-test package only so tests
// in several keystone packages can share it (the internal/fakesecret
// precedent).
package fakenode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// SpecHashLabel mirrors the agent's (and the reconciler's) spec-hash label:
// an ensure whose label matches the running container's is a no-op.
const SpecHashLabel = "dev.wildstone.spec-hash"

// opMinor is the protocol minor that introduced each op the fleet serves
// and an older agent would not know (wardstone/internal/agent/agent.go's
// ProtocolMinor history). Ops absent here predate every minor a test
// would model.
var opMinor = map[string]uint32{
	"EnsureSecret":            7,
	"PutSecret":               7,
	"ListSecrets":             7,
	"DiskSpace":               19,
	"ListRadios":              25,
	"HomeAutomationReadiness": 26,
}

// alwaysPresentDevices are character devices every modelled node has.
var alwaysPresentDevices = map[string]bool{"/dev/net/tun": true}

var (
	digestRef = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)
	secretRef = regexp.MustCompile(`(?:pbkdf2:)?secret/([A-Za-z0-9][A-Za-z0-9._-]{0,63})`)
)

// Container is one container as the node holds it.
type Container struct {
	Spec    *wsv1.EnsureContainer
	Running bool
	// Status/ExitCode are Docker's State.Status and exit code; Status is
	// "running" or "exited".
	Status   string
	ExitCode int32
	// AppHealth is the image healthcheck's verdict ("" when none).
	AppHealth string
	// Created counts how many times this name was (re)created.
	Created int
}

// Node is one modelled machine. Fields are the node's facts; set them
// before the first op or under Fleet.Do.
type Node struct {
	ID            string
	ProtocolMinor uint32
	Connected     bool

	// What the read-only probes report (#434, #436).
	Interfaces []*wsv1.NodeInterface
	Checks     []*wsv1.ReadinessCheck
	Radios     []*wsv1.Radio
	Listening  []*wsv1.ListeningPort

	// Starts decides whether a container's process succeeds when created
	// or started, and with which app health: the hook a test uses to
	// model an image that refuses to run on a node missing a
	// prerequisite. nil succeeds everything, with no app health.
	Starts func(n *Node, spec *wsv1.EnsureContainer) (running bool, appHealth string)

	Containers  map[string]*Container
	Volumes     map[string]bool
	VolumeFiles map[string]map[string][]byte
	Secrets     map[string]uint32
	Files       map[string][]byte
	// fileSource is the pre-materialization sha256 of a materialized
	// managed file; such a file is read back as its hash only.
	fileSource map[string]string
}

// Fleet is every modelled node, and the log of ops dispatched to them.
type Fleet struct {
	mu    sync.Mutex
	nodes map[string]*Node
	log   []Op
}

// Op is one dispatched operation, in dispatch order.
type Op struct {
	Node string
	Kind string
	// Name is the op's subject where it has one (a container, volume,
	// secret or file path).
	Name string
	// Created is set on an EnsureContainer that created or replaced.
	Created bool
	// Changed is set when the op changed the node: a container created,
	// replaced, started, stopped or removed, a volume or secret created
	// (or a secret rotated or put), a managed file written with new
	// content, a volume file seeded. An idempotent re-assertion of what
	// is already there is not a change.
	Changed bool
	// Err is the OpError message, or the transport error, if any.
	Err string
}

// New returns an empty fleet.
func New() *Fleet { return &Fleet{nodes: map[string]*Node{}} }

// Add registers a connected node speaking minor.
func (f *Fleet) Add(id string, minor uint32) *Node {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := &Node{ID: id, ProtocolMinor: minor, Connected: true,
		Containers: map[string]*Container{}, Volumes: map[string]bool{}, VolumeFiles: map[string]map[string][]byte{},
		Secrets: map[string]uint32{}, Files: map[string][]byte{}, fileSource: map[string]string{}}
	f.nodes[id] = n
	return n
}

// Do runs fn with the fleet locked: the way to change a node's facts or
// read its state while a loop may be dispatching.
func (f *Fleet) Do(id string, fn func(n *Node)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := f.nodes[id]; n != nil {
		fn(n)
	}
}

// Connected reports whether the node has a live session (loop.Loop's
// Connected).
func (f *Fleet) Connected(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nodes[id]
	return n != nil && n.Connected
}

// ProtocolMinor answers what the node's Hello said, false when it is not
// connected (reconcile.Reconciler's AgentProtocolMinor).
func (f *Fleet) ProtocolMinor(id string) (uint32, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nodes[id]
	if n == nil || !n.Connected {
		return 0, false
	}
	return n.ProtocolMinor, true
}

// Live lists the connected nodes and their minors, sorted.
func (f *Fleet) Live() map[string]uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]uint32{}
	for id, n := range f.nodes {
		if n.Connected {
			out[id] = n.ProtocolMinor
		}
	}
	return out
}

// SetConnected drops or restores a node's session. Containers keep
// running either way: an agent disconnect is not a host failure.
func (f *Fleet) SetConnected(id string, up bool) {
	f.Do(id, func(n *Node) { n.Connected = up })
}

// Reboot models the host restarting: every container with a restart
// policy comes back (Docker's unless-stopped), anything else stays down;
// volumes, secrets and managed files survive. The agent reconnects.
func (f *Fleet) Reboot(id string) {
	f.Do(id, func(n *Node) {
		for _, c := range n.Containers {
			if p := c.Spec.GetRestartPolicy(); p == "unless-stopped" || p == "always" || p == "" {
				n.run(c)
			}
		}
		n.Connected = true
	})
}

// Crash stops one container out of band, as a crashed process the
// restart policy gave up on would leave it.
func (f *Fleet) Crash(id, name string) {
	f.Do(id, func(n *Node) {
		if c := n.Containers[name]; c != nil {
			c.Running, c.Status, c.ExitCode = false, "exited", 1
		}
	})
}

// Log returns a copy of every op dispatched so far.
func (f *Fleet) Log() []Op {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Op(nil), f.log...)
}

// Mark returns the current log position, for Since.
func (f *Fleet) Mark() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.log)
}

// Since returns the ops dispatched after mark.
func (f *Fleet) Since(mark int) []Op {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Op(nil), f.log[mark:]...)
}

// Container returns a copy of the named container's state.
func (f *Fleet) Container(node, name string) (Container, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nodes[node]
	if n == nil || n.Containers[name] == nil {
		return Container{}, false
	}
	return *n.Containers[name], true
}

// ContainerNames lists a node's containers, sorted.
func (f *Fleet) ContainerNames(node string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	if n := f.nodes[node]; n != nil {
		for name := range n.Containers {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// run starts c's process. A service keeps running; a one-shot (restart
// policy "no", which is how the reconciler runs a Job) runs to completion
// and exits. Starts decides whether the process succeeds: a failing
// service exits 1, as a crash loop the restart policy gave up on would
// leave it, and so does a failing one-shot.
func (n *Node) run(c *Container) {
	ok, health := true, ""
	if n.Starts != nil {
		ok, health = n.Starts(n, c.Spec)
	}
	oneShot := c.Spec.GetRestartPolicy() == "no"
	c.Running, c.AppHealth, c.Status, c.ExitCode = ok && !oneShot, health, "exited", 0
	if c.Running {
		c.Status = "running"
	}
	if !ok {
		c.ExitCode = 1
	}
}

// HasDevice reports whether path is present on the node.
func (n *Node) HasDevice(path string) bool {
	if alwaysPresentDevices[path] {
		return true
	}
	for _, r := range n.Radios {
		if r.GetDevPath() == path {
			return true
		}
		for _, p := range r.GetByIdPaths() {
			if p == path {
				return true
			}
		}
	}
	return false
}

// Check returns the node's readiness check of that name and subject
// ("" matches any subject), or nil.
func (n *Node) Check(name, subject string) *wsv1.ReadinessCheck {
	for _, c := range n.Checks {
		if c.GetCheck() == name && (subject == "" || c.GetSubject() == subject) {
			return c
		}
	}
	return nil
}

// Apply dispatches one op (reconcile.Applier).
func (f *Fleet) Apply(_ context.Context, nodeID string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kind, name := describe(op)
	rec := Op{Node: nodeID, Kind: kind, Name: name}
	n := f.nodes[nodeID]
	if n == nil || !n.Connected {
		err := fmt.Errorf("node %q is not connected", nodeID)
		rec.Err = err.Error()
		f.log = append(f.log, rec)
		return nil, err
	}
	res := n.handle(op, kind, &rec)
	if e := res.GetError(); e != nil {
		rec.Err = e.GetMessage()
	}
	f.log = append(f.log, rec)
	return res, nil
}

func opErr(code wsv1.ErrorCode, format string, args ...any) *wsv1.OpResult {
	return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{Code: code, Message: fmt.Sprintf(format, args...)}}}
}

// describe names an op and its subject for the log.
func describe(op *wsv1.Operation) (string, string) {
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_EnsureContainer:
		return "EnsureContainer", o.EnsureContainer.GetName()
	case *wsv1.Operation_StopContainer:
		return "StopContainer", o.StopContainer.GetName()
	case *wsv1.Operation_RemoveContainer:
		return "RemoveContainer", o.RemoveContainer.GetName()
	case *wsv1.Operation_ContainerStats:
		return "ContainerStats", o.ContainerStats.GetName()
	case *wsv1.Operation_InspectContainer:
		return "InspectContainer", o.InspectContainer.GetName()
	case *wsv1.Operation_ContainerLogs:
		return "ContainerLogs", o.ContainerLogs.GetName()
	case *wsv1.Operation_EnsureVolume:
		return "EnsureVolume", o.EnsureVolume.GetName()
	case *wsv1.Operation_EnsureSecret:
		return "EnsureSecret", o.EnsureSecret.GetName()
	case *wsv1.Operation_PutSecret:
		return "PutSecret", o.PutSecret.GetName()
	case *wsv1.Operation_WriteManagedFile:
		return "WriteManagedFile", o.WriteManagedFile.GetPath()
	case *wsv1.Operation_ReadManagedFile:
		return "ReadManagedFile", o.ReadManagedFile.GetPath()
	case *wsv1.Operation_SeedVolumeFile:
		return "SeedVolumeFile", o.SeedVolumeFile.GetVolume() + "/" + o.SeedVolumeFile.GetPath()
	case *wsv1.Operation_StatVolumeFile:
		return "StatVolumeFile", o.StatVolumeFile.GetVolume() + "/" + o.StatVolumeFile.GetPath()
	case *wsv1.Operation_PullImage:
		return "PullImage", o.PullImage.GetImage()
	case *wsv1.Operation_ProbeHttp:
		return "ProbeHttp", o.ProbeHttp.GetUrl()
	case *wsv1.Operation_ProbeTcp:
		return "ProbeTcp", fmt.Sprintf("%s:%d", o.ProbeTcp.GetHost(), o.ProbeTcp.GetPort())
	case *wsv1.Operation_EnsureDockerNetwork:
		return "EnsureDockerNetwork", o.EnsureDockerNetwork.GetName()
	case *wsv1.Operation_ListSecrets:
		return "ListSecrets", ""
	case *wsv1.Operation_ListPorts:
		return "ListPorts", ""
	case *wsv1.Operation_ListRadios:
		return "ListRadios", ""
	case *wsv1.Operation_HomeAutomationReadiness:
		return "HomeAutomationReadiness", ""
	case *wsv1.Operation_Health:
		return "Health", ""
	case *wsv1.Operation_HostFacts:
		return "HostFacts", ""
	case *wsv1.Operation_DiskSpace:
		return "DiskSpace", o.DiskSpace.GetPath()
	}
	return fmt.Sprintf("%T", op.GetOp()), ""
}

func (n *Node) handle(op *wsv1.Operation, kind string, rec *Op) *wsv1.OpResult {
	if need, ok := opMinor[kind]; ok && n.ProtocolMinor < need {
		return opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "unknown operation — agent too old for this Keystone? (protocol skew)")
	}
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_EnsureContainer:
		return n.ensureContainer(o.EnsureContainer, rec)
	case *wsv1.Operation_StopContainer:
		c := n.Containers[o.StopContainer.GetName()]
		if c == nil {
			return opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "container %q", o.StopContainer.GetName())
		}
		was := c.Running
		c.Running, c.Status = false, "exited"
		rec.Changed = was
		return &wsv1.OpResult{Result: &wsv1.OpResult_StopContainer{StopContainer: &wsv1.StopContainerResult{WasRunning: was}}}
	case *wsv1.Operation_RemoveContainer:
		_, existed := n.Containers[o.RemoveContainer.GetName()]
		delete(n.Containers, o.RemoveContainer.GetName())
		rec.Changed = existed
		return &wsv1.OpResult{Result: &wsv1.OpResult_RemoveContainer{RemoveContainer: &wsv1.RemoveContainerResult{Existed: existed}}}
	case *wsv1.Operation_ContainerStats:
		c := n.Containers[o.ContainerStats.GetName()]
		if c == nil || !c.Running {
			return opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "container %q is not running", o.ContainerStats.GetName())
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_ContainerStats{ContainerStats: &wsv1.ContainerStatsResult{CpuUsageNs: 1, MemUsageBytes: 1, MemLimitBytes: 1}}}
	case *wsv1.Operation_InspectContainer:
		c := n.Containers[o.InspectContainer.GetName()]
		if c == nil {
			return opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "container %q", o.InspectContainer.GetName())
		}
		mode := c.Spec.GetNetwork()
		if c.Spec.GetNetworkMode() == wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST {
			mode = "host"
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_InspectContainer{InspectContainer: &wsv1.InspectContainerResult{
			Running: c.Running, Image: c.Spec.GetImage(), Labels: c.Spec.GetLabels(), Status: c.Status,
			ExitCode: c.ExitCode, AppHealth: c.AppHealth, NetworkMode: mode}}}
	case *wsv1.Operation_ContainerLogs:
		return &wsv1.OpResult{Result: &wsv1.OpResult_ContainerLogs{ContainerLogs: &wsv1.ContainerLogsResult{}}}
	case *wsv1.Operation_EnsureVolume:
		existed := n.Volumes[o.EnsureVolume.GetName()]
		n.Volumes[o.EnsureVolume.GetName()] = true
		rec.Changed = !existed
		return &wsv1.OpResult{Result: &wsv1.OpResult_EnsureVolume{EnsureVolume: &wsv1.EnsureVolumeResult{Created: !existed}}}
	case *wsv1.Operation_EnsureSecret:
		name := o.EnsureSecret.GetName()
		v, existed := n.Secrets[name]
		if !existed || o.EnsureSecret.GetRotate() {
			v++
			n.Secrets[name] = v
			rec.Changed = true
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_EnsureSecret{EnsureSecret: &wsv1.EnsureSecretResult{Existed: existed, Version: v}}}
	case *wsv1.Operation_PutSecret:
		n.Secrets[o.PutSecret.GetName()]++
		rec.Changed = true
		return &wsv1.OpResult{Result: &wsv1.OpResult_PutSecret{PutSecret: &wsv1.PutSecretResult{Version: n.Secrets[o.PutSecret.GetName()]}}}
	case *wsv1.Operation_ListSecrets:
		var infos []*wsv1.SecretInfo
		for name, v := range n.Secrets {
			infos = append(infos, &wsv1.SecretInfo{Name: name, Version: v})
		}
		sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
		return &wsv1.OpResult{Result: &wsv1.OpResult_ListSecrets{ListSecrets: &wsv1.ListSecretsResult{Secrets: infos}}}
	case *wsv1.Operation_WriteManagedFile:
		return n.writeManagedFile(o.WriteManagedFile, rec)
	case *wsv1.Operation_ReadManagedFile:
		p := o.ReadManagedFile.GetPath()
		content, ok := n.Files[p]
		if !ok {
			return opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "%s", p)
		}
		if src := n.fileSource[p]; src != "" {
			return &wsv1.OpResult{Result: &wsv1.OpResult_ReadManagedFile{ReadManagedFile: &wsv1.ReadManagedFileResult{SourceSha256Hex: src}}}
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_ReadManagedFile{ReadManagedFile: &wsv1.ReadManagedFileResult{Content: content}}}
	case *wsv1.Operation_SeedVolumeFile:
		s := o.SeedVolumeFile
		if !n.Volumes[s.GetVolume()] {
			return opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "volume %q does not exist", s.GetVolume())
		}
		files := n.VolumeFiles[s.GetVolume()]
		if files == nil {
			files = map[string][]byte{}
			n.VolumeFiles[s.GetVolume()] = files
		}
		if _, ok := files[s.GetPath()]; ok {
			return &wsv1.OpResult{Result: &wsv1.OpResult_SeedVolumeFile{SeedVolumeFile: &wsv1.SeedVolumeFileResult{Written: false}}}
		}
		files[s.GetPath()] = append([]byte(nil), s.GetContent()...)
		rec.Changed = true
		return &wsv1.OpResult{Result: &wsv1.OpResult_SeedVolumeFile{SeedVolumeFile: &wsv1.SeedVolumeFileResult{Written: true}}}
	case *wsv1.Operation_StatVolumeFile:
		_, ok := n.VolumeFiles[o.StatVolumeFile.GetVolume()][o.StatVolumeFile.GetPath()]
		return &wsv1.OpResult{Result: &wsv1.OpResult_StatVolumeFile{StatVolumeFile: &wsv1.StatVolumeFileResult{Exists: ok}}}
	case *wsv1.Operation_PullImage:
		if !digestRef.MatchString(o.PullImage.GetImage()) {
			return opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN, "image %q is not digest-pinned (repo@sha256:... required)", o.PullImage.GetImage())
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_PullImage{PullImage: &wsv1.PullImageResult{Digest: o.PullImage.GetImage()}}}
	case *wsv1.Operation_ProbeHttp:
		// The modelled host answers on every port it is asked about: what
		// is under test is whether the probe is asked, not the network.
		return &wsv1.OpResult{Result: &wsv1.OpResult_ProbeHttp{ProbeHttp: &wsv1.ProbeHttpResult{Reachable: true, StatusCode: 200}}}
	case *wsv1.Operation_ProbeTcp:
		return &wsv1.OpResult{Result: &wsv1.OpResult_ProbeTcp{ProbeTcp: &wsv1.ProbeTcpResult{Reachable: true}}}
	case *wsv1.Operation_EnsureDockerNetwork:
		return &wsv1.OpResult{Result: &wsv1.OpResult_EnsureDockerNetwork{EnsureDockerNetwork: &wsv1.EnsureDockerNetworkResult{}}}
	case *wsv1.Operation_ListPorts:
		return &wsv1.OpResult{Result: &wsv1.OpResult_ListPorts{ListPorts: &wsv1.ListPortsResult{Ports: n.Listening}}}
	case *wsv1.Operation_ListRadios:
		return &wsv1.OpResult{Result: &wsv1.OpResult_ListRadios{ListRadios: &wsv1.ListRadiosResult{PlatformSupported: true, Radios: n.Radios}}}
	case *wsv1.Operation_HomeAutomationReadiness:
		return &wsv1.OpResult{Result: &wsv1.OpResult_HomeAutomationReadiness{HomeAutomationReadiness: &wsv1.HomeAutomationReadinessResult{
			PlatformSupported: true, Interfaces: n.Interfaces, Checks: n.Checks}}}
	case *wsv1.Operation_Health:
		return &wsv1.OpResult{Result: &wsv1.OpResult_Health{Health: &wsv1.HealthResult{Ok: true, Detail: "fakenode"}}}
	case *wsv1.Operation_HostFacts:
		return &wsv1.OpResult{Result: &wsv1.OpResult_HostFacts{HostFacts: &wsv1.HostFactsResult{Hostname: n.ID, Os: "linux", NumCpu: 4,
			MemTotalBytes: 8 << 30, RootDiskTotalBytes: 64 << 30, RootDiskFreeBytes: 32 << 30, AgentVersion: "fakenode"}}}
	case *wsv1.Operation_DiskSpace:
		return &wsv1.OpResult{Result: &wsv1.OpResult_DiskSpace{DiskSpace: &wsv1.DiskSpaceResult{TotalBytes: 64 << 30, FreeBytes: 32 << 30,
			MountPoint: "/", Fstype: "ext4", Exists: true}}}
	}
	return opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "unknown operation — agent too old for this Keystone? (protocol skew)")
}

func (n *Node) ensureContainer(op *wsv1.EnsureContainer, rec *Op) *wsv1.OpResult {
	// Every refusal comes before the existing container is touched, as
	// the real agent's do.
	if !digestRef.MatchString(op.GetImage()) {
		return opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN, "image %q is not digest-pinned (repo@sha256:... required)", op.GetImage())
	}
	if op.GetNetworkMode() == wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST && (len(op.GetPorts()) > 0 || op.GetNetwork() != "") {
		return opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "host network mode excludes ports and network")
	}
	for _, d := range op.GetDevices() {
		if !n.HasDevice(d.GetHostPath()) {
			return opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "device %q does not exist on this node", d.GetHostPath())
		}
	}
	for _, m := range op.GetMounts() {
		if m.GetSource() == "/run/dbus" && !m.GetReadOnly() {
			return opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN, "/run/dbus may only be mounted read-only")
		}
	}
	keys := make([]string, 0, len(op.GetEnv()))
	for k := range op.GetEnv() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, m := range secretRef.FindAllStringSubmatch(op.GetEnv()[k], -1) {
			if _, ok := n.Secrets[m[1]]; !ok {
				return opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "env %s references secret/%s, which does not exist on this node", k, m[1])
			}
		}
	}
	name := op.GetName()
	if c := n.Containers[name]; c != nil {
		if want, ok := op.GetLabels()[SpecHashLabel]; ok && c.Spec.GetLabels()[SpecHashLabel] == want {
			started := !c.Running && c.Spec.GetRestartPolicy() != "no"
			if started {
				n.run(c)
				rec.Changed = true
			}
			return &wsv1.OpResult{Result: &wsv1.OpResult_EnsureContainer{EnsureContainer: &wsv1.EnsureContainerResult{
				ContainerId: "cid-" + name, Created: false, Started: started}}}
		}
	}
	prev := 0
	if c := n.Containers[name]; c != nil {
		prev = c.Created
	}
	c := &Container{Spec: op, Created: prev + 1}
	n.run(c)
	n.Containers[name] = c
	rec.Created, rec.Changed = true, true
	return &wsv1.OpResult{Result: &wsv1.OpResult_EnsureContainer{EnsureContainer: &wsv1.EnsureContainerResult{
		ContainerId: "cid-" + name, Created: true, Started: true}}}
}

func (n *Node) writeManagedFile(op *wsv1.WriteManagedFile, rec *Op) *wsv1.OpResult {
	p := op.GetPath()
	sum := sha256.Sum256(op.GetContent())
	source := hex.EncodeToString(sum[:])
	if old, ok := n.Files[p]; ok {
		oldSum := sha256.Sum256(old)
		rec.Changed = n.fileSource[p] != source && hex.EncodeToString(oldSum[:]) != source
	} else {
		rec.Changed = true
	}
	if op.GetMaterializeSecrets() {
		refs := secretRef.FindAllStringSubmatch(string(op.GetContent()), -1)
		if len(refs) == 0 {
			return opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "%s: materialize_secrets set but the content carries no secret reference", p)
		}
		for _, m := range refs {
			if _, ok := n.Secrets[m[1]]; !ok {
				return opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "%s references secret/%s, which does not exist on this node", p, m[1])
			}
		}
		n.fileSource[p] = source
		// What lands on disk is not what was sent; nothing reads it back.
		n.Files[p] = []byte(strings.Repeat("x", len(op.GetContent())))
	} else {
		delete(n.fileSource, p)
		n.Files[p] = append([]byte(nil), op.GetContent()...)
	}
	return &wsv1.OpResult{Result: &wsv1.OpResult_WriteManagedFile{WriteManagedFile: &wsv1.WriteManagedFileResult{}}}
}

// Profile is a node's facts as a fixture file holds them
// (testing/fixtures/home-automation/*.json): the probes' answers in
// protojson, so a fixture reads like what the real agent sends.
type Profile struct {
	ID            string
	ProtocolMinor uint32
	Readiness     *wsv1.HomeAutomationReadinessResult
	Radios        *wsv1.ListRadiosResult
}

// LoadProfile reads a node fixture.
func LoadProfile(path string) (Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, err
	}
	var raw struct {
		ID            string          `json:"id"`
		ProtocolMinor uint32          `json:"protocolMinor"`
		Readiness     json.RawMessage `json:"readiness"`
		Radios        json.RawMessage `json:"radios"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Profile{}, fmt.Errorf("%s: %w", path, err)
	}
	p := Profile{ID: raw.ID, ProtocolMinor: raw.ProtocolMinor,
		Readiness: &wsv1.HomeAutomationReadinessResult{}, Radios: &wsv1.ListRadiosResult{}}
	if err := protojson.Unmarshal(raw.Readiness, p.Readiness); err != nil {
		return Profile{}, fmt.Errorf("%s: readiness: %w", path, err)
	}
	if err := protojson.Unmarshal(raw.Radios, p.Radios); err != nil {
		return Profile{}, fmt.Errorf("%s: radios: %w", path, err)
	}
	if p.ID == "" || p.ProtocolMinor == 0 {
		return Profile{}, fmt.Errorf("%s: id and protocolMinor are required", path)
	}
	return p, nil
}

// AddProfile registers a connected node with a profile's facts. Each
// node gets its own copies, so a test may break one without the next.
func (f *Fleet) AddProfile(p Profile) *Node {
	n := f.Add(p.ID, p.ProtocolMinor)
	f.Do(p.ID, func(n *Node) {
		r := proto.Clone(p.Readiness).(*wsv1.HomeAutomationReadinessResult)
		n.Interfaces, n.Checks = r.GetInterfaces(), r.GetChecks()
		n.Radios = proto.Clone(p.Radios).(*wsv1.ListRadiosResult).GetRadios()
	})
	return n
}
