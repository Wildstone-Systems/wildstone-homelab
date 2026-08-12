package agent

// ADR-W003 W6c: storage/network ops. EnsureMount is, per the ADR, "the
// most dangerous op in the product" — its FORBIDDEN tests are the most
// important tests in this file.

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// ---------------------------------------------------------------- EnsureMount

// fakeMount records calls instead of touching the real mount table. Tests
// must never perform a real mount — real-mount e2e coverage belongs to the
// VM harness in testing/e2e.
type fakeMountCall struct {
	source, target, fstype, data string
	flags                        uintptr
}

func stubMount(t *testing.T, fn func(source, target, fstype string, flags uintptr, data string) error) *[]fakeMountCall {
	t.Helper()
	orig := mountFn
	t.Cleanup(func() { mountFn = orig })
	var mu sync.Mutex
	var calls []fakeMountCall
	mountFn = func(source, target, fstype string, flags uintptr, data string) error {
		mu.Lock()
		calls = append(calls, fakeMountCall{source, target, fstype, data, flags})
		mu.Unlock()
		if fn != nil {
			return fn(source, target, fstype, flags, data)
		}
		return nil
	}
	return &calls
}

func stubMountinfo(t *testing.T, path string) {
	t.Helper()
	orig := mountinfoPath
	t.Cleanup(func() { mountinfoPath = orig })
	mountinfoPath = path
}

func TestEnsureMountHappyPathAndIdempotent(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	mountRoot := t.TempDir()
	fstabPath := filepath.Join(t.TempDir(), "fstab")
	mountinfoFixture := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountinfoFixture, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	calls := stubMount(t, nil)
	stubMountinfo(t, mountinfoFixture)

	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.ManagedPrefixes = []string{mountRoot}
		c.MountRoot = mountRoot
		c.FstabPath = fstabPath
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	target := filepath.Join(mountRoot, "nfs-share")
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureMount{
		EnsureMount: &wsv1.EnsureMount{
			Source: "10.0.0.5:/export", Target: target, Fstype: "nfs", Options: []string{"ro"},
		},
	}})
	er := res.GetEnsureMount()
	if er == nil || !er.GetMounted() || !er.GetFstabUpdated() {
		t.Fatalf("ensure mount happy path: %+v", res)
	}
	if len(*calls) != 1 {
		t.Fatalf("want 1 mount call, got %d: %+v", len(*calls), *calls)
	}
	call := (*calls)[0]
	if call.target != target || call.fstype != "nfs" || call.source != "10.0.0.5:/export" {
		t.Fatalf("mount call: %+v", call)
	}
	// nosuid/nodev/ro are mount(2) FLAGS, not data-string words — a kernel
	// fs driver rejects (EINVAL) or ignores them in data, which would
	// silently defeat the force-appended security options.
	for _, want := range []struct {
		name string
		bit  uintptr
	}{{"MS_NOSUID", syscall.MS_NOSUID}, {"MS_NODEV", syscall.MS_NODEV}, {"MS_RDONLY", syscall.MS_RDONLY}} {
		if call.flags&want.bit == 0 {
			t.Fatalf("mount flags %#x missing %s", call.flags, want.name)
		}
	}
	for _, word := range []string{"nosuid", "nodev", "ro"} {
		if strings.Contains(call.data, word) {
			t.Fatalf("flag option %q leaked into mount data %q", word, call.data)
		}
	}

	fstabContent, err := os.ReadFile(fstabPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fstabContent), target) {
		t.Fatalf("fstab does not contain target: %s", fstabContent)
	}
	if !strings.Contains(string(fstabContent), fstabBeginMarker) || !strings.Contains(string(fstabContent), fstabEndMarker) {
		t.Fatalf("fstab missing marker block: %s", fstabContent)
	}

	// Simulate mountinfo now reporting the target as mounted (field 5).
	line := fmt.Sprintf("1 1 0:1 / %s rw - nfs 10.0.0.5:/export rw", target)
	if err := os.WriteFile(mountinfoFixture, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureMount{
		EnsureMount: &wsv1.EnsureMount{
			Source: "10.0.0.5:/export", Target: target, Fstype: "nfs", Options: []string{"ro"},
		},
	}})
	er = res.GetEnsureMount()
	if er == nil || er.GetMounted() || er.GetFstabUpdated() {
		t.Fatalf("ensure mount idempotent call: want mounted=false fstab_updated=false, got %+v", res)
	}
	if len(*calls) != 1 {
		t.Fatalf("mountFn called again on an already-mounted target: %d calls", len(*calls))
	}
}

