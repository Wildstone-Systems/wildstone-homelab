// Package agent implements wardstoned: the privileged host agent. It dials
// out to Keystone over mTLS gRPC (never listens), bootstraps its identity
// with a one-time join token, and serves the closed typed operation
// surface defined in proto/wardstone/v1 (ADR-W003).
package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// ProtocolMinor is the v1 minor revision this agent speaks.
const ProtocolMinor = 0

type Config struct {
	NodeID       string
	KeystoneAddr string // host:port
	DataDir      string // certs + CA live here
	JoinToken    string // consumed on first run only

	// CAFingerprint, if set, is the hex-encoded sha256 of Keystone's CA
	// certificate DER. When set, join uses CA pinning instead of TOFU
	// (ADR-W003 W3): a preflight TLS dial verifies the gateway's presented
	// CA against this fingerprint BEFORE the join token is sent, so a
	// wrong fingerprint fails without spending the token. When unset, join
	// falls back to trust-on-first-use and logs a loud Warn.
	CAFingerprint string

	ManagedPrefixes []string // default: /etc/wildstone, /srv/wildstone
	DockerEndpoint  string   // default: unix:///var/run/docker.sock
	BinaryPath      string   // self-update target; default: os.Executable()
	UpdatePublicKey []byte   // ed25519, baked in at build in production
	Version         string

	// Reconnect backoff (full jitter). Defaults: 500ms base, 15s cap.
	BackoffBase time.Duration
	BackoffCap  time.Duration

	Logger *slog.Logger
}

type Agent struct {
	cfg    Config
	log    *slog.Logger
	docker *dockerClient
}

func New(cfg Config) (*Agent, error) {
	if cfg.NodeID == "" || cfg.KeystoneAddr == "" || cfg.DataDir == "" {
		return nil, fmt.Errorf("NodeID, KeystoneAddr and DataDir are required")
	}
	if len(cfg.ManagedPrefixes) == 0 {
		cfg.ManagedPrefixes = []string{"/etc/wildstone", "/srv/wildstone"}
	}
	for i, p := range cfg.ManagedPrefixes {
		cfg.ManagedPrefixes[i] = filepath.Clean(p)
	}
	if cfg.DockerEndpoint == "" {
		cfg.DockerEndpoint = "unix:///var/run/docker.sock"
	}
	if cfg.BinaryPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve own binary: %w", err)
		}
		cfg.BinaryPath = exe
	}
	if cfg.BackoffBase == 0 {
		cfg.BackoffBase = 500 * time.Millisecond
	}
	if cfg.BackoffCap == 0 {
		cfg.BackoffCap = 15 * time.Second
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	docker, err := newDockerClient(cfg.DockerEndpoint)
	if err != nil {
		return nil, err
	}
	return &Agent{cfg: cfg, log: cfg.Logger.With("node", cfg.NodeID), docker: docker}, nil
}

