package agent

// Agent-side op audit logging (ADR-W003 W7): every dispatched operation is
// logged at Info via the agent's structured slog logger, so a systemd
// journal captures a per-host trail independent of Keystone's own audit.log
// (proposal §4.2's "two independent audit trails").
//
// auditFields is the ONLY place allowed to decide what a given operation
// arm logs. It is an explicit per-arm allowlist, not a generic "dump the
// message" reflection walk, on purpose: several operations carry payloads
// that may hold secrets (WriteManagedFile.content, SelfUpdate.binary/
// signature) or bulk data, and those must NEVER reach the log. Any new op
// arm that forgets to add a case here falls into the default branch and
// logs only its arm name — safe by default, never secrets by accident.

import (
	"context"
	"log/slog"
	"strings"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// auditFields returns the operation's arm name and a small set of safe,
// non-secret parameter attrs suitable for structured logging. It NEVER
// returns file contents, env values, WriteManagedFile content, or
// SelfUpdate binary/signature bytes.
func auditFields(op *wsv1.Operation) (arm string, fields []slog.Attr) {
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_EnsureContainer:
		attrs := []slog.Attr{
			slog.String("name", o.EnsureContainer.GetName()),
			slog.String("image", o.EnsureContainer.GetImage()),
		}
		// Host access is the audit-worthy half of a container spec
		// (protocol minor 12): which devices and capabilities a
		// container was granted is what a reader of this log wants to
		// know first when something on the host misbehaves.
		if ds := o.EnsureContainer.GetDevices(); len(ds) > 0 {
			attrs = append(attrs, slog.String("devices", describeDevices(ds)))
		}
		if aliases := o.EnsureContainer.GetHostAliases(); len(aliases) > 0 {
			attrs = append(attrs, slog.Any("host_aliases", aliases))
		}
		if caps := o.EnsureContainer.GetCapAdd(); len(caps) > 0 {
			attrs = append(attrs, slog.String("cap_add", strings.Join(caps, ",")))
		}
		// Host networking (protocol minor 23) is host access in the same
		// sense: the container listens on the node's own interfaces.
		if o.EnsureContainer.GetNetworkMode() == wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST {
			attrs = append(attrs, slog.String("network_mode", "host"))
		}
		return "EnsureContainer", attrs
	case *wsv1.Operation_StopContainer:
		return "StopContainer", []slog.Attr{slog.String("name", o.StopContainer.GetName())}
	case *wsv1.Operation_ContainerLogs:
		return "ContainerLogs", []slog.Attr{slog.String("name", o.ContainerLogs.GetName())}
	case *wsv1.Operation_PullImage:
		return "PullImage", []slog.Attr{slog.String("image", o.PullImage.GetImage())}
	case *wsv1.Operation_WriteManagedFile:
		// Path only. Content is potentially secret-bearing and MUST NOT be
		// logged.
		return "WriteManagedFile", []slog.Attr{slog.String("path", o.WriteManagedFile.GetPath())}
	case *wsv1.Operation_ReadManagedFile:
		return "ReadManagedFile", []slog.Attr{slog.String("path", o.ReadManagedFile.GetPath())}
	case *wsv1.Operation_HostFacts:
		return "HostFacts", nil
	case *wsv1.Operation_Health:
		return "Health", nil
	case *wsv1.Operation_SelfUpdate:
		// version and sha256_hex are safe identifiers; binary and signature
		// are raw bytes that MUST NOT be logged.
		return "SelfUpdate", []slog.Attr{
			slog.String("version", o.SelfUpdate.GetVersion()),
			slog.String("sha256_hex", o.SelfUpdate.GetSha256Hex()),
		}
	case *wsv1.Operation_RemoveContainer:
		return "RemoveContainer", []slog.Attr{slog.String("name", o.RemoveContainer.GetName())}
	case *wsv1.Operation_ContainerStats:
		return "ContainerStats", []slog.Attr{slog.String("name", o.ContainerStats.GetName())}
	case *wsv1.Operation_EnsureSecret:
		return "EnsureSecret", []slog.Attr{
			slog.String("name", o.EnsureSecret.GetName()),
			slog.Bool("rotate", o.EnsureSecret.GetRotate()),
		}
	case *wsv1.Operation_PutSecret:
		// Name only. The value is the one payload in the whole protocol
		// that is ALWAYS a secret and MUST NOT be logged.
		return "PutSecret", []slog.Attr{slog.String("name", o.PutSecret.GetName())}
	case *wsv1.Operation_ListSecrets:
		return "ListSecrets", nil
	case *wsv1.Operation_EraseSecret:
		// Name and fence only; the nonce is harmless but belongs to the
		// evidence, not the log.
		return "EraseSecret", []slog.Attr{
			slog.String("name", o.EraseSecret.GetName()),
			slog.Uint64("max_version", uint64(o.EraseSecret.GetMaxVersion())),
		}
	case *wsv1.Operation_ExportSecretBundle:
		// The public key is not confidential, but omitting it keeps recovery
		// material out of routine logs and makes this arm safe by construction.
		return "ExportSecretBundle", nil
	case *wsv1.Operation_ImportSecretBundle:
		// Never log the private key or sealed bundle.
		return "ImportSecretBundle", nil
	case *wsv1.Operation_SeedVolumeFile:
		// Volume and path only. A seed's content is package-rendered
		// config that may carry a reference to something sensitive, and
		// is treated like every other file body: not logged.
		return "SeedVolumeFile", []slog.Attr{
			slog.String("volume", o.SeedVolumeFile.GetVolume()),
			slog.String("path", o.SeedVolumeFile.GetPath()),
		}
	case *wsv1.Operation_ListDirectory:
		return "ListDirectory", []slog.Attr{slog.String("path", o.ListDirectory.GetPath())}
	case *wsv1.Operation_StatVolumeFile:
		return "StatVolumeFile", []slog.Attr{
			slog.String("volume", o.StatVolumeFile.GetVolume()),
			slog.String("path", o.StatVolumeFile.GetPath()),
		}
	case *wsv1.Operation_InspectContainer:
		return "InspectContainer", []slog.Attr{slog.String("name", o.InspectContainer.GetName())}
	case *wsv1.Operation_PruneImages:
		return "PruneImages", nil
	case *wsv1.Operation_EnsureVolume:
		return "EnsureVolume", []slog.Attr{slog.String("name", o.EnsureVolume.GetName())}
	case *wsv1.Operation_SnapshotDir:
		return "SnapshotDir", []slog.Attr{
			slog.String("source_dir", o.SnapshotDir.GetSourceDir()),
			slog.String("dest_path", o.SnapshotDir.GetDestPath()),
		}
	case *wsv1.Operation_MeasureUsage:
		return "MeasureUsage", []slog.Attr{slog.String("path", o.MeasureUsage.GetPath()), slog.String("volume", o.MeasureUsage.GetVolume())}
	case *wsv1.Operation_DiskSpace:
		return "DiskSpace", []slog.Attr{slog.String("path", o.DiskSpace.GetPath()), slog.String("volume", o.DiskSpace.GetVolume())}
	case *wsv1.Operation_ListBlockDevices:
		return "ListBlockDevices", nil
	case *wsv1.Operation_EnsureMount:
		return "EnsureMount", []slog.Attr{
			slog.String("source", o.EnsureMount.GetSource()),
			slog.String("target", o.EnsureMount.GetTarget()),
			slog.String("fstype", o.EnsureMount.GetFstype()),
		}
	case *wsv1.Operation_TestNfs:
		return "TestNfs", []slog.Attr{
			slog.String("host", o.TestNfs.GetHost()),
			slog.Int("port", int(o.TestNfs.GetPort())),
		}
	case *wsv1.Operation_TestSmb:
		return "TestSmb", []slog.Attr{
			slog.String("host", o.TestSmb.GetHost()),
			slog.Int("port", int(o.TestSmb.GetPort())),
		}
	case *wsv1.Operation_ListPorts:
		return "ListPorts", nil
	case *wsv1.Operation_ProbeTcp:
		return "ProbeTcp", []slog.Attr{
			slog.String("host", o.ProbeTcp.GetHost()),
			slog.Int("port", int(o.ProbeTcp.GetPort())),
		}
	case *wsv1.Operation_ProbeHttp:
		return "ProbeHttp", []slog.Attr{slog.String("url", o.ProbeHttp.GetUrl())}
	case *wsv1.Operation_EnsureDockerNetwork:
		return "EnsureDockerNetwork", []slog.Attr{slog.String("name", o.EnsureDockerNetwork.GetName())}
	case *wsv1.Operation_SystemdUnitStatus:
		return "SystemdUnitStatus", []slog.Attr{slog.String("unit", o.SystemdUnitStatus.GetUnit())}
	case *wsv1.Operation_TimeSyncStatus:
		return "TimeSyncStatus", nil
	case *wsv1.Operation_ListRadios:
		return "ListRadios", nil
	case *wsv1.Operation_HomeAutomationReadiness:
		return "HomeAutomationReadiness", nil
	case *wsv1.Operation_ApplyWorkload:
		return "ApplyWorkload", []slog.Attr{
			slog.String("name", o.ApplyWorkload.GetWorkload().GetName()),
			slog.Uint64("leader_epoch", o.ApplyWorkload.GetAuthority().GetLeaderEpoch()),
		}
	case *wsv1.Operation_DeleteWorkload:
		return "DeleteWorkload", []slog.Attr{
			slog.String("name", o.DeleteWorkload.GetName()),
			slog.Uint64("leader_epoch", o.DeleteWorkload.GetAuthority().GetLeaderEpoch()),
		}
	case *wsv1.Operation_ObserveWorkload:
		return "ObserveWorkload", []slog.Attr{slog.String("name", o.ObserveWorkload.GetName())}
	case *wsv1.Operation_WorkloadLogs:
		return "WorkloadLogs", []slog.Attr{slog.String("name", o.WorkloadLogs.GetName())}
	default:
		// Unknown/future op arm (protocol skew, or a new arm someone forgot
		// to add a case for above): log the fact that something was
		// dispatched, nothing else. Safe default.
		return "unknown", nil
	}
}

// auditDispatch logs the pre-dispatch (arm + safe params) and post-dispatch
// (outcome + error code) lines around handler execution. run performs the
// actual dispatch (dispatchOp); auditDispatch never inspects its own
// arguments beyond what auditFields extracts, so it cannot accidentally log
// anything auditFields did not explicitly allow.
func (a *Agent) auditDispatch(ctx context.Context, opID string, op *wsv1.Operation, run func() *wsv1.OpResult) *wsv1.OpResult {
	arm, fields := auditFields(op)
	preAttrs := make([]slog.Attr, 0, len(fields)+2)
	preAttrs = append(preAttrs, slog.String("op_id", opID), slog.String("arm", arm))
	preAttrs = append(preAttrs, fields...)
	a.log.LogAttrs(ctx, slog.LevelInfo, "dispatching operation", preAttrs...)

	res := run()

	outcome, errCode := "ok", ""
	if oerr := res.GetError(); oerr != nil {
		outcome = "error"
		errCode = oerr.GetCode().String()
	}
	a.log.LogAttrs(ctx, slog.LevelInfo, "operation complete",
		slog.String("op_id", opID),
		slog.String("arm", arm),
		slog.String("outcome", outcome),
		slog.String("error_code", errCode),
	)
	return res
}
