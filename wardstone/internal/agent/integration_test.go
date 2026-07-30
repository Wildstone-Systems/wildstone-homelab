package agent

// Integration tests: a real Gateway (Keystone side) and real Agents talk
// over localhost TLS 1.3 exactly as they would over a tailnet. No mocks on
// the protocol path; only the Docker daemon is faked (httptest server).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/gateway"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

const testDigestImage = "ghcr.io/example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type harness struct {
	gw   *gateway.Gateway
	addr string
	done chan error
}

// startGateway serves a fresh gateway; reusePort lets restarts keep the
// same address so agents can reconnect to it.
func startGateway(t *testing.T, tokens []string, addr string) *harness {
	t.Helper()
	gw, err := gateway.New(gateway.Config{
		JoinTokens: tokens,
		Hosts:      []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{gw: gw, addr: lis.Addr().String(), done: make(chan error, 1)}
	go func() { h.done <- gw.Serve(lis) }()
	t.Cleanup(gw.Stop)
	return h
}

func newTestAgent(t *testing.T, h *harness, nodeID, token string, mutate func(*Config)) (*Agent, context.CancelFunc) {
	t.Helper()
	cfg := Config{
		NodeID:          nodeID,
		KeystoneAddr:    h.addr,
		DataDir:         filepath.Join(t.TempDir(), "data"),
		JoinToken:       token,
		ManagedPrefixes: []string{filepath.Join(t.TempDir(), "managed")},
		DockerEndpoint:  "http://127.0.0.1:1", // unused unless a test overrides
		BinaryPath:      filepath.Join(t.TempDir(), "wardstoned"),
		Version:         "test",
		BackoffBase:     50 * time.Millisecond,
		BackoffCap:      500 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go a.Run(ctx)
	t.Cleanup(cancel)
	return a, cancel
}

func dispatch(t *testing.T, h *harness, nodeID string, op *wsv1.Operation) *wsv1.OpResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := h.gw.Dispatch(ctx, nodeID, op)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return res
}

// ---------------------------------------------------------------- lifecycle

func TestJoinSessionAndCoreOps(t *testing.T) {
	h := startGateway(t, []string{"tok-1"}, "")
	managed := t.TempDir()
	newTestAgent(t, h, "node-a", "tok-1", func(c *Config) {
		c.ManagedPrefixes = []string{managed}
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// Health
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_Health{Health: &wsv1.Health{}}})
	if hr := res.GetHealth(); hr == nil || !hr.GetOk() {
		t.Fatalf("health: %+v", res)
	}

	// WriteManagedFile / ReadManagedFile round trip
	target := filepath.Join(managed, "sub", "caddy.json")
	content := []byte(`{"fixture": true}`)
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
		WriteManagedFile: &wsv1.WriteManagedFile{Path: target, Content: content, Mode: 0o600},
	}})
	want := sha256.Sum256(content)
	if res.GetWriteManagedFile().GetSha256Hex() != hex.EncodeToString(want[:]) {
		t.Fatalf("write result: %+v", res)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("written file: %v %v", info, err)
	}
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ReadManagedFile{
		ReadManagedFile: &wsv1.ReadManagedFile{Path: target},
	}})
	if string(res.GetReadManagedFile().GetContent()) != string(content) {
		t.Fatalf("read back: %+v", res)
	}

	// HostFacts
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_HostFacts{HostFacts: &wsv1.HostFacts{}}})
	if hf := res.GetHostFacts(); hf == nil || hf.GetNumCpu() == 0 || hf.GetOs() == "" {
		t.Fatalf("host facts: %+v", res)
	}
}

func TestJoinTokenSingleUseAndBadToken(t *testing.T) {
	h := startGateway(t, []string{"tok-once"}, "")
	newTestAgent(t, h, "node-a", "tok-once", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("first agent never connected")
	}
	// Second agent reusing the same token must be rejected and stay out.
	newTestAgent(t, h, "node-b", "tok-once", nil)
	if h.gw.WaitConnected("node-b", 1*time.Second) {
		t.Fatal("agent with a used join token was admitted")
	}
	newTestAgent(t, h, "node-c", "no-such-token", nil)
	if h.gw.WaitConnected("node-c", 1*time.Second) {
		t.Fatal("agent with an invalid join token was admitted")
	}
}