// Run keeps a session with Keystone alive until ctx is cancelled,
// bootstrapping identity on first run.
func (a *Agent) Run(ctx context.Context) error {
	if err := os.MkdirAll(a.cfg.DataDir, 0o700); err != nil {
		return err
	}
	if !a.hasIdentity() {
		if err := a.join(ctx); err != nil {
			return fmt.Errorf("join: %w", err)
		}
	}
	attempt := 0
	for {
		err := a.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attempt++
		delay := a.backoff(attempt)
		a.log.Debug("session ended, reconnecting", "error", err, "delay", delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// backoff returns a full-jitter delay for the given attempt number.
func (a *Agent) backoff(attempt int) time.Duration {
	max := a.cfg.BackoffBase << min(attempt, 20)
	if max > a.cfg.BackoffCap {
		max = a.cfg.BackoffCap
	}
	return time.Duration(mrand.Int64N(int64(max)) + 1)
}

// ---------------------------------------------------------------- identity

func (a *Agent) certPath() string         { return filepath.Join(a.cfg.DataDir, "node.crt") }
func (a *Agent) keyPath() string          { return filepath.Join(a.cfg.DataDir, "node.key") }
func (a *Agent) caPath() string           { return filepath.Join(a.cfg.DataDir, "keystone-ca.crt") }
func (a *Agent) opSigningPubPath() string { return filepath.Join(a.cfg.DataDir, "op-signing.pub") }

func (a *Agent) hasIdentity() bool {
	for _, p := range []string{a.certPath(), a.keyPath(), a.caPath()} {
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}

// join performs the one-time bootstrap: generate a key, send CSR + join
// token, persist the issued certificate and Keystone's CA.
//
// Two trust modes (ADR-W003 W3):
//
//   - CAFingerprint set: CA pinning. A preflight plain TLS dial (§preflightPinnedCA)
//     captures the CA certificate the gateway serves alongside its leaf
//     (see CA.ServerTLSCert) and verifies it against the configured
//     fingerprint BEFORE any RPC — in particular before the join token is
//     sent, so a wrong fingerprint never spends the token. The join RPC
//     itself then dials with full TLS verification (RootCAs = the pinned
//     CA only, no InsecureSkipVerify). As belt-and-braces, the CA returned
//     in JoinResponse.ca_pem is also checked against the pinned cert
//     before anything is persisted.
//   - CAFingerprint unset: trust on first use — the join dial does not
//     verify the server (the agent has no CA yet); the CA received in the
//     response is pinned for every subsequent connection. Logged loudly:
//     this is not safe against a join-time MITM.
func (a *Agent) join(ctx context.Context) error {
	if a.cfg.JoinToken == "" {
		return fmt.Errorf("no identity on disk and no join token configured")
	}

	var joinTLS *tls.Config
	var pinnedCA *x509.Certificate
	if a.cfg.CAFingerprint != "" {
		var err error
		pinnedCA, err = preflightPinnedCA(ctx, a.cfg.KeystoneAddr, a.cfg.CAFingerprint)
		if err != nil {
			return fmt.Errorf("CA pinning preflight: %w", err)
		}
		pool := x509.NewCertPool()
		pool.AddCert(pinnedCA)
		joinTLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
	} else {
		a.log.Warn("TOFU join — use --ca-fingerprint in production")
		joinTLS = &tls.Config{InsecureSkipVerify: true} // TOFU, see above
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: a.cfg.NodeID},
	}, key)
	if err != nil {
		return err
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	conn, err := grpc.NewClient(a.cfg.KeystoneAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(joinTLS)))
	if err != nil {
		return err
	}
	defer conn.Close()
	resp, err := wsv1.NewKeystoneGatewayClient(conn).Join(ctx, &wsv1.JoinRequest{
		JoinToken: a.cfg.JoinToken,
		NodeId:    a.cfg.NodeID,
		CsrPem:    csrPEM,
	})
	if err != nil {
		return err
	}

	if pinnedCA != nil {
		if !caPEMMatchesCert(resp.GetCaPem(), pinnedCA) {
			return fmt.Errorf("join response CA does not match the pinned CA — discarding, refusing to persist identity")
		}
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(a.keyPath(), keyPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(a.certPath(), resp.GetCertPem(), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(a.caPath(), resp.GetCaPem(), 0o600); err != nil {
		return err
	}
	// Pin the op-signing public key (ADR-W003 W1). From this point on every
	// operation must verify against it — see verifyAndDispatch. A key that
	// isn't a valid ed25519 public key (protocol skew with a pre-W1
	// Keystone) is not pinned; the agent then falls back to the pre-W1
	// unsigned-accept path and logs a warning each session until it
	// re-joins against a Keystone that supplies one.
	if pub := resp.GetOpSigningPublicKey(); len(pub) == ed25519.PublicKeySize {
		if err := os.WriteFile(a.opSigningPubPath(), pub, 0o600); err != nil {
			return err
		}
	}
	a.log.Info("joined keystone, identity persisted", "data_dir", a.cfg.DataDir)
	return nil
}

// preflightPinnedCA dials addr with a plain TLS handshake (no application
// protocol, no join token sent) and verifies the CA certificate the gateway
// presents alongside its leaf against the configured fingerprint (ADR-W003
// W3). Keystone's serving certificate chain is [leaf, CA] — see
// CA.ServerTLSCert — so PeerCertificates[1] is the CA the gateway trusts
// itself with. Returns the parsed, pinned CA certificate on success. Fails
// closed: any mismatch, missing chain entry, or dial error returns before
// the caller ever sends a join token.
func preflightPinnedCA(ctx context.Context, addr, fingerprint string) (*x509.Certificate, error) {
	want, err := hex.DecodeString(fingerprint)
	if err != nil || len(want) != sha256.Size {
		return nil, fmt.Errorf("invalid CA fingerprint %q: want 64 hex chars (sha256)", fingerprint)
	}
	dialer := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("preflight dial %s: %w", addr, err)
	}
	defer conn.Close()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return nil, fmt.Errorf("preflight dial %s: not a TLS connection", addr)
	}
	peerCerts := tlsConn.ConnectionState().PeerCertificates
	if len(peerCerts) < 2 {
		return nil, fmt.Errorf("gateway did not present a CA certificate alongside its leaf; cannot pin")
	}
	caCert := peerCerts[1]
	got := sha256.Sum256(caCert.Raw)
	if subtle.ConstantTimeCompare(got[:], want) != 1 {
		return nil, fmt.Errorf("CA fingerprint mismatch: got %x, want %s — refusing to join (possible MITM)",
			got, fingerprint)
	}
	return caCert, nil
}

// caPEMMatchesCert reports whether the PEM-encoded certificate in caPEM is
// byte-identical (DER) to want. Belt-and-braces check (ADR-W003 W3) applied
// to JoinResponse.ca_pem after a pinned join RPC, on top of the preflight
// pin and the TLS-layer chain verification the join dial itself performed.
func caPEMMatchesCert(caPEM []byte, want *x509.Certificate) bool {
	block, _ := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	return subtle.ConstantTimeCompare(block.Bytes, want.Raw) == 1
}

// loadOpSigningPub reads the pinned op-signing public key from DataDir, if
// any (ADR-W003 W1). A missing/invalid file means this identity was joined
// before per-op signing existed (or against a pre-W1 Keystone); the agent
// accepts unsigned operations until it re-joins and pins a key.
func (a *Agent) loadOpSigningPub() (ed25519.PublicKey, bool) {
	b, err := os.ReadFile(a.opSigningPubPath())
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, false
	}
	return ed25519.PublicKey(b), true
}

// ---------------------------------------------------------------- session

func (a *Agent) session(ctx context.Context) error {
	cert, err := tls.LoadX509KeyPair(a.certPath(), a.keyPath())
	if err != nil {
		return err
	}
	caPEM, err := os.ReadFile(a.caPath())
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("bad pinned CA")
	}
	conn, err := grpc.NewClient(a.cfg.KeystoneAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS13,
		})))
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := wsv1.NewKeystoneGatewayClient(conn).Session(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&wsv1.AgentMessage{Msg: &wsv1.AgentMessage_Hello{Hello: &wsv1.Hello{
		NodeId:        a.cfg.NodeID,
		AgentVersion:  a.cfg.Version,
		ProtocolMinor: ProtocolMinor,
	}}}); err != nil {
		return err
	}
	a.log.Info("session established", "keystone", a.cfg.KeystoneAddr)

	pinnedPub, pinned := a.loadOpSigningPub()
	if !pinned {
		a.log.Warn("no pinned op-signing key — accepting unsigned operations this session " +
			"(pre-W1 identity; re-join against a Keystone that serves op_signing_public_key to pin one)")
	}

	var sendMu sync.Mutex
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		go func(m *wsv1.KeystoneMessage) {
			res := a.verifyAndDispatch(ctx, m.GetOpId(), m.GetOp(), m.GetOpSignature(), pinnedPub)
			sendMu.Lock()
			defer sendMu.Unlock()
			if err := stream.Send(&wsv1.AgentMessage{Msg: &wsv1.AgentMessage_Result{Result: res}}); err != nil {
				a.log.Warn("result send failed", "op_id", m.GetOpId(), "error", err)
			}
		}(msg)
	}
}