func TestEnsureMountForbiddenBoundaries(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	mountRoot := t.TempDir()
	otherManaged := t.TempDir() // managed, but NOT under MountRoot
	fstabPath := filepath.Join(t.TempDir(), "fstab")
	mountinfoFixture := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountinfoFixture, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	calls := stubMount(t, nil)
	stubMountinfo(t, mountinfoFixture)

	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.ManagedPrefixes = []string{mountRoot, otherManaged}
		c.MountRoot = mountRoot
		c.FstabPath = fstabPath
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// Target is under a managed prefix but NOT under MountRoot: the second
	// hard boundary must still refuse it.
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureMount{
		EnsureMount: &wsv1.EnsureMount{
			Source: "10.0.0.5:/export", Target: filepath.Join(otherManaged, "share"), Fstype: "nfs",
		},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("target outside MountRoot: want FORBIDDEN, got %+v", res)
	}

	// fstype not in the allowlist.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureMount{
		EnsureMount: &wsv1.EnsureMount{
			Source: "/dev/sdb1", Target: filepath.Join(mountRoot, "tmpfs-share"), Fstype: "tmpfs",
		},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("disallowed fstype: want FORBIDDEN, got %+v", res)
	}

	// Comma-injection in an option.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureMount{
		EnsureMount: &wsv1.EnsureMount{
			Source: "10.0.0.5:/export", Target: filepath.Join(mountRoot, "inject-share"), Fstype: "nfs",
			Options: []string{"foo,bar"},
		},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("comma-injection option: want FORBIDDEN, got %+v", res)
	}

	// Newline-injection in the source: a raw newline would inject a whole
	// attacker-chosen /etc/fstab entry (a boot-time landmine).
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureMount{
		EnsureMount: &wsv1.EnsureMount{
			Source: "10.0.0.5:/export\n/dev/sda1 /mnt/evil ext4 defaults 0 0",
			Target: filepath.Join(mountRoot, "inject-src"), Fstype: "nfs",
		},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("newline-injection source: want FORBIDDEN, got %+v", res)
	}

	// Whitespace in the source breaks fstab field parsing too.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureMount{
		EnsureMount: &wsv1.EnsureMount{
			Source: "10.0.0.5:/ex port", Target: filepath.Join(mountRoot, "inject-space"), Fstype: "nfs",
		},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("whitespace source: want FORBIDDEN, got %+v", res)
	}

	if len(*calls) != 0 {
		t.Fatalf("mountFn must never be called for a FORBIDDEN request, got %d calls: %+v", len(*calls), *calls)
	}
	if _, err := os.Stat(fstabPath); !os.IsNotExist(err) {
		t.Fatalf("fstab must not be created by any FORBIDDEN request: stat err=%v", err)
	}
}