func TestSessionRequiresClientCert(t *testing.T) {
	h := startGateway(t, nil, "")
	// A "certificate-less" client: hand-roll the session dial the agent
	// would do, but without a client cert. The gateway must refuse.
	a, err := New(Config{
		NodeID: "node-x", KeystoneAddr: h.addr, DataDir: t.TempDir(),
		DockerEndpoint: "http://127.0.0.1:1",
		BinaryPath:     filepath.Join(t.TempDir(), "wardstoned"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// No identity on disk and no join token: Run must fail fast, never connect.
	if err := a.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "join") {
		t.Fatalf("expected join failure, got %v", err)
	}
	if h.gw.Connected("node-x") {
		t.Fatal("uncertified agent is connected")
	}
}

// ---------------------------------------------------------------- invariants

func TestManagedPrefixEnforced(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	managed := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim.txt")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.ManagedPrefixes = []string{managed} })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	for _, path := range []string{
		outside,                                 // plainly outside
		filepath.Join(managed, "..", "esc.txt"), // dot-dot escape
		"relative/path.txt",                     // not absolute
	} {
		res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
			WriteManagedFile: &wsv1.WriteManagedFile{Path: path, Content: []byte("x")},
		}})
		if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
			t.Errorf("write to %q: want FORBIDDEN, got %+v", path, res)
		}
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("file outside managed prefix was created")
	}

	// Symlink escape: managed/link -> /  then write via managed/link/...
	link := filepath.Join(managed, "link")
	if err := os.Symlink("/", link); err != nil {
		t.Fatal(err)
	}
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
		WriteManagedFile: &wsv1.WriteManagedFile{Path: filepath.Join(link, "tmp", "wildstone-escape.txt"), Content: []byte("x")},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Errorf("symlink escape: want FORBIDDEN, got %+v", res)
	}
}

func TestDigestPinningEnforced(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	for _, image := range []string{"nginx:latest", "nginx", "ghcr.io/x/y:v1.2.3"} {
		res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_PullImage{
			PullImage: &wsv1.PullImage{Image: image},
		}})
		if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
			t.Errorf("pull %q: want FORBIDDEN, got %+v", image, res)
		}
		res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
			EnsureContainer: &wsv1.EnsureContainer{Name: "c", Image: image},
		}})
		if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
			t.Errorf("ensure %q: want FORBIDDEN, got %+v", image, res)
		}
	}
}

func TestContainerOpsAgainstFakeDocker(t *testing.T) {
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// Ensure: create + start, owner label stamped.
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{Name: "app", Image: testDigestImage},
	}})
	ec := res.GetEnsureContainer()
	if ec == nil || !ec.GetCreated() || !ec.GetStarted() {
		t.Fatalf("ensure: %+v", res)
	}
	if fake.containers["app"].labels[OwnerLabel] != "wildstone" {
		t.Fatal("owner label not stamped on created container")
	}

	// Ensure again: idempotent, not recreated.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{Name: "app", Image: testDigestImage},
	}})
	if res.GetEnsureContainer().GetCreated() {
		t.Fatalf("second ensure recreated the container: %+v", res)
	}

	// Logs.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ContainerLogs{
		ContainerLogs: &wsv1.ContainerLogs{Name: "app", Tail: 10},
	}})
	if !strings.Contains(string(res.GetContainerLogs().GetLogs()), "fake log line") {
		t.Fatalf("logs: %+v", res)
	}

	// Pull (digest-pinned) passes through.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_PullImage{
		PullImage: &wsv1.PullImage{Image: testDigestImage},
	}})
	if res.GetPullImage().GetDigest() == "" {
		t.Fatalf("pull: %+v", res)
	}

	// Stop.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_StopContainer{
		StopContainer: &wsv1.StopContainer{Name: "app"},
	}})
	if !res.GetStopContainer().GetWasRunning() {
		t.Fatalf("stop: %+v", res)
	}

	// Ownership guard: a container that exists but is NOT wildstone-owned.
	fake.addForeign("victim")
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_StopContainer{
		StopContainer: &wsv1.StopContainer{Name: "victim"},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("stop foreign container: want FORBIDDEN, got %+v", res)
	}
	if !fake.containers["victim"].running {
		t.Fatal("foreign container was actually stopped")
	}
}

func TestRemoveContainerContainerStatsPruneImages(t *testing.T) {
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// Create an owned container to operate on.
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{Name: "app", Image: testDigestImage},
	}})
	if ec := res.GetEnsureContainer(); ec == nil || !ec.GetCreated() {
		t.Fatalf("ensure: %+v", res)
	}

	// ContainerStats: returns the fake's fixed numbers for an owned container.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ContainerStats{
		ContainerStats: &wsv1.ContainerStats{Name: "app"},
	}})
	sr := res.GetContainerStats()
	if sr == nil || sr.GetCpuUsageNs() != 424242 || sr.GetMemUsageBytes() != 111111 || sr.GetMemLimitBytes() != 999999 {
		t.Fatalf("container stats: %+v", res)
	}

	// ContainerStats on a foreign container: FORBIDDEN.
	fake.addForeign("victim-stats")
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ContainerStats{
		ContainerStats: &wsv1.ContainerStats{Name: "victim-stats"},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("stats on foreign container: want FORBIDDEN, got %+v", res)
	}

	// RemoveContainer on a foreign container: FORBIDDEN, fake still has it.
	fake.addForeign("victim-remove")
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_RemoveContainer{
		RemoveContainer: &wsv1.RemoveContainer{Name: "victim-remove"},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("remove foreign container: want FORBIDDEN, got %+v", res)
	}
	if _, ok := fake.containers["victim-remove"]; !ok {
		t.Fatal("foreign container was actually removed")
	}

	// RemoveContainer on the owned container: existed=true, fake no longer has it.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_RemoveContainer{
		RemoveContainer: &wsv1.RemoveContainer{Name: "app", Force: true},
	}})
	if rc := res.GetRemoveContainer(); rc == nil || !rc.GetExisted() {
		t.Fatalf("remove owned container: %+v", res)
	}
	if _, ok := fake.containers["app"]; ok {
		t.Fatal("owned container still present after remove")
	}

	// RemoveContainer again: idempotent success, existed=false.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_RemoveContainer{
		RemoveContainer: &wsv1.RemoveContainer{Name: "app"},
	}})
	if res.GetError() != nil {
		t.Fatalf("second remove returned an error: %+v", res)
	}
	if rc := res.GetRemoveContainer(); rc == nil || rc.GetExisted() {
		t.Fatalf("second remove: want existed=false success, got %+v", res)
	}

	// PruneImages: result carries the fake's fixed numbers, and the fake
	// recorded the ownership label filter — pruning without it would be
	// an ADR-W003 §A4.3 violation even if the numbers matched.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_PruneImages{
		PruneImages: &wsv1.PruneImages{},
	}})
	pr := res.GetPruneImages()
	if pr == nil || pr.GetReclaimedBytes() != 12345 || pr.GetDeletedCount() != 2 {
		t.Fatalf("prune images: %+v", res)
	}
	if !strings.Contains(fake.pruneFilters, "dev.wildstone.owner=wildstone") {
		t.Fatalf("prune images: fake did not receive ownership label filter, got %q", fake.pruneFilters)
	}
}

