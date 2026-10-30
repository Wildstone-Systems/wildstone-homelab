//go:build linux

package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
	"golang.org/x/sys/unix"
)

func mountAccessHarness(t *testing.T, volume, root string) (*fakeDocker, *harness) {
	t.Helper()
	fake := newFakeDocker(t)
	fake.addOwnedVolumeAt(volume, root)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	return fake, h
}

func mountAccessEnsure(name, hash, volume string, uid, gid int64, preflight bool) *wsv1.Operation {
	return &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{EnsureContainer: &wsv1.EnsureContainer{
		Name: name, Image: testDigestImage,
		Labels:               map[string]string{SpecHashLabel: hash},
		Mounts:               []*wsv1.Mount{{Source: volume, Target: "/library"}},
		RuntimeIdentity:      &wsv1.ContainerRuntimeIdentity{Uid: uid, Gid: &gid},
		PreflightMountAccess: preflight,
	}}}
}

func TestMountAccessPreflightUsesEffectiveIdentityAndLeavesCurrentWorkload(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to install an explicitly empty supplementary-group set")
	}
	root := t.TempDir()
	fake, h := mountAccessHarness(t, "library", root)
	uid, gid := int64(os.Getuid()), int64(os.Getgid())

	if res := dispatch(t, h, "node-a", mountAccessEnsure("app", "v1", "library", uid, gid, true)); res.GetError() != nil {
		t.Fatalf("writable mount refused: %+v", res.GetError())
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("sentinel cleanup: entries=%v err=%v", entries, err)
	}

	// A different effective identity cannot traverse this root. The changed
	// hash would replace the running container if the check happened too late.
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	deniedUID, deniedGID := int64(65534), int64(65534)
	res := dispatch(t, h, "node-a", mountAccessEnsure("app", "v2", "library", deniedUID, deniedGID, true))
	oe := res.GetError()
	if oe == nil || oe.GetMountAccessFailure().GetReason() != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_IDENTITY_DENIED {
		t.Fatalf("denied identity result = %+v", oe)
	}
	if strings.Contains(oe.GetMessage(), root) {
		t.Fatalf("preflight exposed its source: %q", oe.GetMessage())
	}
	fake.mu.Lock()
	current := fake.containers["app"]
	fake.mu.Unlock()
	if current == nil || !current.running || current.labels[SpecHashLabel] != "v1" {
		t.Fatalf("failed preflight replaced the current workload: %+v", current)
	}

	if err := os.Chmod(filepath.Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(root, int(deniedUID), int(deniedGID)); err != nil {
		t.Fatal(err)
	}
	if res = dispatch(t, h, "node-a", mountAccessEnsure("app", "v2", "library", deniedUID, deniedGID, true)); res.GetError() != nil {
		t.Fatalf("safe retry after permission correction: %+v", res.GetError())
	}
}

func TestMountAccessPreflightClearsAgentSupplementaryGroups(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to install a different child identity")
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	const targetID = 65534
	group := -1
	for _, candidate := range groups {
		if candidate != targetID {
			group = candidate
			break
		}
	}
	if group < 0 {
		t.Skip("agent has no distinct supplementary group")
	}
	root := t.TempDir()
	if err := os.Chmod(filepath.Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(root, 0, group); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o770); err != nil {
		t.Fatal(err)
	}
	if got := runMountAccessCheck(context.Background(), root, targetID, targetID); got != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_IDENTITY_DENIED {
		t.Fatalf("supplementary-group-only access = %v, want identity denied", got)
	}
}