func TestEnsureMountFailureDoesNotUpdateFstab(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	mountRoot := t.TempDir()
	fstabPath := filepath.Join(t.TempDir(), "fstab")
	mountinfoFixture := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountinfoFixture, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stubMount(t, func(source, target, fstype string, flags uintptr, data string) error {
		return fmt.Errorf("simulated mount failure")
	})
	stubMountinfo(t, mountinfoFixture)

	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.ManagedPrefixes = []string{mountRoot}
		c.MountRoot = mountRoot
		c.FstabPath = fstabPath
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureMount{
		EnsureMount: &wsv1.EnsureMount{
			Source: "10.0.0.5:/export", Target: filepath.Join(mountRoot, "share"), Fstype: "nfs",
		},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_RUNTIME {
		t.Fatalf("failed mount: want RUNTIME, got %+v", res)
	}
	// Ordering guarantee: a failed mount must never leave a boot-time
	// landmine in fstab.
	if _, err := os.Stat(fstabPath); !os.IsNotExist(err) {
		t.Fatalf("fstab was written despite mount failure: stat err=%v", err)
	}
}

func TestEnsureMountFstabPreservesUserContent(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	mountRoot := t.TempDir()
	fstabPath := filepath.Join(t.TempDir(), "fstab")
	mountinfoFixture := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountinfoFixture, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stubMount(t, nil)
	stubMountinfo(t, mountinfoFixture)

	before := "UUID=deadbeef / ext4 defaults 0 1\n"
	existingEntry := "existing-src\t/existing-target\text4\tdefaults\t0\t0"
	after := "# a trailing user comment\ntmpfs /tmp tmpfs defaults 0 0\n"
	seed := before + fstabBeginMarker + "\n" + existingEntry + "\n" + fstabEndMarker + "\n" + after
	if err := os.WriteFile(fstabPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.ManagedPrefixes = []string{mountRoot}
		c.MountRoot = mountRoot
		c.FstabPath = fstabPath
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	target := filepath.Join(mountRoot, "new-share")
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureMount{
		EnsureMount: &wsv1.EnsureMount{
			Source: "10.0.0.5:/export", Target: target, Fstype: "nfs",
		},
	}})
	if er := res.GetEnsureMount(); er == nil || !er.GetFstabUpdated() {
		t.Fatalf("ensure mount: %+v", res)
	}

	got, err := os.ReadFile(fstabPath)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	if !strings.HasPrefix(gotStr, before) {
		t.Fatalf("content before the block was not preserved byte-identical:\n%s", gotStr)
	}
	if !strings.HasSuffix(gotStr, after) {
		t.Fatalf("content after the block was not preserved byte-identical:\n%s", gotStr)
	}
	if !strings.Contains(gotStr, existingEntry) {
		t.Fatalf("pre-existing managed entry was dropped:\n%s", gotStr)
	}
	if !strings.Contains(gotStr, target) {
		t.Fatalf("new entry was not added:\n%s", gotStr)
	}
}

// ---------------------------------------------------------------- ListBlockDevices

