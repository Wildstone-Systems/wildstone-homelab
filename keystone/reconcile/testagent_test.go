package reconcile

// A minimal real Wardstone agent for keystone/reconcile's tests: real join
// + real mTLS Session stream against a real gateway.Gateway, exactly as
// wardstone/internal/agent's own integration tests do (real gateway + real
// agent, only Docker faked) — see ADR-W004 B1. keystone cannot import
// wardstone/internal/agent directly (wardstone's module depends on
// keystone's, so the reverse import would be circular, and the package is
// internal to wardstone besides), so this recreates the minimal subset of
// the protocol client the Reconciler's op surface actually needs:
// EnsureContainer, StopContainer, RemoveContainer, ContainerStats,
// EnsureVolume, WriteManagedFile, ReadManagedFile, ProbeHttp. Docker itself
// is faked as plain in-memory Go state (no HTTP layer needed, since this
// test agent talks to that state directly rather than shelling out to a
// docker daemon), and WriteManagedFile/ReadManagedFile use the real
// filesystem under a t.TempDir().

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/chickenrat17/wildstone-homelab/keystone/gateway"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// testDigestImage is a validly digest-pinned image reference for tests.
const testDigestImage = "ghcr.io/example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// ---------------------------------------------------------------- harness

type harness struct {
	gw   *gateway.Gateway
	addr string
}

