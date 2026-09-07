package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// OwnerLabel marks Docker objects Wardstone manages. It refuses to touch
// anything without it (defense in depth: even a compromised Keystone
// cannot use the agent against containers Wildstone does not own).
const OwnerLabel = "dev.wildstone.owner"

const ownerValue = "wildstone"

// SpecHashLabel carries Keystone's hash of the full desired container
// spec (ADR-W008 R2c, protocol minor 5). Keystone stamps it at
// EnsureContainer; ensureContainer compares it to decide replacement, and
// InspectContainer returns it so observe can detect spec drift. The agent
// never computes it — it only compares and echoes.
const SpecHashLabel = "dev.wildstone.spec-hash"

var digestRef = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./_:-]*@sha256:[a-f0-9]{64}$`)

const maxLogBytes = 1 << 20 // 1 MiB cap on returned logs

// dispatch executes one typed operation, wrapped with the ADR-W003 W7 audit
// log (arm + safe params before, outcome + error code after — see audit.go).
func (a *Agent) dispatch(ctx context.Context, opID string, op *wsv1.Operation) *wsv1.OpResult {
	return a.auditDispatch(ctx, opID, op, func() *wsv1.OpResult {
		return a.dispatchOp(ctx, opID, op)
	})
}

// dispatchOp is the actual per-arm handler switch. Every arm returns a
// result or a typed OpError; nothing here ever shells out.
func (a *Agent) dispatchOp(ctx context.Context, opID string, op *wsv1.Operation) *wsv1.OpResult {
	res := &wsv1.OpResult{OpId: opID}
	var oerr *wsv1.OpError
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_EnsureContainer:
		var r *wsv1.OpResult_EnsureContainer
		if r, oerr = a.ensureContainer(ctx, o.EnsureContainer); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_StopContainer:
		var r *wsv1.OpResult_StopContainer
		if r, oerr = a.stopContainer(ctx, o.StopContainer); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_ContainerLogs:
		var r *wsv1.OpResult_ContainerLogs
		if r, oerr = a.containerLogs(ctx, o.ContainerLogs); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_PullImage:
		var r *wsv1.OpResult_PullImage
		if r, oerr = a.pullImage(ctx, o.PullImage); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_WriteManagedFile:
		var r *wsv1.OpResult_WriteManagedFile
		if r, oerr = a.writeManagedFile(o.WriteManagedFile); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_ReadManagedFile:
		var r *wsv1.OpResult_ReadManagedFile
		if r, oerr = a.readManagedFile(o.ReadManagedFile); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_HostFacts:
		var r *wsv1.OpResult_HostFacts
		if r, oerr = a.hostFacts(); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_Health:
		res.Result = &wsv1.OpResult_Health{Health: &wsv1.HealthResult{Ok: true, Detail: "agent " + a.cfg.Version}}
	case *wsv1.Operation_SelfUpdate:
		var r *wsv1.OpResult_SelfUpdate
		if r, oerr = a.selfUpdate(o.SelfUpdate); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_RemoveContainer:
		var r *wsv1.OpResult_RemoveContainer
		if r, oerr = a.removeContainer(ctx, o.RemoveContainer); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_ContainerStats:
		var r *wsv1.OpResult_ContainerStats
		if r, oerr = a.containerStats(ctx, o.ContainerStats); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_InspectContainer:
		var r *wsv1.OpResult_InspectContainer
		if r, oerr = a.inspectContainer(ctx, o.InspectContainer); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_EnsureSecret:
		var r *wsv1.OpResult_EnsureSecret
		if r, oerr = a.ensureSecret(o.EnsureSecret); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_PutSecret:
		var r *wsv1.OpResult_PutSecret
		if r, oerr = a.putSecret(o.PutSecret); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_ListSecrets:
		var r *wsv1.OpResult_ListSecrets
		if r, oerr = a.listSecrets(); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_SeedVolumeFile:
		var r *wsv1.OpResult_SeedVolumeFile
		if r, oerr = a.seedVolumeFile(ctx, o.SeedVolumeFile); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_StatVolumeFile:
		var r *wsv1.OpResult_StatVolumeFile
		if r, oerr = a.statVolumeFile(ctx, o.StatVolumeFile); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_PruneImages:
		var r *wsv1.OpResult_PruneImages
		if r, oerr = a.pruneImages(ctx, o.PruneImages); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_EnsureVolume:
		var r *wsv1.OpResult_EnsureVolume
		if r, oerr = a.ensureVolume(ctx, o.EnsureVolume); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_SnapshotDir:
		var r *wsv1.OpResult_SnapshotDir
		if r, oerr = a.snapshotDir(o.SnapshotDir); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_MeasureUsage:
		var r *wsv1.OpResult_MeasureUsage
		if r, oerr = a.measureUsage(o.MeasureUsage); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_ListBlockDevices:
		var r *wsv1.OpResult_ListBlockDevices
		if r, oerr = a.listBlockDevices(); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_EnsureMount:
		var r *wsv1.OpResult_EnsureMount
		if r, oerr = a.ensureMount(o.EnsureMount); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_TestNfs:
		var r *wsv1.OpResult_TestNfs
		if r, oerr = a.testNfs(ctx, o.TestNfs); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_TestSmb:
		var r *wsv1.OpResult_TestSmb
		if r, oerr = a.testSmb(ctx, o.TestSmb); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_ListPorts:
		var r *wsv1.OpResult_ListPorts
		if r, oerr = a.listPorts(); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_ProbeTcp:
		var r *wsv1.OpResult_ProbeTcp
		if r, oerr = a.probeTcp(ctx, o.ProbeTcp); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_ProbeHttp:
		var r *wsv1.OpResult_ProbeHttp
		if r, oerr = a.probeHttp(ctx, o.ProbeHttp); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_EnsureDockerNetwork:
		var r *wsv1.OpResult_EnsureDockerNetwork
		if r, oerr = a.ensureDockerNetwork(ctx, o.EnsureDockerNetwork); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_SystemdUnitStatus:
		var r *wsv1.OpResult_SystemdUnitStatus
		if r, oerr = a.systemdUnitStatus(ctx, o.SystemdUnitStatus); oerr == nil {
			res.Result = r
		}
	case *wsv1.Operation_TimeSyncStatus:
		var r *wsv1.OpResult_TimeSyncStatus
		if r, oerr = a.timeSyncStatus(o.TimeSyncStatus); oerr == nil {
			res.Result = r
		}
	default:
		oerr = opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "unknown operation — agent too old for this Keystone? (protocol skew)")
	}
	if oerr != nil {
		res.Result = &wsv1.OpResult_Error{Error: oerr}
	}
	return res
}

func opErr(code wsv1.ErrorCode, format string, args ...any) *wsv1.OpError {
	return &wsv1.OpError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------- files

// checkBindSource refuses a host-path bind mount that overlaps the agent's
// own data directory, in either direction.
//
// The managed-prefix rule governs where the agent may READ and WRITE. It
// says nothing about what a container may be handed, and those are not the
// same question: the ingress packages bind-mount the managed prefix so
// Caddy can read its rendered config, and the agent's data directory
// defaults to a path INSIDE that prefix — so an internet-facing reverse
// proxy could read node.key (this node's mTLS identity), secret.key (the
// key that decrypts the store) and every secret under it. The host
// directory is 0700, which does not help: the container runs as uid 0.
// readOnly does not help either, because reading is the compromise.
//
// Both directions are refused. A source ABOVE the data dir hands over the
// store by containing it; a source INSIDE it hands over part of the store
// directly. The check is on the symlink-resolved path, so a link cannot
// launder either case.
//
// Non-absolute sources are Docker volume names, not host paths, and are
// none of this function's business.
func (a *Agent) checkBindSource(src string) *wsv1.OpError {
	if src == "" || !filepath.IsAbs(src) {
		return nil
	}
	resolved, err := resolveExisting(filepath.Clean(src))
	if err != nil {
		return opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "resolve mount source %q: %v", src, err)
	}
	data, err := resolveExisting(filepath.Clean(a.cfg.DataDir))
	if err != nil {
		return opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "resolve data dir: %v", err)
	}
	if !pathOverlaps(resolved, data) {
		return nil
	}
	return opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
		"mount source %q overlaps the agent data directory %q: a container given this path "+
			"can read the node's identity and secret store", src, a.cfg.DataDir)
}

// pathOverlaps reports whether a and b are equal or one contains the other.
// Compares whole path elements, so /etc/wildstone-other does not count as
// being under /etc/wildstone.
func pathOverlaps(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasPrefix(a, dirPrefix(b)) || strings.HasPrefix(b, dirPrefix(a))
}

// dirPrefix returns the string every path strictly under dir starts with.
// The root needs its own case: naively appending a separator to "/" yields
// "//", which nothing has as a prefix, so a mount of "/" — the broadest
// possible escape — would have been allowed. Caught by the test rather
// than foreseen.
func dirPrefix(dir string) string {
	if strings.HasSuffix(dir, string(filepath.Separator)) {
		return dir
	}
	return dir + string(filepath.Separator)
}

// resolveManaged enforces the managed-prefix invariant: the cleaned,
// symlink-resolved path must live under one of the configured prefixes.
func (a *Agent) resolveManaged(p string) (string, *wsv1.OpError) {
	if !filepath.IsAbs(p) {
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN, "path %q is not absolute", p)
	}
	clean := filepath.Clean(p)
	// Resolve symlinks on the deepest existing ancestor so a link inside a
	// managed prefix cannot smuggle writes outside it.
	resolvedDir, err := resolveExisting(filepath.Dir(clean))
	if err != nil {
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "resolve %q: %v", p, err)
	}
	resolved := filepath.Join(resolvedDir, filepath.Base(clean))
	if !a.underManagedPrefix(resolved) {
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"path %q resolves outside managed prefixes %v", p, a.cfg.ManagedPrefixes)
	}
	// The parent chain is now symlink-resolved, but the FINAL component may
	// itself be a symlink pointing outside the managed tree — the prefix
	// check above only inspected the unresolved link path. If it exists,
	// resolve it fully and require the real target to stay within a managed
	// prefix; otherwise a read (ReadManagedFile), a mount, or a bind-mount
	// through the link would escape the boundary. A non-existent final
	// component (the ordinary create case) has nothing to follow.
	final, ferr := filepath.EvalSymlinks(resolved)
	if ferr != nil {
		if os.IsNotExist(ferr) {
			return resolved, nil
		}
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "resolve %q: %v", p, ferr)
	}
	if !a.underManagedPrefix(final) {
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"path %q resolves via a symlink to %q, outside managed prefixes %v", p, final, a.cfg.ManagedPrefixes)
	}
	return final, nil
}

// underManagedPrefix reports whether an absolute, cleaned path lies at or
// under one of the configured managed prefixes.
func (a *Agent) underManagedPrefix(path string) bool {
	for _, prefix := range a.cfg.ManagedPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func resolveExisting(dir string) (string, error) {
	for d := dir; ; d = filepath.Dir(d) {
		resolved, err := filepath.EvalSymlinks(d)
		if err == nil {
			rel, rerr := filepath.Rel(d, dir)
			if rerr != nil {
				return "", rerr
			}
			return filepath.Join(resolved, rel), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if d == filepath.Dir(d) {
			return dir, nil // hit the root without finding anything existing
		}
	}
}

func (a *Agent) writeManagedFile(op *wsv1.WriteManagedFile) (*wsv1.OpResult_WriteManagedFile, *wsv1.OpError) {
	path, oerr := a.resolveManaged(op.GetPath())
	if oerr != nil {
		return nil, oerr
	}
	// Mask to permission bits only: a caller-supplied mode must never set
	// setuid/setgid/sticky, which would let a managed file become a
	// setuid-root binary that is then bind-mountable into a container.
	mode := os.FileMode(op.GetMode()) & 0o777
	if mode == 0 {
		mode = 0o644
	}

	// The hash is of what the CALLER SENT, always — it is the only thing
	// the caller can verify, and after materialization the bytes on disk
	// are something it has never seen.
	sum := sha256.Sum256(op.GetContent())
	sourceHex := hex.EncodeToString(sum[:])

	content := op.GetContent()
	if op.GetMaterializeSecrets() {
		if !containsSecretRef(content) {
			// A materializing write with nothing to materialize is a
			// renderer that stopped emitting references, which would
			// otherwise ship a config with a secret-shaped hole in it and
			// no error anywhere.
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID,
				"%s: materialize_secrets set but the content carries no secret reference", op.GetPath())
		}
		var oerr *wsv1.OpError
		if content, oerr = a.materializeFileContent(op.GetPath(), content); oerr != nil {
			return nil, oerr
		}
		// A materialized file holds live secrets. 0644 would put them in
		// reach of every user on the host and of any container given the
		// directory, so the caller's mode is narrowed rather than
		// honored — the containers that need it are handed the file
		// explicitly, and nothing else has business reading it.
		mode &= 0o600
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "mkdir: %v", err)
	}
	tmp := path + ".wildstone-tmp"
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "write: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "rename: %v", err)
	}

	// Record (or clear) the source hash AFTER the rename, so a crash
	// between the two leaves a file whose recorded source is stale rather
	// than one that claims a source it does not have. A stale record
	// reads as drift and is repaired on the next tick; the reverse would
	// read as converged and never be.
	if oerr := a.recordManagedSource(op.GetPath(), op.GetMaterializeSecrets(), sourceHex); oerr != nil {
		return nil, oerr
	}

	return &wsv1.OpResult_WriteManagedFile{
		WriteManagedFile: &wsv1.WriteManagedFileResult{Sha256Hex: sourceHex},
	}, nil
}

func (a *Agent) readManagedFile(op *wsv1.ReadManagedFile) (*wsv1.OpResult_ReadManagedFile, *wsv1.OpError) {
	path, oerr := a.resolveManaged(op.GetPath())
	if oerr != nil {
		return nil, oerr
	}
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "%s", op.GetPath())
	}
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "read: %v", err)
	}
	sourceHex, oerr := a.readManagedSource(op.GetPath())
	if oerr != nil {
		return nil, oerr
	}
	if sourceHex != "" {
		// Materialized: the bytes on disk hold live secrets and are not
		// what the caller sent. Return the pre-materialization hash and
		// NOT the content — that answers "did this drift?" without
		// carrying a secret to Keystone, into a plan, an error string, or
		// the Butler's tool results.
		return &wsv1.OpResult_ReadManagedFile{
			ReadManagedFile: &wsv1.ReadManagedFileResult{SourceSha256Hex: sourceHex},
		}, nil
	}
	return &wsv1.OpResult_ReadManagedFile{ReadManagedFile: &wsv1.ReadManagedFileResult{Content: content}}, nil
}

// ---------------------------------------------------------------- containers

func (a *Agent) ensureContainer(ctx context.Context, op *wsv1.EnsureContainer) (*wsv1.OpResult_EnsureContainer, *wsv1.OpError) {
	if !digestRef.MatchString(op.GetImage()) {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"image %q is not digest-pinned (repo@sha256:... required)", op.GetImage())
	}
	for _, m := range op.GetMounts() {
		if strings.Contains(m.GetSource(), "/") { // host path, not a named volume
			if _, oerr := a.resolveManaged(m.GetSource()); oerr != nil {
				return nil, oerr
			}
		}
	}
	// Materialize secret/<name> env references from the node store
	// (ADR-W012 A4) — the plaintext goes into the docker create body and
	// nowhere else. Resolved up front, BEFORE the replace path below can
	// stop/remove an existing container: a dangling reference must fail
	// the op with the old container still running, not after it is gone.
	materialized, oerr := a.materializeEnv(op.GetEnv())
	if oerr != nil {
		return nil, oerr
	}
	// Devices and capabilities (protocol minor 12) are checked here, up
	// front, for the same reason: a device that is not on the allowlist
	// or not plugged in yet must fail the op with the OLD container still
	// running. Checked after the replace below, enabling Frigate's Coral
	// before the TPU was seated turned a validation error into an
	// outage — the healthy container was already stopped and removed
	// when NOT_FOUND came back.
	var devices []deviceMapping
	for _, d := range op.GetDevices() {
		dm, oe := a.checkDevice(d)
		if oe != nil {
			return nil, oe
		}
		devices = append(devices, dm)
	}
	var capAdd []string
	for _, c := range op.GetCapAdd() {
		if oe := checkCapability(c); oe != nil {
			return nil, oe
		}
		capAdd = append(capAdd, c)
	}
	// A network must exist and be wildstone-owned before anything is
	// attached to it (protocol minor 8). Attaching to a foreign network
	// would join the container to whatever else lives there, so the same
	// rule EnsureDockerNetwork applies to creation applies to use:
	// never adopt a resource Wardstone did not create.
	if net := op.GetNetwork(); net != "" {
		ni, err := a.docker.networkInspect(ctx, net)
		if derr, ok := err.(*dockerError); err != nil && (!ok || !derr.notFound()) {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "network inspect: %v", err)
		}
		if ni == nil {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND,
				"network %q does not exist (EnsureDockerNetwork first)", net)
		}
		if ni.Labels[OwnerLabel] != ownerValue {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
				"network %q exists but is not wildstone-owned", net)
		}
	}

	existing, err := a.docker.inspect(ctx, op.GetName())
	if derr, ok := err.(*dockerError); err != nil && (!ok || !derr.notFound()) {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "inspect: %v", err)
	}
	if existing != nil {
		if existing.Config.Labels[OwnerLabel] != ownerValue {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
				"container %q exists but is not wildstone-owned", op.GetName())
		}
		// Replace when the spec changed. The spec-hash label (stamped by
		// Keystone on every EnsureContainer since protocol minor 5,
		// ADR-W008 R2c) covers the WHOLE spec — env, ports, mounts,
		// restart policy — which the pre-R2c image-only comparison
		// missed. When the incoming op carries no spec-hash label (an
		// older Keystone), fall back to the image comparison so N−1
		// keeps its exact previous behavior.
		wantHash, hasHash := op.GetLabels()[SpecHashLabel]
		specMatches := existing.Config.Image == op.GetImage()
		if hasHash {
			specMatches = existing.Config.Labels[SpecHashLabel] == wantHash
		}
		if specMatches {
			started := false
			if !existing.State.Running {
				if err := a.docker.start(ctx, existing.ID); err != nil {
					return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "start: %v", err)
				}
				started = true
			}
			return &wsv1.OpResult_EnsureContainer{EnsureContainer: &wsv1.EnsureContainerResult{
				ContainerId: existing.ID, Created: false, Started: started,
			}}, nil
		}
		// Spec changed: replace. (Spike kept this simple; health-gated
		// replacement is the reconciler's job, per step.)
		if err := a.docker.stop(ctx, existing.ID, 10); err != nil {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "stop for replace: %v", err)
		}
		if _, err := a.docker.call(ctx, "DELETE", "/containers/"+existing.ID, nil, nil); err != nil {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "remove for replace: %v", err)
		}
	}

	labels := map[string]string{}
	for k, v := range op.GetLabels() {
		labels[k] = v
	}
	labels[OwnerLabel] = ownerValue

	var env []string
	for k, v := range materialized {
		env = append(env, k+"="+v)
	}
	var binds []string
	for _, m := range op.GetMounts() {
		if oe := a.checkBindSource(m.GetSource()); oe != nil {
			return nil, oe
		}
		b := m.GetSource() + ":" + m.GetTarget()
		if m.GetReadOnly() {
			b += ":ro"
		}
		binds = append(binds, b)
	}
	ports := map[string][]portBinding{}
	for _, p := range op.GetPorts() {
		key := fmt.Sprintf("%d/%s", p.GetContainerPort(), portProtocol(p.GetProtocol()))
		ports[key] = append(ports[key], portBinding{HostIP: p.GetHostIp(), HostPort: fmt.Sprint(p.GetHostPort())})
	}
	restart := op.GetRestartPolicy()
	if restart == "" {
		restart = "unless-stopped"
	}

	var netCfg *networkingConfig
	if net := op.GetNetwork(); net != "" {
		netCfg = &networkingConfig{EndpointsConfig: map[string]endpointConfig{net: {}}}
	}

	id, err := a.docker.create(ctx, op.GetName(), createBody{
		Image:  op.GetImage(),
		Cmd:    op.GetCommand(),
		Env:    env,
		Labels: labels,
		HostConfig: hostConfig{
			Binds:         binds,
			PortBindings:  ports,
			RestartPolicy: restartPolicy{Name: restart},
			Devices:       devices,
			CapAdd:        capAdd,
		},
		NetworkingConfig: netCfg,
	})
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "create: %v", err)
	}
	if err := a.docker.start(ctx, id); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "start: %v", err)
	}
	return &wsv1.OpResult_EnsureContainer{EnsureContainer: &wsv1.EnsureContainerResult{
		ContainerId: id, Created: true, Started: true,
	}}, nil
}

// requireOwned loads a container and enforces the ownership label.
func (a *Agent) requireOwned(ctx context.Context, name string) (*containerInspect, *wsv1.OpError) {
	ci, err := a.docker.inspect(ctx, name)
	if derr, ok := err.(*dockerError); ok && derr.notFound() {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "container %q", name)
	}
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "inspect: %v", err)
	}
	if ci.Config.Labels[OwnerLabel] != ownerValue {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN, "container %q is not wildstone-owned", name)
	}
	return ci, nil
}

func (a *Agent) stopContainer(ctx context.Context, op *wsv1.StopContainer) (*wsv1.OpResult_StopContainer, *wsv1.OpError) {
	ci, oerr := a.requireOwned(ctx, op.GetName())
	if oerr != nil {
		return nil, oerr
	}
	if !ci.State.Running {
		return &wsv1.OpResult_StopContainer{StopContainer: &wsv1.StopContainerResult{WasRunning: false}}, nil
	}
	if err := a.docker.stop(ctx, ci.ID, op.GetTimeoutSeconds()); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "stop: %v", err)
	}
	return &wsv1.OpResult_StopContainer{StopContainer: &wsv1.StopContainerResult{WasRunning: true}}, nil
}

func (a *Agent) containerLogs(ctx context.Context, op *wsv1.ContainerLogs) (*wsv1.OpResult_ContainerLogs, *wsv1.OpError) {
	ci, oerr := a.requireOwned(ctx, op.GetName())
	if oerr != nil {
		return nil, oerr
	}
	logs, err := a.docker.logs(ctx, ci.ID, op.GetTail(), op.GetSinceUnixSec())
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "logs: %v", err)
	}
	if len(logs) > maxLogBytes {
		logs = logs[len(logs)-maxLogBytes:]
	}
	return &wsv1.OpResult_ContainerLogs{ContainerLogs: &wsv1.ContainerLogsResult{Logs: logs}}, nil
}

func (a *Agent) pullImage(ctx context.Context, op *wsv1.PullImage) (*wsv1.OpResult_PullImage, *wsv1.OpError) {
	if !digestRef.MatchString(op.GetImage()) {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"image %q is not digest-pinned (repo@sha256:... required)", op.GetImage())
	}
	// A credential for this image's registry, if the node holds one
	// (F94). Absent is the ordinary case and stays an anonymous pull;
	// present-but-unreadable is an error, because an anonymous pull
	// against a private registry fails as "not found" and sends the
	// operator looking for the wrong problem.
	auth, err := a.registryAuthFor(op.GetImage())
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "%v", err)
	}
	if err := a.docker.pull(ctx, op.GetImage(), auth); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "pull: %v", err)
	}
	digest := op.GetImage()[strings.Index(op.GetImage(), "@")+1:]
	return &wsv1.OpResult_PullImage{PullImage: &wsv1.PullImageResult{Digest: digest}}, nil
}

// removeContainer is idempotent: a container that does not exist is a
// success (existed=false), not NOT_FOUND. Ownership is still enforced
// before that idempotence kicks in — a foreign container is FORBIDDEN,
// never silently reported as "didn't exist".
func (a *Agent) removeContainer(ctx context.Context, op *wsv1.RemoveContainer) (*wsv1.OpResult_RemoveContainer, *wsv1.OpError) {
	ci, err := a.docker.inspect(ctx, op.GetName())
	if derr, ok := err.(*dockerError); ok && derr.notFound() {
		return &wsv1.OpResult_RemoveContainer{RemoveContainer: &wsv1.RemoveContainerResult{Existed: false}}, nil
	}
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "inspect: %v", err)
	}
	if ci.Config.Labels[OwnerLabel] != ownerValue {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN, "container %q is not wildstone-owned", op.GetName())
	}
	if err := a.docker.remove(ctx, ci.ID, op.GetForce()); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "remove: %v", err)
	}
	return &wsv1.OpResult_RemoveContainer{RemoveContainer: &wsv1.RemoveContainerResult{Existed: true}}, nil
}

func (a *Agent) containerStats(ctx context.Context, op *wsv1.ContainerStats) (*wsv1.OpResult_ContainerStats, *wsv1.OpError) {
	ci, oerr := a.requireOwned(ctx, op.GetName())
	if oerr != nil {
		return nil, oerr
	}
	st, err := a.docker.stats(ctx, ci.ID)
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "stats: %v", err)
	}
	return &wsv1.OpResult_ContainerStats{ContainerStats: &wsv1.ContainerStatsResult{
		CpuUsageNs:    st.CPUStats.CPUUsage.TotalUsage,
		MemUsageBytes: st.MemoryStats.Usage,
		MemLimitBytes: st.MemoryStats.Limit,
	}}, nil
}

// inspectContainer (protocol minor 5, ADR-W008 R2c) reads back a
// wildstone-owned container's identity — image, labels, run state — so
// Keystone's observe step can detect spec drift via the
// dev.wildstone.spec-hash label, not just presence. Ownership semantics
// are requireOwned's, identical to containerStats: FORBIDDEN for a
// foreign container (checked before existence), NOT_FOUND when absent.
// Env values are deliberately not returned (they may carry secrets).
func (a *Agent) inspectContainer(ctx context.Context, op *wsv1.InspectContainer) (*wsv1.OpResult_InspectContainer, *wsv1.OpError) {
	ci, oerr := a.requireOwned(ctx, op.GetName())
	if oerr != nil {
		return nil, oerr
	}
	return &wsv1.OpResult_InspectContainer{InspectContainer: &wsv1.InspectContainerResult{
		Running:  ci.State.Running,
		Image:    ci.Config.Image,
		Labels:   ci.Config.Labels,
		Status:   ci.State.Status,
		ExitCode: ci.State.ExitCode,
	}}, nil
}

// pruneImages MUST scope to wildstone-owned images only (ADR-W003 §A4.3);
// the label filter lives in dockerClient.pruneImages — never bypass it.
func (a *Agent) pruneImages(ctx context.Context, _ *wsv1.PruneImages) (*wsv1.OpResult_PruneImages, *wsv1.OpError) {
	res, err := a.docker.pruneImages(ctx)
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "prune images: %v", err)
	}
	return &wsv1.OpResult_PruneImages{PruneImages: &wsv1.PruneImagesResult{
		ReclaimedBytes: res.SpaceReclaimed,
		DeletedCount:   uint32(len(res.ImagesDeleted)),
	}}, nil
}

// ---------------------------------------------------------------- volumes

// ensureVolume creates an owner-labeled named volume if absent. Docker's
// own /volumes/create is idempotent but does NOT update labels on an
// existing volume, so the agent inspects first and treats a pre-existing,
// non-wildstone-owned volume as FORBIDDEN rather than silently adopting
// it (ADR-W003 §A4.3 applied to volumes, not just containers).
func (a *Agent) ensureVolume(ctx context.Context, op *wsv1.EnsureVolume) (*wsv1.OpResult_EnsureVolume, *wsv1.OpError) {
	existing, err := a.docker.volumeInspect(ctx, op.GetName())
	if derr, ok := err.(*dockerError); err != nil && (!ok || !derr.notFound()) {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "volume inspect: %v", err)
	}
	if existing != nil {
		if existing.Labels[OwnerLabel] != ownerValue {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
				"volume %q exists but is not wildstone-owned", op.GetName())
		}
		return &wsv1.OpResult_EnsureVolume{EnsureVolume: &wsv1.EnsureVolumeResult{Created: false}}, nil
	}
	if err := a.docker.volumeCreate(ctx, op.GetName(), map[string]string{OwnerLabel: ownerValue}); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "volume create: %v", err)
	}
	return &wsv1.OpResult_EnsureVolume{EnsureVolume: &wsv1.EnsureVolumeResult{Created: true}}, nil
}

// ---------------------------------------------------------------- networks

// ensureDockerNetwork mirrors ensureVolume's idempotent/ownership pattern
// (ADR-W003 §A4.3 applied to networks): absent -> create (created=true);
// present and wildstone-owned -> created=false; present and NOT
// wildstone-owned -> FORBIDDEN (never adopt a network Wardstone did not
// create).
func (a *Agent) ensureDockerNetwork(ctx context.Context, op *wsv1.EnsureDockerNetwork) (*wsv1.OpResult_EnsureDockerNetwork, *wsv1.OpError) {
	existing, err := a.docker.networkInspect(ctx, op.GetName())
	if derr, ok := err.(*dockerError); err != nil && (!ok || !derr.notFound()) {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "network inspect: %v", err)
	}
	if existing != nil {
		if existing.Labels[OwnerLabel] != ownerValue {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
				"network %q exists but is not wildstone-owned", op.GetName())
		}
		return &wsv1.OpResult_EnsureDockerNetwork{EnsureDockerNetwork: &wsv1.EnsureDockerNetworkResult{Created: false}}, nil
	}
	if err := a.docker.networkCreate(ctx, op.GetName(), map[string]string{OwnerLabel: ownerValue}); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "network create: %v", err)
	}
	return &wsv1.OpResult_EnsureDockerNetwork{EnsureDockerNetwork: &wsv1.EnsureDockerNetworkResult{Created: true}}, nil
}
