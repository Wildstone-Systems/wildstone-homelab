package butler

// A minimal real Wardstone agent for keystone/butler's tests, mirroring
// keystone/reconcile's testagent_test.go pattern (ADR-W006 B1: "reuse the
// S1/E1 real-gateway+fake-docker harness"). It cannot be imported from
// reconcile's _test.go file (test-only, unexported, different package), so
// this recreates just the subset the Butler's read tools actually use:
// real join + real mTLS Session against a real gateway.Gateway, answering
// only Health and ContainerStats against in-memory fake state — no other
// op is implemented, matching AgentReader's own read-only surface.

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
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/gateway"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

type butlerHarness struct {
	gw   *gateway.Gateway
	addr string
}

func startButlerGateway(t *testing.T, tokens []string) *butlerHarness {
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
	h := &butlerHarness{gw: gw, addr: lis.Addr().String()}
	go gw.Serve(lis) //nolint:errcheck
	t.Cleanup(gw.Stop)
	return h
}

type fakeContainer struct {
	running       bool
	cpuUsageNs    uint64
	memUsageBytes uint64
	memLimitBytes uint64
}

// butlerTestAgent answers Health and ContainerStats against in-memory fake
// state. Any other op it receives is reported as unimplemented — matching
// the read-only surface AgentReader actually exercises.
type butlerTestAgent struct {
	t      *testing.T
	nodeID string

	mu         sync.Mutex
	containers map[string]*fakeContainer
	healthOK   bool
	healthMsg  string
}

func newButlerTestAgent(t *testing.T, h *butlerHarness, nodeID, token string) *butlerTestAgent {
	t.Helper()
	a := &butlerTestAgent{
		t:          t,
		nodeID:     nodeID,
		containers: map[string]*fakeContainer{},
		healthOK:   true,
		healthMsg:  "all good",
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.run(ctx, h, token)
	return a
}

func (a *butlerTestAgent) setContainer(name string, running bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.containers[name] = &fakeContainer{running: running, cpuUsageNs: 42, memUsageBytes: 1024, memLimitBytes: 2048}
}

func (a *butlerTestAgent) setHealth(ok bool, msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.healthOK = ok
	a.healthMsg = msg
}

func (a *butlerTestAgent) run(ctx context.Context, h *butlerHarness, token string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		a.t.Errorf("butlerTestAgent %s: generate key: %v", a.nodeID, err)
		return
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: a.nodeID},
	}, key)
	if err != nil {
		a.t.Errorf("butlerTestAgent %s: create CSR: %v", a.nodeID, err)
		return
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	caPool := x509.NewCertPool()
	caPool.AddCert(mustParseButlerCACert(a.t, h.gw))

	joinConn, err := grpc.NewClient(h.addr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: caPool, MinVersion: tls.VersionTLS13})))
	if err != nil {
		a.t.Errorf("butlerTestAgent %s: dial for join: %v", a.nodeID, err)
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
		a.t.Errorf("butlerTestAgent %s: marshal key: %v", a.nodeID, err)
		return
	}
	cert, err := tls.X509KeyPair(resp.GetCertPem(), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		a.t.Errorf("butlerTestAgent %s: build client cert: %v", a.nodeID, err)
		return
	}

	sessConn, err := grpc.NewClient(h.addr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      caPool,
			MinVersion:   tls.VersionTLS13,
		})))
	if err != nil {
		a.t.Errorf("butlerTestAgent %s: dial for session: %v", a.nodeID, err)
		return
	}
	defer sessConn.Close()

	stream, err := wsv1.NewKeystoneGatewayClient(sessConn).Session(ctx)
	if err != nil {
		a.t.Errorf("butlerTestAgent %s: open session: %v", a.nodeID, err)
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

func mustParseButlerCACert(t *testing.T, gw *gateway.Gateway) *x509.Certificate {
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

func butlerOpErrResult(opID string, code wsv1.ErrorCode, format string, args ...any) *wsv1.OpResult {
	return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_Error{
		Error: &wsv1.OpError{Code: code, Message: fmt.Sprintf(format, args...)},
	}}
}

func (a *butlerTestAgent) handleOp(opID string, op *wsv1.Operation) *wsv1.OpResult {
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_Health:
		a.mu.Lock()
		ok, msg := a.healthOK, a.healthMsg
		a.mu.Unlock()
		return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_Health{Health: &wsv1.HealthResult{Ok: ok, Detail: msg}}}
	case *wsv1.Operation_ContainerStats:
		a.mu.Lock()
		c, ok := a.containers[o.ContainerStats.GetName()]
		a.mu.Unlock()
		if !ok || !c.running {
			return butlerOpErrResult(opID, wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "container %q", o.ContainerStats.GetName())
		}
		return &wsv1.OpResult{OpId: opID, Result: &wsv1.OpResult_ContainerStats{ContainerStats: &wsv1.ContainerStatsResult{
			CpuUsageNs: c.cpuUsageNs, MemUsageBytes: c.memUsageBytes, MemLimitBytes: c.memLimitBytes,
		}}}
	default:
		return butlerOpErrResult(opID, wsv1.ErrorCode_ERROR_CODE_INVALID, "butler test agent only implements Health/ContainerStats")
	}
}