func startGateway(t *testing.T, tokens []string) *harness {
	t.Helper()
	gw, err := gateway.New(gateway.Config{
		JoinTokens: tokens,
		Hosts:      []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{gw: gw, addr: lis.Addr().String()}
	go gw.Serve(lis) //nolint:errcheck
	t.Cleanup(gw.Stop)
	return h
}

// ---------------------------------------------------------------- fake docker/fs state

type fakeContainer struct {
	image   string
	labels  map[string]string
	running bool
	owned   bool
	// status/exitCode model protocol minor 11. A container the fake has
	// never stopped reports "running"; one a job run has finished
	// reports "exited" and a code.
	status   string
	exitCode int32
	// network/networkMode are what the container was created with, so a
	// test can see a move between the managed bridge and the host's
	// namespace (protocol minor 19).
	network     string
	networkMode wsv1.ContainerNetworkMode
	// hostAliases is the EnsureContainer.host_aliases it was created with.
	hostAliases []string
	// mounts and capAdd are what it was created with (#445).
	mounts []*wsv1.Mount
	capAdd []string
}

// dockerStatus is what the real agent reads off Docker's State.Status.
func (c *fakeContainer) dockerStatus() string {
	if c.status != "" {
		return c.status
	}
	if c.running {
		return "running"
	}
	return "exited"
}

type fakeVolume struct {
	owned bool
}

// testAgent is the minimal real Wardstone-protocol client described in the
// file doc: it joins h's gateway for real (CSR/cert issuance, mTLS
// Session), and answers the subset of ops the Reconciler actually issues
// against fake in-memory container/volume state plus the real filesystem
// (managed files).
type testAgent struct {
	t       *testing.T
	nodeID  string
	managed string // managed-file root (t.TempDir())

	mu         sync.Mutex
	containers map[string]*fakeContainer
	volumes    map[string]*fakeVolume
	opCounts   map[string]int
	// jobExit/jobRuns model one-shot jobs: a name registered in jobExit
	// exits with that code the moment it is created or started, and
	// jobRuns counts how often that happened.
	jobExit map[string]int32
	jobRuns map[string]int
	// secrets records what was ensured and with which kind, so a test can
	// assert an RSA key was asked for rather than a random blob — the
	// distinction the Secret kind exists to carry.
	secrets map[string]wsv1.SecretKind
	// listening is what ListPorts reports: the node's listening TCP
	// sockets, which a test sets to put something on a port.
	listening []*wsv1.ListeningPort
	// readiness is what HomeAutomationReadiness reports (#436); nil
	// answers with no checks.
	readiness []*wsv1.ReadinessCheck
}

func newTestAgent(t *testing.T, h *harness, nodeID, token string) *testAgent {
	t.Helper()
	a := &testAgent{
		t:          t,
		nodeID:     nodeID,
		managed:    t.TempDir(),
		containers: map[string]*fakeContainer{},
		volumes:    map[string]*fakeVolume{},
		opCounts:   map[string]int{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.run(ctx, h, token)
	return a
}

// addForeignContainer seeds a container this agent will report as NOT
// wildstone-owned (ContainerStats/StopContainer/RemoveContainer all
// FORBIDDEN on it), simulating an object Wardstone never created.
func (a *testAgent) addForeignContainer(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.containers[name] = &fakeContainer{image: "someone-elses:latest", running: true, owned: false}
}

// killContainer simulates out-of-band removal: the container vanishes
// from the fake entirely, as if `docker rm -f` ran outside Wardstone.
func (a *testAgent) killContainer(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.containers, name)
}

func (a *testAgent) containerExists(name string) (running bool, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.containers[name]
	if !ok {
		return false, false
	}
	return c.running, true
}

func (a *testAgent) volumeExists(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.volumes[name]
	return ok
}

func (a *testAgent) opCount(name string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.opCounts[name]
}

func (a *testAgent) countOp(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opCounts[name]++
}

// readFragmentForTest reads a Route fragment's on-disk bytes directly
// (bypassing the op surface) for test assertions. Returns nil if the
// fragment was never written. Mirrors routeFragmentPath/testManagedPrefix
// used by the Reconciler under test.
func (a *testAgent) readFragmentForTest(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join(a.managed, filepath.FromSlash(routeFragmentPath(testManagedPrefix, name)))
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("readFragmentForTest %q: %v", name, err)
	}
	return content
}

// readOidcFragmentForTest reads an OidcClient fragment's on-disk bytes
// directly (bypassing the op surface) for test assertions. Returns nil if
// the fragment was never written. Mirrors oidcClientFragmentPath/
// testManagedPrefix used by the Reconciler under test.
func (a *testAgent) readOidcFragmentForTest(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join(a.managed, filepath.FromSlash(oidcClientFragmentPath(testManagedPrefix, name)))
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("readOidcFragmentForTest %q: %v", name, err)
	}
	return content
}

// unreachableAddr returns a "host:port" that is guaranteed nothing is
// listening on: bind, learn the address, then close before returning it.
func unreachableAddr() (string, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := lis.Addr().String()
	lis.Close()
	return addr, nil
}

// ---------------------------------------------------------------- join + session

// run joins h's gateway and then serves the Session stream until ctx is
// cancelled, mirroring wardstone/internal/agent's join()/session() shape
// (generate key -> CSR -> Join RPC -> mTLS Session -> Hello -> op loop) at
// the minimum fidelity the Reconciler's tests need.
func (a *testAgent) run(ctx context.Context, h *harness, token string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		a.t.Errorf("testAgent %s: generate key: %v", a.nodeID, err)
		return
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: a.nodeID},
	}, key)
	if err != nil {
		a.t.Errorf("testAgent %s: create CSR: %v", a.nodeID, err)
		return
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	caPool := x509.NewCertPool()
	caPool.AddCert(mustParseCACert(a.t, h.gw))

	joinConn, err := grpc.NewClient(h.addr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: caPool, MinVersion: tls.VersionTLS13})))
	if err != nil {
		a.t.Errorf("testAgent %s: dial for join: %v", a.nodeID, err)
		return
	}
	defer joinConn.Close()
	resp, err := wsv1.NewKeystoneGatewayClient(joinConn).Join(ctx, &wsv1.JoinRequest{
		JoinToken: token,
		NodeId:    a.nodeID,
		CsrPem:    csrPEM,
	})
	if err != nil {
		// Some tests deliberately exercise a bad/reused token and never
		// expect a session; nothing more to do.
		return
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		a.t.Errorf("testAgent %s: marshal key: %v", a.nodeID, err)
		return
	}
	cert, err := tls.X509KeyPair(resp.GetCertPem(), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		a.t.Errorf("testAgent %s: build client cert: %v", a.nodeID, err)
		return
	}

	sessConn, err := grpc.NewClient(h.addr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      caPool,
			MinVersion:   tls.VersionTLS13,
		})))
	if err != nil {
		a.t.Errorf("testAgent %s: dial for session: %v", a.nodeID, err)
		return
	}
	defer sessConn.Close()

	stream, err := wsv1.NewKeystoneGatewayClient(sessConn).Session(ctx)
	if err != nil {
		a.t.Errorf("testAgent %s: open session: %v", a.nodeID, err)
		return
	}
	if err := stream.Send(&wsv1.AgentMessage{Msg: &wsv1.AgentMessage_Hello{Hello: &wsv1.Hello{
		NodeId: a.nodeID, AgentVersion: "test", ProtocolMinor: 4,
	}}}); err != nil {
		return
	}

	var sendMu sync.Mutex
	for {
		msg, err := stream.Recv()
		if err != nil {
			return
		}
		go func(m *wsv1.KeystoneMessage) {
			res := a.handleOp(m.GetOpId(), m.GetOp())
			sendMu.Lock()
			stream.Send(&wsv1.AgentMessage{Msg: &wsv1.AgentMessage_Result{Result: res}}) //nolint:errcheck
			sendMu.Unlock()
		}(msg)
	}
}

