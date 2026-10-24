package agent

// Seeding: write a file into an application's OWN data volume, once,
// before that application first starts (protocol minor 13).
//
// Why this exists as its own operation rather than as a managed file:
// some applications only read a setting on their very first start. Home
// Assistant is the case that forced it — on first boot it migrates any
// `http:` block out of configuration.yaml into its own store and marks
// the migration done, after which the YAML is ignored forever. A file
// mounted alongside the app after that first boot can never win, no
// matter how correct it is.
//
// The contract is deliberately narrow, because writing into user data is
// the one thing the managed-file path is built to avoid:
//
//   - WRITE IF ABSENT, never overwrite. Enforced with O_EXCL, not with a
//     stat-then-write: the kernel decides, so a retry, a concurrent seed
//     and a user editing the file at the same moment cannot combine into
//     a lost edit.
//   - The file is the APPLICATION'S from the moment it lands. Wildstone
//     does not read it back, compare it, revert it, or delete it.
//   - Every path is resolved through an *os.Root anchored at the volume,
//     so no symlink anywhere along it can redirect a write onto the
//     host.
//
// CONFINEMENT, AND WHY IT IS NOT DONE BY HAND. The first version of this
// resolved the parent directory with EvalSymlinks and, on ENOENT, took
// the missing parent as proof the path was confined. That is wrong when
// an EARLIER component is a symlink and a later one is absent: with
// `escape -> /etc` and the path `escape/newdir/file`, EvalSymlinks
// returns ENOENT, the check passes, and MkdirAll then follows the
// symlink and writes outside the volume — as root. os.Root does the
// resolution with directory-relative syscalls instead, refusing an
// escape at every component, which is the only version of this that is
// worth trusting.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// checkSeedPath rejects a path that is not a plain relative path inside
// the volume. os.Root is what actually enforces confinement; this is the
// cheap, legible half that turns an obviously bad request into a clear
// INVALID rather than a syscall error.
func checkSeedPath(rel string) *wsv1.OpError {
	if rel == "" {
		return opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "seed: no path named")
	}
	if filepath.IsAbs(rel) {
		return opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "seed: path %q must be relative to the volume", rel)
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == "." || seg == ".." || seg == "" {
			return opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "seed: path %q must name a file inside the volume", rel)
		}
	}
	return nil
}

// volumeRoot opens the volume's backing directory as an os.Root, inside
// which no operation can escape however the filesystem is arranged.
//
// The caller closes the Root.
func (a *Agent) volumeRoot(ctx context.Context, volume string) (*os.Root, *wsv1.OpError) {
	if volume == "" {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "seed: no volume named")
	}
	vol, err := a.runtime.volumeInspect(ctx, volume)
	if err != nil {
		if isNotFound(err) {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "seed: volume %q does not exist", volume)
		}
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: volume inspect: %v", err)
	}
	// Only ever seed a volume Wildstone created. Seeding writes into data
	// we would otherwise never touch; refusing an adopted volume keeps a
	// package from reaching into something a person set up by hand.
	//
	// This is a NECESSARY check, not a sufficient one: the label says
	// Wildstone owns the volume, not WHICH capability does. Keystone is
	// what holds the desired state and so is the only side that can know
	// that; it checks before dispatching (seedVolumeIsOwnedBy).
	if vol.Labels[OwnerLabel] != ownerValue {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"seed: volume %q exists but is not wildstone-owned", volume)
	}
	if vol.Mountpoint == "" {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: volume %q reports no mountpoint", volume)
	}
	root, err := os.OpenRoot(vol.Mountpoint)
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: open volume %q: %v", volume, err)
	}
	return root, nil
}

// seedVolumeFile writes the file only if it is not already there.
//
// "Already there" is reported as written=false and NOT as an error: after
// the first tick that is the permanent steady state, and a reconciler
// that treated it as a failure would set the resource aside forever.
func (a *Agent) seedVolumeFile(ctx context.Context, op *wsv1.SeedVolumeFile) (*wsv1.OpResult_SeedVolumeFile, *wsv1.OpError) {
	rel := filepath.ToSlash(op.GetPath())
	if oerr := checkSeedPath(rel); oerr != nil {
		return nil, oerr
	}
	root, oerr := a.volumeRoot(ctx, op.GetVolume())
	if oerr != nil {
		return nil, oerr
	}
	defer root.Close() //nolint:errcheck

	// Same masking as writeManagedFile: a caller-supplied mode must never
	// carry setuid/setgid/sticky.
	mode := os.FileMode(op.GetMode()) & 0o777
	if mode == 0 {
		mode = 0o644
	}

	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			if isRootEscape(err) {
				return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
					"seed: path %q resolves outside volume %q", op.GetPath(), op.GetVolume())
			}
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: mkdir: %v", err)
		}
	}

	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return &wsv1.OpResult_SeedVolumeFile{SeedVolumeFile: &wsv1.SeedVolumeFileResult{Written: false}}, nil
		}
		if isRootEscape(err) {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
				"seed: path %q resolves outside volume %q", op.GetPath(), op.GetVolume())
		}
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: create %s: %v", op.GetPath(), err)
	}
	if _, err := f.Write(op.GetContent()); err != nil {
		f.Close()        //nolint:errcheck
		root.Remove(rel) //nolint:errcheck
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: write %s: %v", op.GetPath(), err)
	}
	if err := f.Close(); err != nil {
		root.Remove(rel) //nolint:errcheck
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: close %s: %v", op.GetPath(), err)
	}
	return &wsv1.OpResult_SeedVolumeFile{SeedVolumeFile: &wsv1.SeedVolumeFileResult{Written: true}}, nil
}

// statVolumeFile reports existence only. Content is never returned: the
// file belongs to the application after seeding, and a caller that could
// read it back would inevitably grow a comparison against the original.
func (a *Agent) statVolumeFile(ctx context.Context, op *wsv1.StatVolumeFile) (*wsv1.OpResult_StatVolumeFile, *wsv1.OpError) {
	rel := filepath.ToSlash(op.GetPath())
	if oerr := checkSeedPath(rel); oerr != nil {
		return nil, oerr
	}
	root, oerr := a.volumeRoot(ctx, op.GetVolume())
	if oerr != nil {
		return nil, oerr
	}
	defer root.Close() //nolint:errcheck

	if _, err := root.Lstat(rel); err != nil {
		if errors.Is(err, os.ErrNotExist) || isRootEscape(err) {
			// An escaping path names nothing INSIDE the volume, which is
			// the only place this op speaks about.
			return &wsv1.OpResult_StatVolumeFile{StatVolumeFile: &wsv1.StatVolumeFileResult{Exists: false}}, nil
		}
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: stat %s: %v", op.GetPath(), err)
	}
	return &wsv1.OpResult_StatVolumeFile{StatVolumeFile: &wsv1.StatVolumeFileResult{Exists: true}}, nil
}

// isRootEscape reports whether err is os.Root refusing to leave its root.
// Matched on the sentinel rather than the message.
func isRootEscape(err error) bool {
	return errors.Is(err, os.ErrInvalid) || strings.Contains(err.Error(), "path escapes from parent")
}
