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
	"strconv"
	"strings"
	"syscall"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
	"golang.org/x/sys/unix"
)

const (
	mountAccessHelperArg = "__wildstone_mount_access_helper"
	mountAccessWorkerArg = "__wildstone_mount_access_worker"

	mountAccessActionProbe   = "probe"
	mountAccessActionCleanup = "cleanup"
	mountAccessRoleRoot      = "root"
	mountAccessRoleIdentity  = "identity"
	mountAccessRoleFD        = "fd"
)

const (
	mountAccessExitUnavailable = 20 + iota
	mountAccessExitDenied
	mountAccessExitReadOnly
	mountAccessExitUnsupported
	mountAccessExitCleanupIncomplete
	mountAccessExitRetryIdentity
)

var mountAccessTimeout = 5 * time.Second
var mountAccessCleanupTimeout = 5 * time.Second

// Test seam between the effective-identity operations and the final root
// identity recheck. Production leaves it as a no-op.
var mountAccessPostCheck = func() {}
var mountAccessUnlinkat = unix.Unlinkat

// init turns the already-installed wardstoned binary (and the Go test binary)
// into fixed supervisor and worker helpers. The host path travels only over an
// inherited pipe. The supervisor never touches the filesystem: a root worker
// anchors paths hidden below host-only ancestors, while an identity worker is
// the fallback for root-squashed storage. Blocking lookups therefore remain in
// a worker process that dies with the bounded supervisor.
func init() {
	if len(os.Args) == 7 && os.Args[1] == mountAccessHelperArg {
		uid, uidErr := strconv.ParseUint(os.Args[5], 10, 32)
		gid, gidErr := strconv.ParseUint(os.Args[6], 10, 32)
		if uidErr != nil || gidErr != nil {
			os.Exit(mountAccessExitUnavailable)
		}
		os.Exit(mountAccessSupervisor(os.Args[2], os.Args[3], os.Args[4], uint32(uid), uint32(gid)))
	}
	if len(os.Args) == 8 && os.Args[1] == mountAccessWorkerArg {
		uid, uidErr := strconv.ParseUint(os.Args[6], 10, 32)
		gid, gidErr := strconv.ParseUint(os.Args[7], 10, 32)
		if uidErr != nil || gidErr != nil {
			os.Exit(mountAccessExitUnavailable)
		}
		os.Exit(mountAccessWorker(os.Args[2], os.Args[3], os.Args[4], os.Args[5], uint32(uid), uint32(gid)))
	}
}

func runMountAccessCheck(ctx context.Context, root string, uid, gid uint32) wsv1.MountAccessFailureReason {
	if !validMountAccessRoot(root) {
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
	exit, timedOut := runMountAccessSupervisor(checkCtx, mountAccessActionProbe, root, tmpName, doneName, uid, gid)
	if timedOut {
		// Cancellation may have killed the worker between create and unlink.
		// Cleanup gets its own bounded supervisor because the caller's context
		// is already expired. No filesystem operation runs in wardstoned.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), mountAccessCleanupTimeout)
		defer cleanupCancel()
		cleanupExit, cleanupTimedOut := runMountAccessSupervisor(cleanupCtx, mountAccessActionCleanup, root, tmpName, doneName, uid, gid)
		if cleanupTimedOut || cleanupExit != 0 {
			return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_CLEANUP_INCOMPLETE
		}
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_TIMEOUT
	}
	return mountAccessReason(exit)
}

func runMountAccessSupervisor(ctx context.Context, action, root, tmpName, doneName string, uid, gid uint32) (int, bool) {
	rootReader, rootWriter, err := os.Pipe()
	if err != nil {
		return mountAccessExitUnavailable, false
	}
	defer rootReader.Close() //nolint:errcheck
	defer rootWriter.Close() //nolint:errcheck

	cmd := exec.Command("/proc/self/exe", mountAccessHelperArg, action, tmpName, doneName, strconv.FormatUint(uint64(uid), 10), strconv.FormatUint(uint64(gid), 10))
	cmd.ExtraFiles = []*os.File{rootReader}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err = cmd.Start(); err != nil {
		return mountAccessExitCode(err), false
	}
	_ = rootReader.Close()
	_, writeErr := io.WriteString(rootWriter, root)
	closeErr := rootWriter.Close()
	if writeErr != nil || closeErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if ctx.Err() != nil {
			return mountAccessExitUnavailable, true
		}
		return mountAccessExitUnavailable, false
	}

	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err = <-wait:
		return processExitCode(err), false
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		// The supervisor performs no filesystem syscalls, so it should exit
		// immediately even when its worker is stuck in an uninterruptible mount.
		// Keep the caller bounded if the platform nevertheless delays reaping it.
		select {
		case <-wait:
		case <-time.After(time.Second):
		}
		return mountAccessExitUnavailable, true
	}
}