func mustParseCACert(t *testing.T, gw *gateway.Gateway) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(gw.CA().PEM())
	if block == nil {
		t.Fatal("gateway CA PEM did not decode")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse gateway CA: %v", err)
	}
	return cert
}

// ---------------------------------------------------------------- op handling

func opErrResult(opID string, code wsv1.ErrorCode, format string, args ...any) *wsv1.OpResult {
	return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_Error{
		Error: &wsv1.OpError{Code: code, Message: fmt.Sprintf(format, args...)},
	}}
}

func (a *testAgent) handleOp(opID string, op *wsv1.Operation) *wsv1.OpResult {
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_EnsureContainer:
		a.countOp("EnsureContainer")
		return a.ensureContainer(opID, o.EnsureContainer)
	case *wsv1.Operation_StopContainer:
		a.countOp("StopContainer")
		return a.stopContainer(opID, o.StopContainer)
	case *wsv1.Operation_RemoveContainer:
		a.countOp("RemoveContainer")
		return a.removeContainer(opID, o.RemoveContainer)
	case *wsv1.Operation_ContainerStats:
		a.countOp("ContainerStats")
		return a.containerStats(opID, o.ContainerStats)
	case *wsv1.Operation_InspectContainer:
		a.countOp("InspectContainer")
		return a.inspectContainer(opID, o.InspectContainer)
	case *wsv1.Operation_ListPorts:
		a.countOp("ListPorts")
		a.mu.Lock()
		defer a.mu.Unlock()
		return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_ListPorts{ListPorts: &wsv1.ListPortsResult{
			Ports: append([]*wsv1.ListeningPort(nil), a.listening...),
		}}}
	case *wsv1.Operation_HomeAutomationReadiness:
		a.countOp("HomeAutomationReadiness")
		a.mu.Lock()
		defer a.mu.Unlock()
		return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_HomeAutomationReadiness{HomeAutomationReadiness: &wsv1.HomeAutomationReadinessResult{
			PlatformSupported: true, Checks: append([]*wsv1.ReadinessCheck(nil), a.readiness...),
		}}}
	case *wsv1.Operation_EnsureSecret:
		return a.ensureSecret(opID, o.EnsureSecret)
	case *wsv1.Operation_ListSecrets:
		a.countOp("ListSecrets")
		return a.listSecrets(opID)
	case *wsv1.Operation_EnsureVolume:
		a.countOp("EnsureVolume")
		return a.ensureVolume(opID, o.EnsureVolume)
	case *wsv1.Operation_WriteManagedFile:
		a.countOp("WriteManagedFile")
		return a.writeManagedFile(opID, o.WriteManagedFile)
	case *wsv1.Operation_ReadManagedFile:
		a.countOp("ReadManagedFile")
		return a.readManagedFile(opID, o.ReadManagedFile)
	case *wsv1.Operation_ProbeHttp:
		a.countOp("ProbeHttp")
		return a.probeHttp(opID, o.ProbeHttp)
	case *wsv1.Operation_EnsureDockerNetwork:
		// The reconciler ensures the container network before every
		// container ensure (protocol minor 8); idempotent on the agent.
		a.countOp("EnsureDockerNetwork")
		return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_EnsureDockerNetwork{
			EnsureDockerNetwork: &wsv1.EnsureDockerNetworkResult{Created: true},
		}}
	case *wsv1.Operation_PullImage:
		// The reconciler pulls before every container ensure; the test
		// agent's "registry" always has the image.
		a.countOp("PullImage")
		return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_PullImage{
			PullImage: &wsv1.PullImageResult{Digest: o.PullImage.GetImage()},
		}}
	case *wsv1.Operation_Health:
		return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_Health{Health: &wsv1.HealthResult{Ok: true, Detail: "test agent"}}}
	default:
		return opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_INVALID, "test agent does not implement this op")
	}
}

