//go:build linux

package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
	"golang.org/x/sys/unix"
)

const mountAccessHelperArg = "__wildstone_mount_access_helper"

const (
	mountAccessExitUnavailable = 20 + iota
	mountAccessExitDenied
	mountAccessExitReadOnly
	mountAccessExitUnsupported
	mountAccessExitCleanupIncomplete
)

var mountAccessTimeout = 5 * time.Second

// Test seam between the effective-identity operations and the final root
// identity recheck. Production leaves it as a no-op.
var mountAccessPostCheck = func() {}
var mountAccessUnlinkat = unix.Unlinkat

// init turns the already-installed wardstoned binary (and the Go test binary)
// into the fixed operation helper. The parent passes only a
// checked root over an inherited pipe plus random sentinel basenames; the host
// path never enters argv or the child environment. The helper opens the root
// only after exec has installed the requested credentials.
func init() {
	if len(os.Args) != 4 || os.Args[1] != mountAccessHelperArg {
		return
	}
	os.Exit(mountAccessHelper(os.Args[2], os.Args[3]))
}

func runMountAccessCheck(ctx context.Context, root string, uid, gid uint32) wsv1.MountAccessFailureReason {
	if len(root) == 0 || len(root) > 4096 || strings.IndexByte(root, 0) >= 0 || !filepath.IsAbs(root) {
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE
	}
	rootReader, rootWriter, err := os.Pipe()
	if err != nil {
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE
	}
	defer rootReader.Close() //nolint:errcheck
	defer rootWriter.Close() //nolint:errcheck

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE
	}
	stem := ".wildstone-access-" + hex.EncodeToString(nonce[:])
	tmpName, doneName := stem+".tmp", stem+".done"

	checkCtx, cancel := context.WithTimeout(ctx, mountAccessTimeout)
	defer cancel()
	// /proc/self/exe avoids passing or resolving an installation path and is
	// executable after credentials are dropped even when the parent binary
	// lives below a root-only directory (as Go test binaries commonly do).
	cmd := exec.CommandContext(checkCtx, "/proc/self/exe", mountAccessHelperArg, tmpName, doneName)
	cmd.ExtraFiles = []*os.File{rootReader}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// An explicit empty group list matches Docker's configured numeric
		// identity. Preserving wardstoned's supplementary groups could let the
		// probe pass with authority the container will not receive.
		Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{}},
		Pdeathsig:  syscall.SIGKILL,
	}
	if err = cmd.Start(); err != nil {
		if checkCtx.Err() != nil {
			return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_TIMEOUT
		}
		return classifyMountAccessErr(err)
	}
	_ = rootReader.Close()
	_, writeErr := io.WriteString(rootWriter, root)
	closeErr := rootWriter.Close()
	if writeErr != nil || closeErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if checkCtx.Err() != nil {
			if !mountAccessCleanup(root, tmpName, doneName) {
				return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_CLEANUP_INCOMPLETE
			}
			return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_TIMEOUT
		}
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE
	}
	err = cmd.Wait()

	if checkCtx.Err() != nil {
		if err == nil {
			// The helper completed its cleanup and identity-side root recheck;
			// the caller's deadline raced only with result delivery.
			return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_TIMEOUT
		}
		if !mountAccessCleanup(root, tmpName, doneName) {
			return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_CLEANUP_INCOMPLETE
		}
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_TIMEOUT
	}
	if err == nil {
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSPECIFIED
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return classifyMountAccessErr(err)
	}
	switch exitErr.ExitCode() {
	case mountAccessExitDenied:
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_IDENTITY_DENIED
	case mountAccessExitReadOnly:
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_READ_ONLY
	case mountAccessExitUnsupported:
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSUPPORTED_BACKEND
	case mountAccessExitCleanupIncomplete:
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_CLEANUP_INCOMPLETE
	default:
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE
	}
}

func mountAccessCleanup(root, tmpName, doneName string) bool {
	fd, err := unix.Openat2(unix.AT_FDCWD, root, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return false
	}
	defer unix.Close(fd) //nolint:errcheck
	return cleanupMountAccessSentinels(fd, tmpName, doneName)
}

