// Package gateway is Keystone's side of the Wardstone protocol: the gRPC
// server that agents dial out to (ADR-W003). It owns the embedded CA, the
// join-token exchange, and the registry of live agent sessions.
package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// opSignDomainPrefix domain-separates the per-operation signature (ADR-W003
// W1). Keep in sync with the .proto comment on KeystoneMessage.op_signature.
const opSignDomainPrefix = "wardstone-op-v1"

type Config struct {
	// JoinTokens maps token -> allowed. Static, non-expiring, single-use
	// per process, and never written to tokens.json — the dev/test path.
	// MintJoinToken is the documented production path (ADR-W003 W2):
	// minted tokens have a TTL and are persisted to DataDir/tokens.json.
	JoinTokens []string
	// Hosts are SANs for the gateway's serving certificate.
	Hosts  []string
	Logger *slog.Logger

	// DataDir, if set, persists the CA (ca.crt/ca.key), the op-signing
	// keypair (op-signing.key/op-signing.pub), and minted join tokens
	// (tokens.json) across restarts (ADR-W003 W2). Empty DataDir keeps the
	// pre-W2 in-memory-only behavior: fresh CA and signing key every
	// process start, minted tokens not persisted.
	DataDir string

	// opSigningKey lets a caller in this package supply a previously-
	// persisted ed25519 signing key instead of having New generate a
	// fresh one. Unexported: external callers always go through DataDir
	// (or get a freshly generated in-memory key when DataDir is empty).
	// Used by New's own DataDir-loading path; not settable from outside
	// the package.
	opSigningKey ed25519.PrivateKey
}

type Gateway struct {
	wsv1.UnimplementedKeystoneGatewayServer

	ca  *CA
	cfg Config
	log *slog.Logger

	signPub  ed25519.PublicKey
	signPriv ed25519.PrivateKey

	mu       sync.Mutex
	tokens   map[string]bool // static Config.JoinTokens -> still valid (in-memory only)
	sessions map[string]*session
	opSeq    uint64

	// opGen is a per-process random generation token (ADR-W003 W1 replay
	// hardening). op_id is formatted "op-<opGen>-<opSeq>": opSeq alone
	// resets to zero on every process start, but the op-signing key
	// persists across restarts (persist.go). Without opGen a restarted
	// gateway would re-issue op-1, op-2, … and a signature captured off
	// the wire before the restart would verify again against a
	// freshly-issued op_id — a replay a transport MITM could mount. A
	// random opGen per process namespaces op_ids so two gateway
	// generations never share one, and a pre-restart signature can never
	// match a post-restart op_id. Set once in New; read-only thereafter.
	opGen string

	tokenStore *tokenStore // minted tokens (ADR-W003 W2); own mutex

	// auditMu guards <DataDir>/audit.log (ADR-W003 W7): Dispatch can run
	// concurrently for different nodes, and appendAudit's rotate-then-append
	// sequence must be atomic with respect to other appenders.
	auditMu sync.Mutex

	server *grpc.Server
}

type session struct {
	nodeID  string
	send    func(*wsv1.KeystoneMessage) error
	pending map[string]chan *wsv1.OpResult
	mu      sync.Mutex
}