func (a *testAgent) ensureContainer(opID string, op *wsv1.EnsureContainer) *wsv1.OpResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Host networking's exclusions, checked before the replace below as
	// the real agent does (ops.go), so a refused spec leaves the old
	// container in place.
	if op.GetNetworkMode() == wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST &&
		(len(op.GetPorts()) > 0 || op.GetNetwork() != "") {
		return opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_INVALID, "host network mode excludes ports and network")
	}
	existing, ok := a.containers[op.GetName()]
	if ok {
		if !existing.owned {
			return opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_FORBIDDEN, "container %q exists but is not wildstone-owned", op.GetName())
		}
		// Spec-hash-aware replace, same as the real agent (ops.go,
		// ADR-W008 R2c): the spec-hash label decides when present; image
		// comparison is the pre-minor-5 fallback.
		wantHash, hasHash := op.GetLabels()[specHashLabel]
		specMatches := existing.image == op.GetImage()
		if hasHash {
			specMatches = existing.labels[specHashLabel] == wantHash
		}
		if specMatches {
			started := !existing.running
			existing.running = true
			existing.status, existing.exitCode = "", 0
			a.runJobLocked(op.GetName(), existing)
			return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_EnsureContainer{EnsureContainer: &wsv1.EnsureContainerResult{
				ContainerId: "cid-" + op.GetName(), Created: false, Started: started,
			}}}
		}
		// spec changed: replace, same as the real agent (ops.go).
		delete(a.containers, op.GetName())
	}
	created := &fakeContainer{image: op.GetImage(), labels: op.GetLabels(), running: true, owned: true,
		network: op.GetNetwork(), networkMode: op.GetNetworkMode(), hostAliases: op.GetHostAliases(),
		mounts: op.GetMounts(), capAdd: op.GetCapAdd()}
	a.containers[op.GetName()] = created
	a.runJobLocked(op.GetName(), created)
	return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_EnsureContainer{EnsureContainer: &wsv1.EnsureContainerResult{
		ContainerId: "cid-" + op.GetName(), Created: true, Started: true,
	}}}
}

// requireOwnedLocked must be called with a.mu already held.
func (a *testAgent) requireOwnedLocked(opID, name string) (*fakeContainer, *wsv1.OpResult) {
	c, ok := a.containers[name]
	if !ok {
		return nil, opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "container %q", name)
	}
	if !c.owned {
		return nil, opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_FORBIDDEN, "container %q is not wildstone-owned", name)
	}
	return c, nil
}

func (a *testAgent) stopContainer(opID string, op *wsv1.StopContainer) *wsv1.OpResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, errRes := a.requireOwnedLocked(opID, op.GetName())
	if errRes != nil {
		return errRes
	}
	wasRunning := c.running
	c.running = false
	return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_StopContainer{StopContainer: &wsv1.StopContainerResult{WasRunning: wasRunning}}}
}

func (a *testAgent) removeContainer(opID string, op *wsv1.RemoveContainer) *wsv1.OpResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.containers[op.GetName()]
	if !ok {
		return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_RemoveContainer{RemoveContainer: &wsv1.RemoveContainerResult{Existed: false}}}
	}
	if !c.owned {
		return opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_FORBIDDEN, "container %q is not wildstone-owned", op.GetName())
	}
	delete(a.containers, op.GetName())
	return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_RemoveContainer{RemoveContainer: &wsv1.RemoveContainerResult{Existed: true}}}
}

// inspectContainer mirrors the real agent's arm (ops.go, ADR-W008 R2c):
// requireOwned semantics, then image/labels/running readback.
func (a *testAgent) inspectContainer(opID string, op *wsv1.InspectContainer) *wsv1.OpResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, errRes := a.requireOwnedLocked(opID, op.GetName())
	if errRes != nil {
		return errRes
	}
	// NetworkMode as Docker records it (minor 22): "host", or the network.
	networkMode := c.network
	if c.networkMode == wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST {
		networkMode = "host"
	}
	return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_InspectContainer{InspectContainer: &wsv1.InspectContainerResult{
		Running: c.running, Image: c.image, Labels: c.labels,
		Status: c.dockerStatus(), ExitCode: c.exitCode, NetworkMode: networkMode,
	}}}
}