func TestMountAccessPreflightOpensRootAsRequestedIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create an identity-owned fixture")
	}
	const targetID = 65534
	root := t.TempDir()
	if err := os.Chmod(filepath.Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(root, targetID, targetID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := runMountAccessCheck(context.Background(), root, targetID, targetID); got != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSPECIFIED {
		t.Fatalf("identity-owned root = %v, want success", got)
	}
}

func TestMountAccessPreflightDoesNotRequireIdentityAccessToHostAncestors(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create a host-only ancestor and identity-owned root")
	}
	const targetID = 65534
	hostOnly := t.TempDir()
	root := filepath.Join(hostOnly, "volume-data")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(root, targetID, targetID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hostOnly, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := runMountAccessCheck(context.Background(), root, targetID, targetID); got != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSPECIFIED {
		t.Fatalf("identity-owned root below host-only ancestor = %v, want success", got)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("sentinel cleanup: entries=%v err=%v", entries, err)
	}
}

func TestMountAccessPreflightReportsOnlyTargetAndClosedReason(t *testing.T) {
	root := t.TempDir()
	_, h := mountAccessHarness(t, "present", root)
	uid, gid := int64(os.Getuid()), int64(os.Getgid())
	op := mountAccessEnsure("app", "v1", "private-source-name", uid, gid, true)
	res := dispatch(t, h, "node-a", op)
	oe := res.GetError()
	if oe == nil || oe.GetMountAccessFailure().GetTarget() != "/library" ||
		oe.GetMountAccessFailure().GetReason() != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE {
		t.Fatalf("missing mount result = %+v", oe)
	}
	for _, forbidden := range []string{"private-source-name", root, "no such", "volume inspect"} {
		if strings.Contains(strings.ToLower(oe.GetMessage()), strings.ToLower(forbidden)) {
			t.Errorf("error %q exposed %q", oe.GetMessage(), forbidden)
		}
	}
}

func TestMountAccessPreflightDoesNotTouchDeclaredReadOnlyMount(t *testing.T) {
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	uid, gid := int64(os.Getuid()), int64(os.Getgid())
	op := mountAccessEnsure("app", "v1", "absent-read-only", uid, gid, true)
	op.GetEnsureContainer().Mounts[0].ReadOnly = true
	if res := dispatch(t, h, "node-a", op); res.GetError() != nil {
		t.Fatalf("read-only mount was probed: %+v", res.GetError())
	}
}

func TestMountAccessPreflightRefusesSymlinkEscapeAndTimesOutCleanly(t *testing.T) {
	managed, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(managed, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.DockerEndpoint = fake.URL()
		c.ManagedPrefixes = []string{managed}
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	uid, gid := int64(os.Getuid()), int64(os.Getgid())
	op := mountAccessEnsure("escape", "v1", link, uid, gid, true)
	if oe := dispatch(t, h, "node-a", op).GetError(); oe == nil || oe.GetMountAccessFailure().GetReason() != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE {
		t.Fatalf("symlink escape result = %+v", oe)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("symlink escape touched outside root: %v", entries)
	}

	fake.addOwnedVolumeAt("slow", managed)
	oldTimeout := mountAccessTimeout
	mountAccessTimeout = time.Nanosecond
	t.Cleanup(func() { mountAccessTimeout = oldTimeout })
	op = mountAccessEnsure("timeout", "v1", "slow", uid, gid, true)
	if oe := dispatch(t, h, "node-a", op).GetError(); oe == nil || oe.GetMountAccessFailure().GetReason() != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_TIMEOUT {
		t.Fatalf("timeout result = %+v", oe)
	}
	entries, err := os.ReadDir(managed)
	if err != nil || len(entries) != 1 { // the symlink created above remains
		t.Fatalf("timeout cleanup: entries=%v err=%v", entries, err)
	}
}

func TestMountAccessClassifiesReadOnlyAndUnsupported(t *testing.T) {
	for err, want := range map[error]wsv1.MountAccessFailureReason{
		os.ErrPermission: wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_IDENTITY_DENIED,
		os.ErrNotExist:   wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE,
		unix.EROFS:       wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_READ_ONLY,
		unix.ENOSYS:      wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSUPPORTED_BACKEND,
	} {
		if got := classifyMountAccessErr(err); got != want {
			t.Errorf("classify %v = %v, want %v", err, got, want)
		}
	}
	if got := runMountAccessCheck(context.Background(), filepath.Join(t.TempDir(), "missing"), uint32(os.Getuid()), uint32(os.Getgid())); got != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE {
		t.Fatalf("missing root = %v", got)
	}
}

func TestMountAccessRefusesRootReplacementAfterProbe(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	old := root + "-old"
	previous := mountAccessPostCheck
	mountAccessPostCheck = func() {
		if err := os.Rename(root, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, root); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { mountAccessPostCheck = previous })
	if got := mountAccessProbe(root, ".wildstone-access-00000000000000000000000000000000.tmp", ".wildstone-access-00000000000000000000000000000000.done"); got != mountAccessExitUnavailable {
		t.Fatalf("replaced root exit = %v, want unavailable", got)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("replacement target was inspected or mutated: %v", entries)
	}
}

func TestMountAccessReportsUnconfirmedCleanup(t *testing.T) {
	previous := mountAccessUnlinkat
	mountAccessUnlinkat = func(int, string, int) error { return unix.EPERM }
	t.Cleanup(func() { mountAccessUnlinkat = previous })
	if got := mountAccessProbe(t.TempDir(), ".wildstone-access-00000000000000000000000000000000.tmp", ".wildstone-access-00000000000000000000000000000000.done"); got != mountAccessExitCleanupIncomplete {
		t.Fatalf("unconfirmed cleanup exit = %v, want cleanup incomplete", got)
	}
}

// TestMountAccessDelayedSupervisor is a subprocess fixture, not a D-state
// simulation. It deliberately delays exit after cancellation so the caller's
// deadline and the capacity retained by a live helper can be tested separately.
func TestMountAccessDelayedSupervisor(t *testing.T) {
	mode := os.Getenv("WILDSTONE_TEST_MOUNT_ACCESS")
	if mode == "unexpected" {
		os.Exit(99)
	}
	if mode == "leaf" {
		ready := os.NewFile(4, "ready")
		_, _ = ready.Write([]byte{1})
		_ = ready.Close()
		release := os.NewFile(3, "release")
		var one [1]byte
		_, _ = release.Read(one[:])
		_ = release.Close()
		os.Exit(0)
	}
	if mode == "orphan-root" {
		cmd := exec.Command("/proc/self/exe", "-test.run=^TestMountAccessDelayedSupervisor$")
		cmd.Env = append(os.Environ(), "WILDSTONE_TEST_MOUNT_ACCESS=leaf")
		cmd.ExtraFiles = []*os.File{os.NewFile(3, "release"), os.NewFile(4, "ready")}
		if cmd.Start() != nil {
			os.Exit(1)
		}
		// Deliberately orphan the leaf, as a root worker killed during blocked
		// descendant I/O would. The dedicated supervisor must adopt and reap it.
		os.Exit(0)
	}
	if mode != "delayed" {
		return
	}
	if _, ok := readMountAccessRequest(3); !ok {
		os.Exit(1)
	}
	if unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) != nil {
		os.Exit(1)
	}
	cmd := exec.Command("/proc/self/exe", "-test.run=^TestMountAccessDelayedSupervisor$")
	cmd.Env = append(os.Environ(), "WILDSTONE_TEST_MOUNT_ACCESS=orphan-root")
	cmd.ExtraFiles = []*os.File{os.NewFile(5, "release"), os.NewFile(6, "ready")}
	if cmd.Run() != nil {
		os.Exit(1)
	}
	// This fixture intentionally postpones cancellation completion. Exercise
	// the production reaping barrier while the orphaned leaf remains alive.
	os.Exit(reapMountAccessDescendants(0))
}

func TestMountAccessRetainsCapacityUntilConfirmedExit(t *testing.T) {
	previousSlots, previousCommand := mountAccessSlots, mountAccessSupervisorCommand
	mountAccessSlots = make(chan struct{}, 1)
	defer func() { mountAccessSlots, mountAccessSupervisorCommand = previousSlots, previousCommand }()
	releaseReader, releaseWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseReader.Close()
	defer releaseWriter.Close()
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyReader.Close()
	defer readyWriter.Close()
	calls := 0
	mountAccessSupervisorCommand = func(string, string, string, uint32, uint32) *exec.Cmd {
		calls++
		cmd := exec.Command("/proc/self/exe", "-test.run=^TestMountAccessDelayedSupervisor$")
		cmd.Env = append(os.Environ(), "WILDSTONE_TEST_MOUNT_ACCESS=delayed", "GORACE=atexit_sleep_ms=0")
		cmd.ExtraFiles = []*os.File{releaseReader, readyWriter}
		return cmd
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		exit             int
		timedOut, reaped bool
	}
	result := make(chan outcome, 1)
	go func() {
		exit, timedOut, reaped := runMountAccessSupervisor(ctx, mountAccessActionProbe, mountAccessRequest{Root: "/unused"}, "", "", 0, 0)
		result <- outcome{exit, timedOut, reaped}
	}()
	if err := readyReader.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := io.ReadFull(readyReader, one[:]); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	cancel()
	select {
	case got := <-result:
		if !got.timedOut || got.reaped {
			t.Fatalf("deadline result = %+v, want timeout with unconfirmed exit", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller did not return while helper remained alive")
	}
	if time.Since(start) > time.Second {
		t.Fatal("deadline return exceeded grace and scheduling allowance")
	}
	if exit, timedOut, _ := runMountAccessSupervisor(context.Background(), mountAccessActionProbe, mountAccessRequest{Root: "/unused"}, "", "", 0, 0); exit != mountAccessExitUnavailable || timedOut {
		t.Fatalf("admission at capacity: exit=%d timeout=%v", exit, timedOut)
	}
	if calls != 1 {
		t.Fatalf("started %d helpers despite occupied capacity", calls)
	}
	_ = releaseWriter.Close()
	deadline := time.Now().Add(5 * time.Second)
	for len(mountAccessSlots) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(mountAccessSlots) != 0 {
		t.Fatal("confirmed helper exit did not release capacity")
	}
	mountAccessSupervisorCommand = previousCommand
	root, data := t.TempDir(), t.TempDir()
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if got := runMountAccessAdmission(ctx, root, Config{ManagedPrefixes: []string{root}, DataDir: data}); got != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSPECIFIED {
		t.Fatalf("probe admission did not recover after reaping: %v", got)
	}
}

func TestMountAccessBoundedAdmissionPreservesConfinement(t *testing.T) {
	root, data, outside := t.TempDir(), t.TempDir(), t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := Config{ManagedPrefixes: []string{root}, DataDir: data}
	if got := runMountAccessAdmission(ctx, root, cfg); got != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSPECIFIED {
		t.Fatalf("valid managed root: %v", got)
	}
	if got := runMountAccessAdmission(ctx, outside, cfg); got != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE {
		t.Fatalf("outside prefix: %v", got)
	}
	cfg.ManagedPrefixes = []string{data}
	if got := runMountAccessAdmission(ctx, data, cfg); got != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE {
		t.Fatalf("agent data overlap: %v", got)
	}
}

func TestMountAccessRefusesMappedDockerWithoutReplacingCurrent(t *testing.T) {
	for _, option := range []string{"name=userns", "name=rootless"} {
		t.Run(option, func(t *testing.T) {
			fake, h := mountAccessHarness(t, "library", t.TempDir())
			if res := dispatch(t, h, "node-a", mountAccessEnsure("app", "v1", "library", 0, 0, false)); res.GetError() != nil {
				t.Fatal(res.GetError())
			}
			fake.mu.Lock()
			fake.securityOptions = []string{option}
			fake.mu.Unlock()
			res := dispatch(t, h, "node-a", mountAccessEnsure("app", "v2", "library", 0, 0, true))
			if res.GetError().GetMountAccessFailure().GetReason() != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSUPPORTED_BACKEND {
				t.Fatalf("mapped Docker result: %v", res.GetError())
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if current := fake.containers["app"]; current == nil || !current.running || current.labels[SpecHashLabel] != "v1" {
				t.Fatalf("refusal replaced current workload: %+v", current)
			}
		})
	}
}

func TestMountAccessUnexpectedSupervisorExitRetainsCapacity(t *testing.T) {
	previousSlots, previousCommand := mountAccessSlots, mountAccessSupervisorCommand
	mountAccessSlots = make(chan struct{}, 1)
	defer func() { mountAccessSlots, mountAccessSupervisorCommand = previousSlots, previousCommand }()
	mountAccessSupervisorCommand = func(string, string, string, uint32, uint32) *exec.Cmd {
		cmd := exec.Command("/proc/self/exe", "-test.run=^TestMountAccessDelayedSupervisor$")
		cmd.Env = append(os.Environ(), "WILDSTONE_TEST_MOUNT_ACCESS=unexpected", "GORACE=atexit_sleep_ms=0")
		return cmd
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, timedOut, reaped := runMountAccessSupervisor(ctx, mountAccessActionProbe, mountAccessRequest{Root: "/unused"}, "", "", 0, 0)
	if timedOut || reaped {
		t.Fatalf("unexpected exit: timeout=%v reaped=%v", timedOut, reaped)
	}
	if len(mountAccessSlots) != 1 {
		t.Fatal("unexpected supervisor exit released unconfirmed descendant capacity")
	}
}

func TestMountAccessRuntimeMetadataSharesProbeDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	runtime, err := newDockerClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	previous := mountAccessTimeout
	mountAccessTimeout = 20 * time.Millisecond
	defer func() { mountAccessTimeout = previous }()
	a := &Agent{runtime: runtime}
	start := time.Now()
	oe := a.preflightMountAccess(context.Background(), mountAccessEnsure("app", "v2", "library", 0, 0, true).GetEnsureContainer())
	if oe.GetMountAccessFailure().GetReason() != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_TIMEOUT {
		t.Fatalf("stalled metadata result: %v", oe)
	}
	if time.Since(start) > time.Second {
		t.Fatal("metadata used Docker's longer general-purpose timeout")
	}
}

func TestMountAccessUnknownRuntimeIdentityIsRefused(t *testing.T) {
	for _, payload := range []string{`{}`, `{"SecurityOptions":null}`, `{"SecurityOptions":["name=userns"]}`, `{"SecurityOptions":["name=rootless"]}`} {
		t.Run(payload, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, payload) }))
			defer server.Close()
			runtime, err := newDockerClient(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			ok, err := runtime.mountAccessIdentitySupported(context.Background())
			if err != nil || ok {
				t.Fatalf("unknown/mapped identity supported=%v err=%v", ok, err)
			}
		})
	}
}