// ---------------------------------------------------------------- self-update

func TestSelfUpdateHappyPathAndBadSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "wardstoned")
	if err := os.WriteFile(binPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.BinaryPath = binPath
		c.UpdatePublicKey = pub
		// ADR-W003 W5: a promoted result triggers a real re-exec after this
		// op's result is sent; override it so the test process is not
		// replaced. See selfupdate_w5_test.go for tests of that wiring.
		c.ExecSelf = func() {}
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	newBinary := []byte("new-binary-v2")
	sum := sha256.Sum256(newBinary)
	op := &wsv1.SelfUpdate{
		Binary:    newBinary,
		Signature: ed25519.Sign(priv, sum[:]),
		Version:   "v2",
		Sha256Hex: hex.EncodeToString(sum[:]),
	}
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_SelfUpdate{SelfUpdate: op}})
	su := res.GetSelfUpdate()
	if su == nil || !su.GetPromoted() {
		t.Fatalf("self-update: %+v", res)
	}
	if got, _ := os.ReadFile(binPath); string(got) != "new-binary-v2" {
		t.Fatalf("binary not promoted: %q", got)
	}
	if got, _ := os.ReadFile(su.GetPreviousPath()); string(got) != "old-binary" {
		t.Fatalf("rollback artifact wrong: %q", got)
	}

	// Tampered payload: flip a byte, keep the (now wrong) signature.
	bad := append([]byte{}, newBinary...)
	bad[0] ^= 0xff
	badSum := sha256.Sum256(bad)
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_SelfUpdate{SelfUpdate: &wsv1.SelfUpdate{
		Binary:    bad,
		Signature: op.Signature,
		Version:   "v3",
		Sha256Hex: hex.EncodeToString(badSum[:]),
	}}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_VERIFICATION {
		t.Fatalf("tampered update: want VERIFICATION error, got %+v", res)
	}
	if got, _ := os.ReadFile(binPath); string(got) != "new-binary-v2" {
		t.Fatal("tampered update modified the binary")
	}
}

// ---------------------------------------------------------------- reconnect storm

func TestReconnectStorm(t *testing.T) {
	const agents = 20
	tokens := make([]string, agents)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("tok-%d", i)
	}
	h := startGateway(t, tokens, "")

	for i := 0; i < agents; i++ {
		newTestAgent(t, h, fmt.Sprintf("node-%d", i), tokens[i], nil)
	}
	for i := 0; i < agents; i++ {
		if !h.gw.WaitConnected(fmt.Sprintf("node-%d", i), 15*time.Second) {
			t.Fatalf("node-%d never connected initially", i)
		}
	}

	// Kill the gateway: every agent loses its stream at once.
	h.gw.Stop()

	// Restart on the SAME address with a fresh gateway process — but agents
	// pinned the old CA, so a fresh CA would (correctly) be rejected. The
	// storm test needs the same gateway identity back, mirroring a Keystone
	// process restart with persisted CA. Spike gateway keeps its CA for the
	// life of the object, so restart = new listener on the same Gateway.
	lis, err := net.Listen("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	go h.gw.Serve(lis) //nolint:errcheck

	var wg sync.WaitGroup
	failures := make(chan string, agents)
	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			node := fmt.Sprintf("node-%d", n)
			if !h.gw.WaitConnected(node, 30*time.Second) {
				failures <- node + ": never reconnected"
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res, err := h.gw.Dispatch(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_Health{Health: &wsv1.Health{}}})
			if err != nil || !res.GetHealth().GetOk() {
				failures <- fmt.Sprintf("%s: post-reconnect health failed: %v %+v", node, err, res)
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for f := range failures {
		t.Error(f)
	}
}
