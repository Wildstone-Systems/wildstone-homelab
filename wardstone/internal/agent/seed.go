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
//   - The path is confined to the volume, checked after symlink
//     resolution of the parent, so a symlink planted inside the volume
//     cannot redirect a write onto the host.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// volumePath resolves (volume, relative path) to an absolute host path
// that is guaranteed to sit inside the volume.
//
// It takes the relative path apart rather than trusting it: an absolute
// path, an empty path, or any ".." element is refused outright, and the
// result is then re-checked against the volume root after resolving
// symlinks on the parent directory. The first check is about what the
// caller asked for; the second is about what the filesystem would
// actually do, and only the second one survives an attacker who can
// create a symlink inside the volume.
func (a *Agent) volumePath(ctx context.Context, volume, rel string) (string, *wsv1.OpError) {
	if volume == "" {
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "seed: no volume named")
	}
	if rel == "" {
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "seed: no path named")
	}
	if filepath.IsAbs(rel) {
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "seed: path %q must be relative to the volume", rel)
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == "." || seg == ".." || seg == "" {
			return "", opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "seed: path %q must name a file inside the volume", rel)
		}
	}

	vol, err := a.docker.volumeInspect(ctx, volume)
	if err != nil {
		if derr, ok := err.(*dockerError); ok && derr.notFound() {
			return "", opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "seed: volume %q does not exist", volume)
		}
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: volume inspect: %v", err)
	}
	// Only ever seed a volume Wildstone created. Seeding writes into data
	// we would otherwise never touch; refusing an adopted volume keeps a
	// package from reaching into something a person set up by hand.
	if vol.Labels[OwnerLabel] != ownerValue {
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"seed: volume %q exists but is not wildstone-owned", volume)
	}
	root := vol.Mountpoint
	if root == "" {
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: volume %q reports no mountpoint", volume)
	}

	full := filepath.Join(root, filepath.FromSlash(rel))

	// Re-check after resolving the PARENT's symlinks. The leaf itself is
	// intentionally not resolved: it is expected not to exist, and if it
	// does exist as a symlink the O_EXCL create refuses it anyway.
	parent := filepath.Dir(full)
	resolvedParent, rerr := filepath.EvalSymlinks(parent)
	if rerr != nil {
		if !errors.Is(rerr, os.ErrNotExist) {
			return "", opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: resolve %q: %v", parent, rerr)
		}
		// Parent does not exist yet; the join above already refused "..",
		// and mkdir below creates it inside the volume.
		resolvedParent = parent
	}
	resolvedRoot, rerr := filepath.EvalSymlinks(root)
	if rerr != nil {
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: resolve volume root: %v", rerr)
	}
	if resolvedParent != resolvedRoot && !strings.HasPrefix(resolvedParent, resolvedRoot+string(os.PathSeparator)) {
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"seed: path %q resolves outside volume %q", rel, volume)
	}
	return full, nil
}

// seedVolumeFile writes the file only if it is not already there.
//
// "Already there" is reported as written=false and NOT as an error: after
// the first tick that is the permanent steady state, and a reconciler
// that treated it as a failure would set the resource aside forever.
func (a *Agent) seedVolumeFile(ctx context.Context, op *wsv1.SeedVolumeFile) (*wsv1.OpResult_SeedVolumeFile, *wsv1.OpError) {
	full, oerr := a.volumePath(ctx, op.GetVolume(), op.GetPath())
	if oerr != nil {
		return nil, oerr
	}

	// Same masking as writeManagedFile: a caller-supplied mode must never
	// carry setuid/setgid/sticky.
	mode := os.FileMode(op.GetMode()) & 0o777
	if mode == 0 {
		mode = 0o644
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: mkdir: %v", err)
	}

	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return &wsv1.OpResult_SeedVolumeFile{SeedVolumeFile: &wsv1.SeedVolumeFileResult{Written: false}}, nil
		}
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: create %s: %v", op.GetPath(), err)
	}
	if _, err := f.Write(op.GetContent()); err != nil {
		f.Close()       //nolint:errcheck
		os.Remove(full) //nolint:errcheck
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: write %s: %v", op.GetPath(), err)
	}
	if err := f.Close(); err != nil {
		os.Remove(full) //nolint:errcheck
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: close %s: %v", op.GetPath(), err)
	}
	return &wsv1.OpResult_SeedVolumeFile{SeedVolumeFile: &wsv1.SeedVolumeFileResult{Written: true}}, nil
}

// statVolumeFile reports existence only. Content is never returned: the
// file belongs to the application after seeding, and a caller that could
// read it back would inevitably grow a comparison against the original.
func (a *Agent) statVolumeFile(ctx context.Context, op *wsv1.StatVolumeFile) (*wsv1.OpResult_StatVolumeFile, *wsv1.OpError) {
	full, oerr := a.volumePath(ctx, op.GetVolume(), op.GetPath())
	if oerr != nil {
		return nil, oerr
	}
	_, err := os.Lstat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &wsv1.OpResult_StatVolumeFile{StatVolumeFile: &wsv1.StatVolumeFileResult{Exists: false}}, nil
		}
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "seed: stat %s: %v", op.GetPath(), err)
	}
	return &wsv1.OpResult_StatVolumeFile{StatVolumeFile: &wsv1.StatVolumeFileResult{Exists: true}}, nil
}