// containerStats doubles as the Reconciler's presence+running observe/
// health-gate signal (package doc): it fails (NOT_FOUND) for both an
// absent AND a present-but-stopped container, since "running" is exactly
// what the Reconciler needs to know and the real Docker stats endpoint
// likewise doesn't serve stats for a stopped container.
func (a *testAgent) containerStats(opID string, op *wsv1.ContainerStats) *wsv1.OpResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, errRes := a.requireOwnedLocked(opID, op.GetName())
	if errRes != nil {
		return errRes
	}
	if !c.running {
		return opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "container %q is not running", op.GetName())
	}
	return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_ContainerStats{ContainerStats: &wsv1.ContainerStatsResult{
		CpuUsageNs: 1, MemUsageBytes: 1, MemLimitBytes: 1,
	}}}
}

func (a *testAgent) ensureVolume(opID string, op *wsv1.EnsureVolume) *wsv1.OpResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.volumes[op.GetName()]
	if ok {
		if !v.owned {
			return opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_FORBIDDEN, "volume %q exists but is not wildstone-owned", op.GetName())
		}
		return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_EnsureVolume{EnsureVolume: &wsv1.EnsureVolumeResult{Created: false}}}
	}
	a.volumes[op.GetName()] = &fakeVolume{owned: true}
	return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_EnsureVolume{EnsureVolume: &wsv1.EnsureVolumeResult{Created: true}}}
}

// fakeSecretRef mirrors the agent's own reference syntax closely enough
// to enforce the two rules that matter to the reconciler. It is a small
// duplicated constant rather than an import for the reason every other
// one in this file is: keystone cannot import wardstone/internal.
var fakeSecretRef = regexp.MustCompile(`(pbkdf2:)?secret/([A-Za-z0-9][A-Za-z0-9._-]{0,63})`)

// writeManagedFile enforces the agent's materialization rules, because a
// fake that ignores them makes the tests that depend on them prove
// nothing. Both of these were real failures on a node:
//
//   - materialize_secrets set on content with no reference is INVALID.
//     Asking for materialization with nothing to materialize is a
//     renderer bug, and the agent refuses rather than silently writing.
//   - a reference to a secret that does not exist is NOT_FOUND. This is
//     what makes the ordering test real: without it, a secret created
//     too late looks exactly like one created in time.
func (a *testAgent) writeManagedFile(opID string, op *wsv1.WriteManagedFile) *wsv1.OpResult {
	if op.GetMaterializeSecrets() {
		refs := fakeSecretRef.FindAllStringSubmatch(string(op.GetContent()), -1)
		if len(refs) == 0 {
			return opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_INVALID,
				"materialize_secrets set but the content carries no secret reference")
		}
		a.mu.Lock()
		for _, m := range refs {
			if _, ok := a.secrets[m[2]]; !ok {
				a.mu.Unlock()
				return opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_NOT_FOUND,
					"%s references secret/%s, which does not exist on this node", op.GetPath(), m[2])
			}
		}
		a.mu.Unlock()
	}
	path := filepath.Join(a.managed, filepath.FromSlash(op.GetPath()))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_RUNTIME, "mkdir: %v", err)
	}
	if err := os.WriteFile(path, op.GetContent(), 0o644); err != nil {
		return opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_RUNTIME, "write: %v", err)
	}
	return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_WriteManagedFile{WriteManagedFile: &wsv1.WriteManagedFileResult{}}}
}

func (a *testAgent) readManagedFile(opID string, op *wsv1.ReadManagedFile) *wsv1.OpResult {
	path := filepath.Join(a.managed, filepath.FromSlash(op.GetPath()))
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "%s", op.GetPath())
	}
	if err != nil {
		return opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_RUNTIME, "read: %v", err)
	}
	return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_ReadManagedFile{ReadManagedFile: &wsv1.ReadManagedFileResult{Content: content}}}
}

// probeHttp issues a real HTTP GET, same as wardstone's real agent, so
// tests can point a Route's upstream at a live httptest.Server and get a
// truthful reachability gate.
func (a *testAgent) probeHttp(opID string, op *wsv1.ProbeHttp) *wsv1.OpResult {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(op.GetUrl())
	if err != nil {
		return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_ProbeHttp{ProbeHttp: &wsv1.ProbeHttpResult{Reachable: false, Detail: err.Error()}}}
	}
	defer resp.Body.Close()
	return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_ProbeHttp{ProbeHttp: &wsv1.ProbeHttpResult{
		Reachable: true, StatusCode: uint32(resp.StatusCode),
	}}}
}