func mountAccessSupervisor(action, tmpName, doneName string, uid, gid uint32) int {
	root, ok := readMountAccessRoot(3)
	if !ok || !validMountAccessAction(action) || !validSentinelName(tmpName, ".tmp") || !validSentinelName(doneName, ".done") {
		return mountAccessExitUnavailable
	}
	exit := startMountAccessWorker(action, mountAccessRoleRoot, root, tmpName, doneName, uid, gid, nil)
	if exit == mountAccessExitRetryIdentity {
		exit = startMountAccessWorker(action, mountAccessRoleIdentity, root, tmpName, doneName, uid, gid, mountAccessCredential(action, uid, gid))
	}
	return exit
}

func startMountAccessWorker(action, role, root, tmpName, doneName string, uid, gid uint32, credential *syscall.Credential) int {
	rootReader, rootWriter, err := os.Pipe()
	if err != nil {
		return mountAccessExitUnavailable
	}
	defer rootReader.Close() //nolint:errcheck
	defer rootWriter.Close() //nolint:errcheck
	cmd := exec.Command("/proc/self/exe", mountAccessWorkerArg, action, role, tmpName, doneName, strconv.FormatUint(uint64(uid), 10), strconv.FormatUint(uint64(gid), 10))
	cmd.ExtraFiles = []*os.File{rootReader}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential, Pdeathsig: syscall.SIGKILL}
	if err = cmd.Start(); err != nil {
		return mountAccessExitCode(err)
	}
	_ = rootReader.Close()
	_, writeErr := io.WriteString(rootWriter, root)
	closeErr := rootWriter.Close()
	if writeErr != nil || closeErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return mountAccessExitUnavailable
	}
	return processExitCode(cmd.Wait())
}

func mountAccessWorker(action, role, tmpName, doneName string, uid, gid uint32) int {
	if !validMountAccessAction(action) || !validSentinelName(tmpName, ".tmp") || !validSentinelName(doneName, ".done") {
		return mountAccessExitUnavailable
	}
	if role == mountAccessRoleFD {
		return mountAccessOnFD(action, 3, tmpName, doneName)
	}
	root, ok := readMountAccessRoot(3)
	if !ok {
		return mountAccessExitUnavailable
	}
	if role == mountAccessRoleIdentity {
		return mountAccessOnPath(action, root, tmpName, doneName)
	}
	if role != mountAccessRoleRoot {
		return mountAccessExitUnavailable
	}
	return mountAccessRootWorker(action, root, tmpName, doneName, uid, gid)
}