func TestListBlockDevices(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	fixture := t.TempDir()
	mkdev := func(name, size, removable string) {
		dir := filepath.Join(fixture, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "size"), []byte(size+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "removable"), []byte(removable+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdev("sda", "1024", "0")
	mkdev("sr0", "2048", "1")
	mkdev("loop0", "999", "0")

	orig := sysBlockPath
	t.Cleanup(func() { sysBlockPath = orig })
	sysBlockPath = fixture

	newTestAgent(t, h, "node-a", "tok", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ListBlockDevices{
		ListBlockDevices: &wsv1.ListBlockDevices{},
	}})
	lr := res.GetListBlockDevices()
	if lr == nil {
		t.Fatalf("list block devices: %+v", res)
	}
	byName := map[string]*wsv1.BlockDevice{}
	for _, d := range lr.GetDevices() {
		byName[d.GetName()] = d
	}
	if _, ok := byName["loop0"]; ok {
		t.Fatal("loop0 must be skipped")
	}
	sda := byName["sda"]
	if sda == nil || sda.GetSizeBytes() != 1024*512 || sda.GetRemovable() {
		t.Fatalf("sda: %+v", sda)
	}
	sr0 := byName["sr0"]
	if sr0 == nil || sr0.GetSizeBytes() != 2048*512 || !sr0.GetRemovable() {
		t.Fatalf("sr0: %+v", sr0)
	}
}

// ---------------------------------------------------------------- ListPorts

func TestListPorts(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	tcpFixture := filepath.Join(t.TempDir(), "tcp")
	tcp6Fixture := filepath.Join(t.TempDir(), "tcp6")

	header := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	// 127.0.0.1:8080, LISTEN (st=0A).
	tcpContent := header +
		"   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1 0000000000000000 100 0 0 10 0\n"
	if err := os.WriteFile(tcpFixture, []byte(tcpContent), 0o644); err != nil {
		t.Fatal(err)
	}
	// ::1 port 9090 (0x2382), LISTEN.
	tcp6Content := header +
		"   0: 00000000000000000000000001000000:2382 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1 0000000000000000 100 0 0 10 0\n"
	if err := os.WriteFile(tcp6Fixture, []byte(tcp6Content), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := procNetPaths
	t.Cleanup(func() { procNetPaths = orig })
	procNetPaths = map[string]string{"tcp": tcpFixture, "tcp6": tcp6Fixture}

	newTestAgent(t, h, "node-a", "tok", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ListPorts{ListPorts: &wsv1.ListPorts{}}})
	lr := res.GetListPorts()
	if lr == nil || len(lr.GetPorts()) != 2 {
		t.Fatalf("list ports: %+v", res)
	}
	var tcpPort, tcp6Port *wsv1.ListeningPort
	for _, p := range lr.GetPorts() {
		switch p.GetProto() {
		case "tcp":
			tcpPort = p
		case "tcp6":
			tcp6Port = p
		}
	}
	if tcpPort == nil || tcpPort.GetPort() != 8080 || tcpPort.GetLocalIp() != "127.0.0.1" {
		t.Fatalf("tcp port: %+v", tcpPort)
	}
	if tcp6Port == nil || tcp6Port.GetPort() != 9090 || tcp6Port.GetLocalIp() != "::1" {
		t.Fatalf("tcp6 port: %+v", tcp6Port)
	}
}

// ---------------------------------------------------------------- TestNfs / TestSmb / ProbeTcp

func TestTestNfsTestSmbProbeTcpReachability(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	host, portStr, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var port uint32
	fmt.Sscanf(portStr, "%d", &port)

	// A closed port: dial+immediately close to free it up, guaranteeing
	// nothing is listening there.
	closedLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closedLis.Addr().String()
	closedLis.Close()
	closedHost, closedPortStr, err := net.SplitHostPort(closedAddr)
	if err != nil {
		t.Fatal(err)
	}
	var closedPort uint32
	fmt.Sscanf(closedPortStr, "%d", &closedPort)

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_TestNfs{
		TestNfs: &wsv1.TestNfs{Host: host, Port: port},
	}})
	if nr := res.GetTestNfs(); nr == nil || !nr.GetReachable() {
		t.Fatalf("TestNfs reachable: %+v", res)
	}
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_TestNfs{
		TestNfs: &wsv1.TestNfs{Host: closedHost, Port: closedPort},
	}})
	if nr := res.GetTestNfs(); nr == nil || nr.GetReachable() {
		t.Fatalf("TestNfs unreachable: want reachable=false (not an OpError), got %+v", res)
	}
	if res.GetError() != nil {
		t.Fatalf("TestNfs against closed port must not be an OpError: %+v", res)
	}

	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_TestSmb{
		TestSmb: &wsv1.TestSmb{Host: host, Port: port},
	}})
	if sr := res.GetTestSmb(); sr == nil || !sr.GetReachable() {
		t.Fatalf("TestSmb reachable: %+v", res)
	}
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_TestSmb{
		TestSmb: &wsv1.TestSmb{Host: closedHost, Port: closedPort},
	}})
	if sr := res.GetTestSmb(); sr == nil || sr.GetReachable() {
		t.Fatalf("TestSmb unreachable: %+v", res)
	}

	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ProbeTcp{
		ProbeTcp: &wsv1.ProbeTcp{Host: host, Port: port},
	}})
	pr := res.GetProbeTcp()
	if pr == nil || !pr.GetReachable() {
		t.Fatalf("ProbeTcp reachable: %+v", res)
	}
	if pr.GetLatencyMs() > 5000 {
		t.Fatalf("ProbeTcp latency implausible: %d ms", pr.GetLatencyMs())
	}
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ProbeTcp{
		ProbeTcp: &wsv1.ProbeTcp{Host: closedHost, Port: closedPort},
	}})
	if pr := res.GetProbeTcp(); pr == nil || pr.GetReachable() {
		t.Fatalf("ProbeTcp unreachable: %+v", res)
	}
	if res.GetError() != nil {
		t.Fatalf("ProbeTcp against closed port must not be an OpError: %+v", res)
	}

	// port 0: INVALID.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ProbeTcp{
		ProbeTcp: &wsv1.ProbeTcp{Host: host, Port: 0},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_INVALID {
		t.Fatalf("ProbeTcp port 0: want INVALID, got %+v", res)
	}

	// timeout 31: INVALID.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ProbeTcp{
		ProbeTcp: &wsv1.ProbeTcp{Host: host, Port: port, TimeoutSeconds: 31},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_INVALID {
		t.Fatalf("ProbeTcp timeout 31: want INVALID, got %+v", res)
	}
}