// dispatch is a small test helper: apply op against nodeID via gw and fail
// the test on a transport error (result-level errors are for the test to
// inspect, not this helper).
func dispatch(t *testing.T, gw *gateway.Gateway, nodeID string, op *wsv1.Operation) *wsv1.OpResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := gw.Dispatch(ctx, nodeID, op)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return res
}

// ---------------------------------------------------------------- fake jobs

// setJobExit registers a container name as a one-shot job: every time it
// is created or started, it "runs" instantly and exits with code. Call
// again with a different code to model a job that fails and then, once
// whatever it needed is in place, succeeds.
func (a *testAgent) setJobExit(name string, code int32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jobExit == nil {
		a.jobExit = map[string]int32{}
	}
	a.jobExit[name] = code
	if a.jobRuns == nil {
		a.jobRuns = map[string]int{}
	}
}

// jobRunCount reports how many times the named job has run. It is the
// assertion that matters most for a Job: a completed one must not run
// again, and a failed one must.
func (a *testAgent) jobRunCount(name string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.jobRuns[name]
}

// runJobLocked is called with a.mu held, from the create and start paths
// of ensureContainer — the two places a real container's process would
// begin.
func (a *testAgent) runJobLocked(name string, c *fakeContainer) {
	code, ok := a.jobExit[name]
	if !ok {
		return
	}
	if a.jobRuns == nil {
		a.jobRuns = map[string]int{}
	}
	a.jobRuns[name]++
	c.running = false
	c.status = "exited"
	c.exitCode = code
}

// ensureSecret mirrors the real agent's arm closely enough for the
// reconciler: create-if-absent, and report a version. The VALUE is never
// modelled, because nothing above the agent may ever see one.
func (a *testAgent) ensureSecret(opID string, op *wsv1.EnsureSecret) *wsv1.OpResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.secrets == nil {
		a.secrets = map[string]wsv1.SecretKind{}
	}
	_, existed := a.secrets[op.GetName()]
	if !existed || op.GetRotate() {
		a.secrets[op.GetName()] = op.GetKind()
	}
	a.opCounts["EnsureSecret"]++
	return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_EnsureSecret{EnsureSecret: &wsv1.EnsureSecretResult{
		Existed: existed, Version: 1,
	}}}
}

// listSecrets answers the op the reconciler uses to check secrets it is
// forbidden to generate.
func (a *testAgent) listSecrets(opID string) *wsv1.OpResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	res := &wsv1.ListSecretsResult{}
	for name := range a.secrets {
		res.Secrets = append(res.Secrets, &wsv1.SecretInfo{Name: name, Version: 1})
	}
	return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_ListSecrets{ListSecrets: res}}
}

// putSecret seeds a secret as though the operator had supplied it, which
// is the only way a provided secret ever comes to exist.
func (a *testAgent) putSecret(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.secrets == nil {
		a.secrets = map[string]wsv1.SecretKind{}
	}
	a.secrets[name] = wsv1.SecretKind_SECRET_KIND_RANDOM
}

// secretKind reports the kind a secret was ensured with, and whether it
// exists at all.
func (a *testAgent) secretKind(name string) (wsv1.SecretKind, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k, ok := a.secrets[name]
	return k, ok
}

// setListening replaces what ListPorts reports.
func (a *testAgent) setListening(ports ...*wsv1.ListeningPort) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listening = ports
}

// setReadiness replaces what HomeAutomationReadiness reports.
func (a *testAgent) setReadiness(checks ...*wsv1.ReadinessCheck) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.readiness = checks
}

// containerHostAccess reports the mounts and added capabilities the named
// container was created with.
func (a *testAgent) containerHostAccess(name string) (mounts []*wsv1.Mount, capAdd []string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.containers[name]
	if !ok {
		return nil, nil, false
	}
	return c.mounts, c.capAdd, true
}

// containerNetworking reports what the named container was created with.
func (a *testAgent) containerNetworking(name string) (network string, mode wsv1.ContainerNetworkMode, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.containers[name]
	if !ok {
		return "", 0, false
	}
	return c.network, c.networkMode, true
}

// containerHostAliases reports the host_aliases the named container was
// created with.
func (a *testAgent) containerHostAliases(name string) ([]string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.containers[name]
	if !ok {
		return nil, false
	}
	return append([]string(nil), c.hostAliases...), true
}