func mountAccessRootWorker(action, root, tmpName, doneName string, uid, gid uint32) int {
	rootFD, err := unix.Openat2(unix.AT_FDCWD, root, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
			return mountAccessExitRetryIdentity
		}
		return mountAccessExitCode(err)
	}
	rootFile := os.NewFile(uintptr(rootFD), "mount-access-root")
	if rootFile == nil {
		_ = unix.Close(rootFD)
		return mountAccessExitUnavailable
	}
	defer rootFile.Close() //nolint:errcheck
	var opened unix.Stat_t
	if err = unix.Fstat(rootFD, &opened); err != nil {
		return mountAccessExitUnavailable
	}

	cmd := exec.Command("/proc/self/exe", mountAccessWorkerArg, action, mountAccessRoleFD, tmpName, doneName, strconv.FormatUint(uint64(uid), 10), strconv.FormatUint(uint64(gid), 10))
	cmd.ExtraFiles = []*os.File{rootFile}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: mountAccessCredential(action, uid, gid), Pdeathsig: syscall.SIGKILL}
	exit := processExitCode(cmd.Run())
	if exit == 0 && action == mountAccessActionProbe && !mountAccessRootUnchanged(root, &opened) {
		return mountAccessExitUnavailable
	}
	return exit
}

func mountAccessCredential(action string, uid, gid uint32) *syscall.Credential {
	// A non-root test process cannot call setgroups, even to retain its own
	// identity. Cleanup may inherit that already-matching identity because it
	// only removes collision-resistant sentinels; probes always install the
	// explicit empty group set that the container receives.
	if action == mountAccessActionCleanup && os.Geteuid() == int(uid) && os.Getegid() == int(gid) {
		return nil
	}
	return &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{}}
}

func mountAccessOnPath(action, root, tmpName, doneName string) int {
	rootFD, err := unix.Openat2(unix.AT_FDCWD, root, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return mountAccessExitCode(err)
	}
	defer unix.Close(rootFD) //nolint:errcheck
	var opened unix.Stat_t
	if err = unix.Fstat(rootFD, &opened); err != nil {
		return mountAccessExitUnavailable
	}
	exit := mountAccessOnFD(action, rootFD, tmpName, doneName)
	if exit == 0 && action == mountAccessActionProbe && !mountAccessRootUnchanged(root, &opened) {
		return mountAccessExitUnavailable
	}
	return exit
}

func mountAccessOnFD(action string, rootFD int, tmpName, doneName string) int {
	if action == mountAccessActionCleanup {
		if cleanupMountAccessSentinels(rootFD, tmpName, doneName) {
			return 0
		}
		return mountAccessExitCleanupIncomplete
	}
	return mountAccessProbeFD(rootFD, tmpName, doneName)
}

func mountAccessProbe(root, tmpName, doneName string) int {
	return mountAccessOnPath(mountAccessActionProbe, root, tmpName, doneName)
}

func mountAccessProbeFD(rootFD int, tmpName, doneName string) int {
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
	fd, err := unix.Openat2(unix.AT_FDCWD, root, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return false
	}
	defer unix.Close(fd) //nolint:errcheck
	var current unix.Stat_t
	return unix.Fstat(fd, &current) == nil && current.Dev == opened.Dev && current.Ino == opened.Ino
}

func readMountAccessRoot(fd int) (string, bool) {
	rootPipe := os.NewFile(uintptr(fd), "mount-access-root-pipe")
	if rootPipe == nil {
		return "", false
	}
	rootBytes, err := io.ReadAll(io.LimitReader(rootPipe, 4097))
	_ = rootPipe.Close()
	root := string(rootBytes)
	return root, err == nil && validMountAccessRoot(root)
}

func validMountAccessRoot(root string) bool {
	return len(root) > 0 && len(root) <= 4096 && strings.IndexByte(root, 0) < 0 && filepath.IsAbs(root)
}

func validMountAccessAction(action string) bool {
	return action == mountAccessActionProbe || action == mountAccessActionCleanup
}

func validSentinelName(name, suffix string) bool {
	const prefix = ".wildstone-access-"
	if len(name) != len(prefix)+32+len(suffix) || name[:len(prefix)] != prefix || name[len(name)-len(suffix):] != suffix {
		return false
	}
	_, err := hex.DecodeString(name[len(prefix) : len(prefix)+32])
	return err == nil
}

func processExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return mountAccessExitCode(err)
}

func mountAccessReason(exit int) wsv1.MountAccessFailureReason {
	switch exit {
	case 0:
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSPECIFIED
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