// ---------------------------------------------------------------- ProbeHttp

func TestProbeHttp(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	var redirectTargetHits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/redirect":
			http.Redirect(w, r, "/redirect-target", http.StatusFound)
		case "/redirect-target":
			mu.Lock()
			redirectTargetHits++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ProbeHttp{
		ProbeHttp: &wsv1.ProbeHttp{Url: srv.URL + "/ok"},
	}})
	pr := res.GetProbeHttp()
	if pr == nil || !pr.GetReachable() || pr.GetStatusCode() != 200 {
		t.Fatalf("ProbeHttp /ok: %+v", res)
	}

	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ProbeHttp{
		ProbeHttp: &wsv1.ProbeHttp{Url: srv.URL + "/redirect"},
	}})
	pr = res.GetProbeHttp()
	if pr == nil || !pr.GetReachable() || pr.GetStatusCode() != 302 {
		t.Fatalf("ProbeHttp /redirect: want status 302, got %+v", res)
	}
	mu.Lock()
	hits := redirectTargetHits
	mu.Unlock()
	if hits != 0 {
		t.Fatalf("redirect target was fetched (%d hits) — ProbeHttp must not follow redirects", hits)
	}

	// file:// scheme: FORBIDDEN.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ProbeHttp{
		ProbeHttp: &wsv1.ProbeHttp{Url: "file:///etc/passwd"},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("ProbeHttp file://: want FORBIDDEN, got %+v", res)
	}
}

// ---------------------------------------------------------------- EnsureDockerNetwork

func TestEnsureDockerNetwork(t *testing.T) {
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureDockerNetwork{
		EnsureDockerNetwork: &wsv1.EnsureDockerNetwork{Name: "wildstone-net"},
	}})
	nr := res.GetEnsureDockerNetwork()
	if nr == nil || !nr.GetCreated() {
		t.Fatalf("ensure docker network create: %+v", res)
	}
	if fake.networks["wildstone-net"].labels[OwnerLabel] != "wildstone" {
		t.Fatal("owner label not stamped on created network")
	}

	// Idempotent.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureDockerNetwork{
		EnsureDockerNetwork: &wsv1.EnsureDockerNetwork{Name: "wildstone-net"},
	}})
	if res.GetEnsureDockerNetwork().GetCreated() {
		t.Fatalf("second ensure recreated the network: %+v", res)
	}

	// Foreign network: FORBIDDEN, never adopted.
	fake.addForeignNetwork("someone-elses-net")
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureDockerNetwork{
		EnsureDockerNetwork: &wsv1.EnsureDockerNetwork{Name: "someone-elses-net"},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("ensure foreign network: want FORBIDDEN, got %+v", res)
	}
}
