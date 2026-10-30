//go:build linux

package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
	"golang.org/x/sys/unix"
)

const (
	mountAccessHelperArg = "__wildstone_mount_access_helper"
	mountAccessWorkerArg = "__wildstone_mount_access_worker"

	mountAccessActionProbe     = "probe"
	mountAccessActionCleanup   = "cleanup"
	mountAccessActionAdmission = "admission"
	mountAccessRoleRoot        = "root"
	mountAccessRoleIdentity    = "identity"
	mountAccessRoleFD          = "fd"
)

const (
	mountAccessExitUnavailable = 20 + iota
	mountAccessExitDenied
	mountAccessExitReadOnly
	mountAccessExitUnsupported
	mountAccessExitCleanupIncomplete
	mountAccessExitRetryIdentity
	mountAccessExitUnconfirmed
)

var mountAccessCleanupTimeout = 5 * time.Second

// Capacity belongs to a supervisor until its entire worker tree is reaped,
// including after the caller has returned. Four trees mean at most twelve
// processes (supervisor, root worker and identity worker per tree).
var mountAccessSlots = make(chan struct{}, 4)
var mountAccessReapGrace = 100 * time.Millisecond

// Only tests replace the launcher; the production command is fixed.
var mountAccessSupervisorCommand = func(action, tmpName, doneName string, uid, gid uint32) *exec.Cmd {
	return exec.Command("/proc/self/exe", mountAccessHelperArg, action, tmpName, doneName, strconv.FormatUint(uint64(uid), 10), strconv.FormatUint(uint64(gid), 10))
}

type mountAccessRequest struct {
	Root            string
	ManagedPrefixes []string
	DataDir         string
}

func runMountAccessAdmission(ctx context.Context, root string, cfg Config) wsv1.MountAccessFailureReason {
	// Admission performs the same existing confinement checks, but in a worker:
	// EvalSymlinks and even a stat of an unavailable bind source can block.
	const stem = ".wildstone-access-00000000000000000000000000000000"
	exit, timedOut, _ := runMountAccessSupervisor(ctx, mountAccessActionAdmission,
		mountAccessRequest{Root: root, ManagedPrefixes: cfg.ManagedPrefixes, DataDir: cfg.DataDir}, stem+".tmp", stem+".done", 0, 0)
	if timedOut {
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_TIMEOUT
	}
	return mountAccessReason(exit)
}

// Test seam between the effective-identity operations and the final root
// identity recheck. Production leaves it as a no-op.
var mountAccessPostCheck = func() {}
var mountAccessUnlinkat = unix.Unlinkat

// init turns the already-installed wardstoned binary (and the Go test binary)
// into fixed supervisor and worker helpers. The host path travels only over an
// inherited pipe. The supervisor never touches the filesystem: a root worker
// anchors paths hidden below host-only ancestors, while an identity worker is
// the fallback for root-squashed storage. Blocking lookups therefore remain in
// a worker process. The supervisor stays alive until every descendant is reaped;
// cancellation requests termination rather than claiming immediate exit.
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
	exit, timedOut, reaped := runMountAccessSupervisor(checkCtx, mountAccessActionProbe, mountAccessRequest{Root: root}, tmpName, doneName, uid, gid)
	if timedOut || !reaped {
		// Cancellation may have killed the worker between create and unlink.
		// Cleanup gets its own bounded supervisor because the caller's context
		// is already expired. No filesystem operation runs in wardstoned.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), mountAccessCleanupTimeout)
		defer cleanupCancel()
		cleanupExit, cleanupTimedOut, _ := runMountAccessSupervisor(cleanupCtx, mountAccessActionCleanup, mountAccessRequest{Root: root}, tmpName, doneName, uid, gid)
		if !reaped || cleanupTimedOut || cleanupExit != 0 {
			return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_CLEANUP_INCOMPLETE
		}
		return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_TIMEOUT
	}
	return mountAccessReason(exit)
}

