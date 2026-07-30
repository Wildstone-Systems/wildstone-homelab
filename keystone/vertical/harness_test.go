package vertical

// A minimal real Wardstone agent for the go/no-go gate test (ADR-W005 S3c):
// real join + real mTLS Session stream against a real gateway.Gateway,
// exactly as keystone/reconcile's own integration tests do (ADR-W004 B1:
// "reuse the S1/E1 real-gateway+fake-docker test style"). keystone/vertical
// cannot reach keystone/reconcile's test-only (_test.go) helpers — they are
// unexported to that package — so this file recreates the same minimal
// protocol client rather than depending on reconcile package internals.
// This is deliberate: the gate test exercises the REAL Reconciler
// (keystone/reconcile, imported normally) against a REAL gateway and a
// faked Docker/filesystem substrate, which is the only place any
// duplication happens. Nothing about the Reconciler's apply/observe/health-
// gate logic is reimplemented here.

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
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/chickenrat17/wildstone-homelab/keystone/gateway"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// ---------------------------------------------------------------- gateway harness

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
	running bool
	owned   bool
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
}

func newTestAgent(t *testing.T, h *harness, nodeID, token string) *testAgent {
	t.Helper()
	a := &testAgent{
		t:          t,
		nodeID:     nodeID,
		managed:    t.TempDir(),
		containers: map[string]*fakeContainer{},
		volumes:    map[string]*fakeVolume{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.run(ctx, h, token)
	return a
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

// readManagedFileForTest reads a managed-file's on-disk bytes directly
// (bypassing the op surface) for test assertions. Returns nil, false if the
// file was never written. path is the same managed-prefix-relative path the
// Reconciler itself writes to (e.g. via oidcClientFragmentPath/
// routeFragmentPath's documented convention — see path helpers in
// vertical_test.go).
func (a *testAgent) readManagedFileForTest(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	full := filepath.Join(a.managed, filepath.FromSlash(path))
	content, err := os.ReadFile(full)
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("readManagedFileForTest %q: %v", path, err)
	}
	return content, true
}

// ---------------------------------------------------------------- join + session

// run joins h's gateway and then serves the Session stream until ctx is
// cancelled, mirroring wardstone/internal/agent's join()/session() shape
// (generate key -> CSR -> Join RPC -> mTLS Session -> Hello -> op loop) at
// the minimum fidelity this test needs.
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
		return a.ensureContainer(opID, o.EnsureContainer)
	case *wsv1.Operation_StopContainer:
		return a.stopContainer(opID, o.StopContainer)
	case *wsv1.Operation_RemoveContainer:
		return a.removeContainer(opID, o.RemoveContainer)
	case *wsv1.Operation_ContainerStats:
		return a.containerStats(opID, o.ContainerStats)
	case *wsv1.Operation_EnsureVolume:
		return a.ensureVolume(opID, o.EnsureVolume)
	case *wsv1.Operation_WriteManagedFile:
		return a.writeManagedFile(opID, o.WriteManagedFile)
	case *wsv1.Operation_ReadManagedFile:
		return a.readManagedFile(opID, o.ReadManagedFile)
	case *wsv1.Operation_ProbeHttp:
		return a.probeHttp(opID, o.ProbeHttp)
	case *wsv1.Operation_Health:
		return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_Health{Health: &wsv1.HealthResult{Ok: true, Detail: "test agent"}}}
	default:
		return opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_INVALID, "test agent does not implement this op")
	}
}

func (a *testAgent) ensureContainer(opID string, op *wsv1.EnsureContainer) *wsv1.OpResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	existing, ok := a.containers[op.GetName()]
	if ok {
		if !existing.owned {
			return opErrResult(opID, wsv1.ErrorCode_ERROR_CODE_FORBIDDEN, "container %q exists but is not wildstone-owned", op.GetName())
		}
		if existing.image == op.GetImage() {
			started := !existing.running
			existing.running = true
			return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_EnsureContainer{EnsureContainer: &wsv1.EnsureContainerResult{
				ContainerId: "cid-" + op.GetName(), Created: false, Started: started,
			}}}
		}
		delete(a.containers, op.GetName())
	}
	a.containers[op.GetName()] = &fakeContainer{image: op.GetImage(), running: true, owned: true}
	return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_EnsureContainer{EnsureContainer: &wsv1.EnsureContainerResult{
		ContainerId: "cid-" + op.GetName(), Created: true, Started: true,
	}}}
}

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

func (a *testAgent) writeManagedFile(opID string, op *wsv1.WriteManagedFile) *wsv1.OpResult {
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

// probeHttp issues a real HTTP GET, so a Route's ip:port upstream can be
// pointed at a live httptest.Server for a truthful reachability gate.
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