func mountAccessHelper(tmpName, doneName string) int {
	if !validSentinelName(tmpName, ".tmp") || !validSentinelName(doneName, ".done") {
		return mountAccessExitUnavailable
	}
	const rootFD = 3
	rootPipe := os.NewFile(rootFD, "mount-access-root-pipe")
	if rootPipe == nil {
		return mountAccessExitUnavailable
	}
	rootBytes, err := io.ReadAll(io.LimitReader(rootPipe, 4097))
	_ = rootPipe.Close()
	root := string(rootBytes)
	if err != nil || len(root) == 0 || len(root) > 4096 || strings.IndexByte(root, 0) >= 0 || !filepath.IsAbs(root) {
		return mountAccessExitUnavailable
	}
	return mountAccessProbe(root, tmpName, doneName)
}

func mountAccessProbe(root, tmpName, doneName string) int {
	rootFD, err := unix.Openat2(unix.AT_FDCWD, root, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return mountAccessExitCode(err)
	}
	defer unix.Close(rootFD) //nolint:errcheck
	var opened unix.Stat_t
	if err = unix.Fstat(rootFD, &opened); err != nil {
		return mountAccessExitUnavailable
	}

	fd, err := unix.Openat(rootFD, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return mountAccessExitCode(err)
	}
	if _, err = unix.Write(fd, []byte("wildstone mount access preflight\n")); err == nil {
		err = unix.Fsync(fd)
	}
	closeErr := unix.Close(fd)
	if err == nil {
		err = closeErr
	}
	if err != nil {
		if !cleanupMountAccessSentinels(rootFD, tmpName) {
			return mountAccessExitCleanupIncomplete
		}
		return mountAccessExitCode(err)
	}
	if err = unix.Renameat2(rootFD, tmpName, rootFD, doneName, unix.RENAME_NOREPLACE); err != nil {
		// doneName was not created by us when RENAME_NOREPLACE fails.
		if !cleanupMountAccessSentinels(rootFD, tmpName) {
			return mountAccessExitCleanupIncomplete
		}
		return mountAccessExitCode(err)
	}
	if err = unix.Fsync(rootFD); err != nil {
		if !cleanupMountAccessSentinels(rootFD, doneName) {
			return mountAccessExitCleanupIncomplete
		}
		return mountAccessExitCode(err)
	}
	if !cleanupMountAccessSentinels(rootFD, doneName) {
		return mountAccessExitCleanupIncomplete
	}
	if err = unix.Fsync(rootFD); err != nil {
		return mountAccessExitCleanupIncomplete
	}
	mountAccessPostCheck()
	if !mountAccessRootUnchanged(root, &opened) {
		return mountAccessExitUnavailable
	}
	return 0
}

func cleanupMountAccessSentinels(rootFD int, names ...string) bool {
	clean := true
	for _, name := range names {
		if err := mountAccessUnlinkat(rootFD, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			clean = false
		}
	}
	return clean
}

func mountAccessRootUnchanged(root string, opened *unix.Stat_t) bool {
	fd, err := unix.Openat2(unix.AT_FDCWD, root, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return false
	}
	defer unix.Close(fd) //nolint:errcheck
	var current unix.Stat_t
	return unix.Fstat(fd, &current) == nil && current.Dev == opened.Dev && current.Ino == opened.Ino
}

func validSentinelName(name, suffix string) bool {
	const prefix = ".wildstone-access-"
	if len(name) != len(prefix)+32+len(suffix) || name[:len(prefix)] != prefix || name[len(name)-len(suffix):] != suffix {
		return false
	}
	_, err := hex.DecodeString(name[len(prefix) : len(prefix)+32])
	return err == nil
}

func mountAccessExitCode(err error) int {
	switch classifyMountAccessErr(err) {
	case wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_IDENTITY_DENIED:
		return mountAccessExitDenied
	case wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_READ_ONLY:
		return mountAccessExitReadOnly
	case wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSUPPORTED_BACKEND:
		return mountAccessExitUnsupported
	default:
		return mountAccessExitUnavailable
	}
}

func classifyMountAccessErr(err error) wsv1.MountAccessFailureReason {
	switch {
	case errors.Is(err, os.ErrPermission), errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_IDENTITY_DENIED
	case errors.Is(err, unix.EROFS):
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_READ_ONLY
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EOPNOTSUPP):
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSUPPORTED_BACKEND
	default:
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE
	}
}
