// Package gateway is Keystone's side of the Wardstone protocol: the gRPC
// server that agents dial out to (ADR-W003). It owns the embedded CA, the
// join-token exchange, and the registry of live agent sessions.
package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

type Config struct {
	// JoinTokens maps token -> allowed (spike: static single-use set;
	// E2: minted per node with expiry, persisted).
	JoinTokens []string
	// Hosts are SANs for the gateway's serving certificate.
	Hosts []string
	// OpSigningPublicKey is handed to agents at join (unused until E1).
	OpSigningPublicKey []byte
	Logger             *slog.Logger
}

type Gateway struct {
	wsv1.UnimplementedKeystoneGatewayServer

	ca  *CA
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	tokens   map[string]bool // token -> still valid
	sessions map[string]*session
	opSeq    uint64

	server *grpc.Server
}

type session struct {
	nodeID  string
	send    func(*wsv1.KeystoneMessage) error
	pending map[string]chan *wsv1.OpResult
	mu      sync.Mutex
}

func New(cfg Config) (*Gateway, error) {
	ca, err := NewCA()
	if err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	tokens := make(map[string]bool, len(cfg.JoinTokens))
	for _, t := range cfg.JoinTokens {
		tokens[t] = true
	}
	return &Gateway{ca: ca, cfg: cfg, log: cfg.Logger, tokens: tokens, sessions: map[string]*session{}}, nil
}

func (g *Gateway) CA() *CA { return g.ca }

// Serve blocks, serving gRPC on lis until Stop is called.
func (g *Gateway) Serve(lis net.Listener) error {
	serverCert, err := g.ca.ServerTLSCert(g.cfg.Hosts)
	if err != nil {
		return err
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    g.ca.Pool(),
		// Join runs before the node has a certificate, so client certs are
		// optional at the TLS layer; Session enforces one per-RPC.
		ClientAuth: tls.VerifyClientCertIfGiven,
		MinVersion: tls.VersionTLS13,
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	wsv1.RegisterKeystoneGatewayServer(srv, g)
	g.mu.Lock()
	g.server = srv
	g.mu.Unlock()
	return srv.Serve(lis)
}

func (g *Gateway) Stop() {
	g.mu.Lock()
	srv := g.server
	g.mu.Unlock()
	if srv != nil {
		srv.Stop()
	}
}

// ---------------------------------------------------------------- Join

func (g *Gateway) Join(_ context.Context, req *wsv1.JoinRequest) (*wsv1.JoinResponse, error) {
	if req.GetNodeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "node_id required")
	}
	g.mu.Lock()
	ok := g.tokens[req.GetJoinToken()]
	if ok {
		g.tokens[req.GetJoinToken()] = false // single use
	}
	g.mu.Unlock()
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "invalid or used join token")
	}
	certPEM, err := g.ca.IssueFromCSR(req.GetCsrPem(), req.GetNodeId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "csr: %v", err)
	}
	g.log.Info("node joined", "node", req.GetNodeId())
	return &wsv1.JoinResponse{
		CertPem:            certPEM,
		CaPem:              g.ca.PEM(),
		OpSigningPublicKey: g.cfg.OpSigningPublicKey,
	}, nil
}

// ---------------------------------------------------------------- Session

func (g *Gateway) Session(stream grpc.BidiStreamingServer[wsv1.AgentMessage, wsv1.KeystoneMessage]) error {
	nodeID, err := peerNodeID(stream.Context())
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be Hello")
	}
	if hello.GetNodeId() != nodeID {
		return status.Errorf(codes.PermissionDenied,
			"hello node_id %q does not match certificate identity %q", hello.GetNodeId(), nodeID)
	}

	var sendMu sync.Mutex
	s := &session{
		nodeID:  nodeID,
		pending: map[string]chan *wsv1.OpResult{},
		send: func(m *wsv1.KeystoneMessage) error {
			sendMu.Lock()
			defer sendMu.Unlock()
			return stream.Send(m)
		},
	}
	g.mu.Lock()
	g.sessions[nodeID] = s
	g.mu.Unlock()
	g.log.Info("agent connected", "node", nodeID, "version", hello.GetAgentVersion(), "protocol_minor", hello.GetProtocolMinor())
	defer func() {
		g.mu.Lock()
		if g.sessions[nodeID] == s {
			delete(g.sessions, nodeID)
		}
		g.mu.Unlock()
		g.log.Info("agent disconnected", "node", nodeID)
	}()

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		res := msg.GetResult()
		if res == nil {
			continue
		}
		s.mu.Lock()
		ch := s.pending[res.GetOpId()]
		delete(s.pending, res.GetOpId())
		s.mu.Unlock()
		if ch != nil {
			ch <- res
		}
	}
}

// Dispatch sends one operation to a connected agent and waits for its result.
func (g *Gateway) Dispatch(ctx context.Context, nodeID string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	g.mu.Lock()
	s := g.sessions[nodeID]
	g.opSeq++
	opID := fmt.Sprintf("op-%d", g.opSeq)
	g.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("node %q is not connected", nodeID)
	}
	ch := make(chan *wsv1.OpResult, 1)
	s.mu.Lock()
	s.pending[opID] = ch
	s.mu.Unlock()
	if err := s.send(&wsv1.KeystoneMessage{OpId: opID, Op: op}); err != nil {
		s.mu.Lock()
		delete(s.pending, opID)
		s.mu.Unlock()
		return nil, err
	}
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, opID)
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Connected reports whether nodeID currently holds a session.
func (g *Gateway) Connected(nodeID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sessions[nodeID] != nil
}

// WaitConnected blocks until nodeID has a session or the timeout elapses.
func (g *Gateway) WaitConnected(nodeID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if g.Connected(nodeID) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// peerNodeID extracts the node identity from the verified client cert.
func peerNodeID(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "no peer")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return "", status.Error(codes.Unauthenticated, "session requires a client certificate issued at join")
	}
	return tlsInfo.State.VerifiedChains[0][0].Subject.CommonName, nil
}