// runMountAccessSupervisor bounds the caller, not the lifetime of Linux I/O.
// Closing fd 4 requests cancellation. The dedicated subreaper stays alive and
// holds its admission slot until all descendants really exit, even if SIGKILL
// remains pending on a worker in uninterruptible filesystem I/O.
func runMountAccessSupervisor(ctx context.Context, action string, request mountAccessRequest, tmpName, doneName string, uid, gid uint32) (int, bool, bool) {
	if ctx.Err() != nil {
		return mountAccessExitUnavailable, true, true
	}
	slots := mountAccessSlots
	select {
	case slots <- struct{}{}:
	default:
		return mountAccessExitUnavailable, false, true
	}
	started := false
	defer func() {
		if !started {
			<-slots
		}
	}()
	payload, err := json.Marshal(request)
	if err != nil || len(payload) > 65536 {
		return mountAccessExitUnavailable, false, true
	}
	rootReader, rootWriter, err := os.Pipe()
	if err != nil {
		return mountAccessExitUnavailable, false, true
	}
	defer rootReader.Close() //nolint:errcheck
	defer rootWriter.Close() //nolint:errcheck
	cancelReader, cancelWriter, err := os.Pipe()
	if err != nil {
		return mountAccessExitUnavailable, false, true
	}
	defer cancelReader.Close() //nolint:errcheck
	defer cancelWriter.Close() //nolint:errcheck
	cmd := mountAccessSupervisorCommand(action, tmpName, doneName, uid, gid)
	cmd.ExtraFiles = append([]*os.File{rootReader, cancelReader}, cmd.ExtraFiles...)
	// No parent-death SIGKILL: pipe EOF must let the subreaper kill and reap
	// its workers after daemon exit, too.
	if err = cmd.Start(); err != nil {
		return mountAccessExitCode(err), false, true
	}
	started = true
	_ = rootReader.Close()
	_ = cancelReader.Close()
	go func() {
		_, _ = rootWriter.Write(payload)
		_ = rootWriter.Close()
	}()
	wait := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		_ = rootWriter.Close()
		// A supervisor killed externally may not have reaped its descendants.
		// Retain capacity conservatively when its completion is unconfirmed.
		if mountAccessSupervisorExitConfirmed(err) {
			<-slots
		}
		wait <- err
	}()
	select {
	case err = <-wait:
		if ctx.Err() != nil {
			return mountAccessExitUnavailable, true, mountAccessSupervisorExitConfirmed(err)
		}
		return processExitCode(err), false, mountAccessSupervisorExitConfirmed(err)
	case <-ctx.Done():
		_ = cancelWriter.Close()
		timer := time.NewTimer(mountAccessReapGrace)
		defer timer.Stop()
		select {
		case err := <-wait:
			return mountAccessExitUnavailable, true, mountAccessSupervisorExitConfirmed(err)
		case <-timer.C:
			return mountAccessExitUnavailable, true, false
		}
	}
}

// Only the fixed supervisor's closed exit statuses confirm that it reached
// its reaping barrier (or refused the request before launching any worker).
func mountAccessSupervisorExitConfirmed(err error) bool {
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	code := exitErr.ExitCode()
	return code >= mountAccessExitUnavailable && code <= mountAccessExitCleanupIncomplete
}

type mountAccessWorkerTree struct {
	mu       sync.Mutex
	canceled bool
	pid      int
}

func (tree *mountAccessWorkerTree) cancel() {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	tree.canceled = true
	if tree.pid != 0 {
		_ = unix.Kill(-tree.pid, unix.SIGKILL)
	}
}

func mountAccessSupervisor(action, tmpName, doneName string, uid, gid uint32) int {
	// This is a disposable process, so subreaping changes no daemon behavior.
	if unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) != nil {
		return mountAccessExitUnsupported
	}
	tree := &mountAccessWorkerTree{}
	cancelPipe := os.NewFile(4, "mount-access-cancel")
	if cancelPipe == nil {
		return mountAccessExitUnavailable
	}
	defer cancelPipe.Close() //nolint:errcheck
	unix.CloseOnExec(4)
	go func() {
		var one [1]byte
		_, _ = cancelPipe.Read(one[:]) // cancellation or parent exit closes the pipe
		tree.cancel()
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	go func() { <-signals; tree.cancel() }()
	request, ok := readMountAccessRequest(3)
	if !ok || !validMountAccessAction(action) || !validSentinelName(tmpName, ".tmp") || !validSentinelName(doneName, ".done") {
		return mountAccessExitUnavailable
	}
	exit := tree.start(action, mountAccessRoleRoot, request, tmpName, doneName, uid, gid, nil)
	if exit == mountAccessExitRetryIdentity {
		exit = tree.start(action, mountAccessRoleIdentity, request, tmpName, doneName, uid, gid, mountAccessCredential(action, uid, gid))
	}
	return reapMountAccessDescendants(exit)
}