func New(cfg Config) (*Gateway, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	var ca *CA
	if cfg.DataDir != "" {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
		if err := os.Chmod(cfg.DataDir, 0o700); err != nil { // MkdirAll's mode is subject to umask
			return nil, fmt.Errorf("chmod data dir: %w", err)
		}
		var err error
		var loaded bool
		ca, loaded, err = loadOrCreateCA(cfg.DataDir)
		if err != nil {
			return nil, fmt.Errorf("CA: %w", err)
		}
		if loaded {
			cfg.Logger.Info("keystone CA loaded from data dir", "data_dir", cfg.DataDir)
		} else {
			cfg.Logger.Info("keystone CA created", "data_dir", cfg.DataDir)
		}
		if cfg.opSigningKey == nil {
			cfg.opSigningKey, loaded, err = loadOrCreateSigningKey(cfg.DataDir)
			if err != nil {
				return nil, fmt.Errorf("op-signing key: %w", err)
			}
			if loaded {
				cfg.Logger.Info("op-signing key loaded from data dir", "data_dir", cfg.DataDir)
			} else {
				cfg.Logger.Info("op-signing key created", "data_dir", cfg.DataDir)
			}
		}
	} else {
		var err error
		ca, err = NewCA()
		if err != nil {
			return nil, err
		}
	}
	// ADR-W003 W3: print the CA fingerprint at startup so an operator can
	// hand it to `wardstoned --ca-fingerprint` and pin the join.
	cfg.Logger.Info("keystone CA fingerprint", "sha256", ca.Fingerprint())

	tokens := make(map[string]bool, len(cfg.JoinTokens))
	for _, t := range cfg.JoinTokens {
		tokens[t] = true
	}
	tokStore, err := loadTokenStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("join tokens: %w", err)
	}

	signPriv := cfg.opSigningKey
	if signPriv == nil {
		var err error
		_, signPriv, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate op-signing key: %w", err)
		}
	}
	signPub := signPriv.Public().(ed25519.PublicKey)
	opGen, err := newOpGen()
	if err != nil {
		return nil, fmt.Errorf("generate op-id generation token: %w", err)
	}
	return &Gateway{
		ca: ca, cfg: cfg, log: cfg.Logger, tokens: tokens, sessions: map[string]*session{},
		tokenStore: tokStore,
		signPub:    signPub, signPriv: signPriv,
		opGen: opGen,
	}, nil
}

// newOpGen returns a short random hex generation token that namespaces this
// process's op_ids (see the Gateway.opGen field). Six random bytes → 12 hex
// chars: enough that two gateway generations sharing the same op-signing key
// across a restart never collide on an op_id.
func newOpGen() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// MintJoinToken creates a new single-use join token with the given TTL —
// the documented production path for admitting a node (ADR-W003 W2). The
// token is 32 random bytes, hex-encoded, and (when Config.DataDir is set)
// persisted to tokens.json before this returns, surviving a Keystone
// restart until it is used or expires.
func (g *Gateway) MintJoinToken(ttl time.Duration) (string, error) {
	return g.tokenStore.mint(ttl)
}

func (g *Gateway) CA() *CA { return g.ca }

// OpSigningPublicKey returns the ed25519 public key agents pin at join to
// verify per-operation signatures (ADR-W003 W1).
func (g *Gateway) OpSigningPublicKey() ed25519.PublicKey { return g.signPub }

// signOp computes the ADR-W003 W1 per-operation signature:
// ed25519(sha256("wardstone-op-v1" || op_id || deterministic-marshal(op))).
func (g *Gateway) signOp(opID string, op *wsv1.Operation) ([]byte, error) {
	payload, err := opSignPayload(opID, op)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(g.signPriv, payload), nil
}

// SignOp exposes signOp for tests that need a validly-signed payload to
// then tamper with (e.g. reusing a valid signature against a different
// op_id or a different Operation) — see ADR-W003 W1's tamper tests. Not
// used by any non-test code path; Dispatch signs internally.
func (g *Gateway) SignOp(opID string, op *wsv1.Operation) ([]byte, error) {
	return g.signOp(opID, op)
}

// opSignPayload builds the digest signed/verified for one operation. Shared
// shape between Keystone (signs) and Wardstone (verifies) — kept as a small
// duplicated helper on each side rather than a shared module dependency
// (ADR-W003 §A4.5 keeps wardstone's dependency surface minimal).
func opSignPayload(opID string, op *wsv1.Operation) ([]byte, error) {
	opBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(op)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(opSignDomainPrefix))
	h.Write([]byte(opID))
	h.Write(opBytes)
	return h.Sum(nil), nil
}

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
	// Message size headroom (ADR-W003 W5): SelfUpdate ships the new binary
	// inline on the unary-op stream. Wardstone binaries are ~10-20 MiB
	// static; 64 MiB leaves comfortable headroom without needing chunked
	// delivery yet (see the .proto comment on SelfUpdate).
	const maxUpdateMsgBytes = 64 << 20
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.MaxRecvMsgSize(maxUpdateMsgBytes),
		grpc.MaxSendMsgSize(maxUpdateMsgBytes),
	)
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
		// Not a static dev token (or already spent); try the minted,
		// persisted token store (ADR-W003 W2). consume() marks the token
		// used and persists that mark to disk before returning true, so a
		// crash right after this point still leaves the token correctly
		// spent on restart.
		ok = g.tokenStore.consume(req.GetJoinToken())
	}
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "invalid, used, or expired join token")
	}
	certPEM, err := g.ca.IssueFromCSR(req.GetCsrPem(), req.GetNodeId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "csr: %v", err)
	}
	g.log.Info("node joined", "node", req.GetNodeId())
	return &wsv1.JoinResponse{
		CertPem:            certPEM,
		CaPem:              g.ca.PEM(),
		OpSigningPublicKey: g.signPub,
	}, nil
}

