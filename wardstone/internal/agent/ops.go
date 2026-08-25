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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "mkdir: %v", err)
	}
	tmp := path + ".wildstone-tmp"
	if err := os.WriteFile(tmp, op.GetContent(), mode); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "write: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "rename: %v", err)
	}
	sum := sha256.Sum256(op.GetContent())
	return &wsv1.OpResult_WriteManagedFile{
		WriteManagedFile: &wsv1.WriteManagedFileResult{Sha256Hex: hex.EncodeToString(sum[:])},
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
	for k, v := range op.GetEnv() {
		env = append(env, k+"="+v)
	}
	var binds []string
	for _, m := range op.GetMounts() {
		b := m.GetSource() + ":" + m.GetTarget()
		if m.GetReadOnly() {
			b += ":ro"
		}
		binds = append(binds, b)
	}
	ports := map[string][]portBinding{}
	for _, p := range op.GetPorts() {
		key := fmt.Sprintf("%d/tcp", p.GetContainerPort())
		ports[key] = append(ports[key], portBinding{HostIP: p.GetHostIp(), HostPort: fmt.Sprint(p.GetHostPort())})
	}
	restart := op.GetRestartPolicy()
	if restart == "" {
		restart = "unless-stopped"
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
		},
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
	if err := a.docker.pull(ctx, op.GetImage()); err != nil {
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
		Running: ci.State.Running,
		Image:   ci.Config.Image,
		Labels:  ci.Config.Labels,
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