func reapMountAccessDescendants(exit int) int {
	// cmd.Wait reaped the direct worker. A killed root worker may have left a
	// descriptor worker behind; subreaping lets us wait for that actual exit.
	for {
		_, err := unix.Wait4(-1, nil, 0, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ECHILD) {
			if exit != 0 && (exit < mountAccessExitUnavailable || exit > mountAccessExitCleanupIncomplete) {
				return mountAccessExitUnavailable
			}
			return exit
		}
		if err != nil {
			return mountAccessExitUnconfirmed
		}
	}
}

func (tree *mountAccessWorkerTree) start(action, role string, request mountAccessRequest, tmpName, doneName string, uid, gid uint32, credential *syscall.Credential) int {
	rootReader, rootWriter, err := os.Pipe()
	if err != nil {
		return mountAccessExitUnavailable
	}
	defer rootReader.Close() //nolint:errcheck
	defer rootWriter.Close() //nolint:errcheck
	cmd := exec.Command("/proc/self/exe", mountAccessWorkerArg, action, role, tmpName, doneName, strconv.FormatUint(uint64(uid), 10), strconv.FormatUint(uint64(gid), 10))
	cmd.ExtraFiles = []*os.File{rootReader}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential, Pdeathsig: syscall.SIGKILL, Setpgid: true}
	tree.mu.Lock()
	if tree.canceled {
		tree.mu.Unlock()
		return mountAccessExitUnavailable
	}
	err = cmd.Start()
	if err == nil {
		tree.pid = cmd.Process.Pid
	}
	tree.mu.Unlock()
	if err != nil {
		return mountAccessExitCode(err)
	}
	_ = rootReader.Close()
	// The request is bounded and written while the worker is supervised; on
	// cancellation the killed worker closes its reader and unblocks this pipe.
	payload, err := json.Marshal(request)
	if err == nil {
		_, err = rootWriter.Write(payload)
	}
	_ = rootWriter.Close()
	if err != nil {
		tree.cancel()
	}
	exit := processExitCode(cmd.Wait())
	tree.mu.Lock()
	tree.pid = 0
	tree.mu.Unlock()
	return exit
}

func mountAccessWorker(action, role, tmpName, doneName string, uid, gid uint32) int {
	if !validMountAccessAction(action) || !validSentinelName(tmpName, ".tmp") || !validSentinelName(doneName, ".done") {
		return mountAccessExitUnavailable
	}
	if role == mountAccessRoleFD {
		return mountAccessOnFD(action, 3, tmpName, doneName)
	}
	request, ok := readMountAccessRequest(3)
	root := request.Root
	if !ok {
		return mountAccessExitUnavailable
	}
	if action == mountAccessActionAdmission {
		if role != mountAccessRoleRoot {
			return mountAccessExitUnavailable
		}
		a := &Agent{cfg: Config{ManagedPrefixes: request.ManagedPrefixes, DataDir: request.DataDir}}
		if _, err := a.resolveManaged(root); err != nil {
			return mountAccessExitUnavailable
		}
		if err := a.checkBindSource(root); err != nil {
			return mountAccessExitUnavailable
		}
		return 0
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

func readMountAccessRequest(fd int) (mountAccessRequest, bool) {
	pipe := os.NewFile(uintptr(fd), "mount-access-request")
	if pipe == nil {
		return mountAccessRequest{}, false
	}
	defer pipe.Close() //nolint:errcheck
	payload, err := io.ReadAll(io.LimitReader(pipe, 65537))
	var request mountAccessRequest
	if err != nil || len(payload) > 65536 || json.Unmarshal(payload, &request) != nil {
		return request, false
	}
	return request, validMountAccessRoot(request.Root)
}

func validMountAccessRoot(root string) bool {
	return len(root) > 0 && len(root) <= 4096 && strings.IndexByte(root, 0) < 0 && filepath.IsAbs(root)
}

func validMountAccessAction(action string) bool {
	return action == mountAccessActionProbe || action == mountAccessActionCleanup || action == mountAccessActionAdmission
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