// ---------------------------------------------------------------- Renew

// Renew issues a fresh client certificate for an already-authenticated node
// ahead of its current certificate's expiry (ADR-W003 W4). The verified
// mTLS peer cert's CN is the ONLY source of node identity — never the CSR
// subject and never anything in the request — so a node cannot use renewal
// to change its identity.
func (g *Gateway) Renew(ctx context.Context, req *wsv1.RenewRequest) (*wsv1.RenewResponse, error) {
	nodeID, err := peerNodeID(ctx)
	if err != nil {
		return nil, err
	}
	certPEM, err := g.ca.IssueFromCSR(req.GetCsrPem(), nodeID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "csr: %v", err)
	}
	g.log.Info("node renewed certificate", "node", nodeID)
	return &wsv1.RenewResponse{CertPem: certPEM}, nil
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

// Dispatch sends one operation to a connected agent and waits for its
// result. The operation is signed per ADR-W003 W1 before it goes on the
// wire.
func (g *Gateway) Dispatch(ctx context.Context, nodeID string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	g.mu.Lock()
	g.opSeq++
	opID := fmt.Sprintf("op-%s-%d", g.opGen, g.opSeq)
	g.mu.Unlock()
	sig, err := g.signOp(opID, op)
	if err != nil {
		return nil, fmt.Errorf("sign op: %w", err)
	}
	res, err := g.DispatchRaw(ctx, nodeID, &wsv1.KeystoneMessage{OpId: opID, Op: op, OpSignature: sig})
	g.auditDispatch(nodeID, opID, op, res, err)
	return res, err
}

// maxDispatchMsgBytes mirrors the grpc.MaxSendMsgSize/MaxRecvMsgSize server
// option and the agent's matching MaxCallRecvMsgSize (ADR-W003 W5, see
// gateway.Serve and the agent's session/join dial options). DispatchRaw
// checks against it BEFORE calling stream.Send: gRPC's SendMsg aborts the
// whole stream on error (see grpc-go's ClientStream/ServerStream docs), so
// letting an oversized message reach Send would tear down the agent's live
// session as a side effect of one rejected op. Checking first keeps an
// oversized-op rejection to "this Dispatch call failed" instead of
// "the agent disconnected".
const maxDispatchMsgBytes = 64 << 20

// DispatchRaw sends an already-built KeystoneMessage (op_id, op, and
// whatever signature the caller supplies) to a connected agent and waits
// for the correlated result. Dispatch is the normal entry point and
// delegates here after computing a valid signature; DispatchRaw itself is
// exported so tests can exercise a hand-corrupted message (wrong op_id,
// tampered op bytes, missing signature) without standing up a second gRPC
// client — see ADR-W003 W1's tamper tests in the agent package.
func (g *Gateway) DispatchRaw(ctx context.Context, nodeID string, msg *wsv1.KeystoneMessage) (*wsv1.OpResult, error) {
	if sz := proto.Size(msg); sz > maxDispatchMsgBytes {
		return nil, fmt.Errorf("op message too large: %d bytes exceeds the %d byte gRPC message cap", sz, maxDispatchMsgBytes)
	}
	g.mu.Lock()
	s := g.sessions[nodeID]
	g.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("node %q is not connected", nodeID)
	}
	ch := make(chan *wsv1.OpResult, 1)
	s.mu.Lock()
	s.pending[msg.GetOpId()] = ch
	s.mu.Unlock()
	if err := s.send(msg); err != nil {
		s.mu.Lock()
		delete(s.pending, msg.GetOpId())
		s.mu.Unlock()
		return nil, err
	}
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, msg.GetOpId())
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
