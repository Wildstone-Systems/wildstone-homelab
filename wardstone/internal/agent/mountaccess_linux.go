//go:build linux

package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
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
)

var mountAccessTimeout = 5 * time.Second

// Test seam between the effective-identity operations and the final root
// identity recheck. Production leaves it as a no-op.
var mountAccessPostCheck = func() {}
var mountAccessUnlinkat = unix.Unlinkat

// init turns the already-installed wardstoned binary (and the Go test binary)
// into the fixed operation helper. The parent passes only a
// directory descriptor and random sentinel basenames; the checked host path
// never enters argv or the child environment.
func init() {
	if len(os.Args) != 4 || os.Args[1] != mountAccessHelperArg {
		return
	}
	os.Exit(mountAccessHelper(os.Args[2], os.Args[3]))
}

func runMountAccessCheck(ctx context.Context, root string, uid, gid uint32) wsv1.MountAccessFailureReason {
	fd, err := unix.Openat2(unix.AT_FDCWD, root, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return classifyMountAccessErr(err)
	}
	rootFile := os.NewFile(uintptr(fd), "mount-access-root")
	if rootFile == nil {
		_ = unix.Close(fd)
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE
	}
	defer rootFile.Close() //nolint:errcheck
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE
	}

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
	cmd.ExtraFiles = []*os.File{rootFile}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid, NoSetGroups: true},
		Pdeathsig:  syscall.SIGKILL,
	}
	err = cmd.Run()

	cleanupOK := true
	for _, name := range []string{tmpName, doneName} {
		if cleanupErr := mountAccessUnlinkat(fd, name, 0); cleanupErr != nil && !errors.Is(cleanupErr, unix.ENOENT) {
			cleanupOK = false
		}
	}
	if !cleanupOK {
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_CLEANUP_INCOMPLETE
	}
	mountAccessPostCheck()
	if !mountAccessRootUnchanged(root, &opened) {
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE
	}
	if checkCtx.Err() != nil {
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
	default:
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE
	}
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

func mountAccessHelper(tmpName, doneName string) int {
	if !validSentinelName(tmpName, ".tmp") || !validSentinelName(doneName, ".done") {
		return mountAccessExitUnavailable
	}
	const rootFD = 3
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
		_ = unix.Unlinkat(rootFD, tmpName, 0)
		return mountAccessExitCode(err)
	}
	if err = unix.Renameat2(rootFD, tmpName, rootFD, doneName, unix.RENAME_NOREPLACE); err != nil {
		_ = unix.Unlinkat(rootFD, tmpName, 0)
		return mountAccessExitCode(err)
	}
	if err = unix.Fsync(rootFD); err == nil {
		err = unix.Unlinkat(rootFD, doneName, 0)
	}
	if err == nil {
		err = unix.Fsync(rootFD)
	}
	if err != nil {
		return mountAccessExitCode(err)
	}
	return 0
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
