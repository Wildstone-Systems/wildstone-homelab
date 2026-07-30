// Package agent implements wardstoned: the privileged host agent. It dials
// out to Keystone over mTLS gRPC (never listens), bootstraps its identity
// with a one-time join token, and serves the closed typed operation
// surface defined in proto/wardstone/v1 (ADR-W003).
package agent

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

func (a *Agent) certPath() string { return filepath.Join(a.cfg.DataDir, "node.crt") }
func (a *Agent) keyPath() string  { return filepath.Join(a.cfg.DataDir, "node.key") }
func (a *Agent) caPath() string   { return filepath.Join(a.cfg.DataDir, "keystone-ca.crt") }

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
// Trust on first use: the join dial does not verify the server (the agent
// has no CA yet) — the CA received in the response is pinned for every
// subsequent connection. E1 replaces TOFU with an installer-delivered CA
// fingerprint.
func (a *Agent) join(ctx context.Context) error {
	if a.cfg.JoinToken == "" {
		return fmt.Errorf("no identity on disk and no join token configured")
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
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true}))) // TOFU, see above
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
	a.log.Info("joined keystone, identity persisted", "data_dir", a.cfg.DataDir)
	return nil
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

	var sendMu sync.Mutex
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		go func(m *wsv1.KeystoneMessage) {
			res := a.dispatch(ctx, m.GetOpId(), m.GetOp())
			sendMu.Lock()
			defer sendMu.Unlock()
			if err := stream.Send(&wsv1.AgentMessage{Msg: &wsv1.AgentMessage_Result{Result: res}}); err != nil {
				a.log.Warn("result send failed", "op_id", m.GetOpId(), "error", err)
			}
		}(msg)
	}
}
