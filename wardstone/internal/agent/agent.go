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
//
// 1: adds RemoveContainer, ContainerStats, PruneImages (ADR-W003 W6a).
// 2: adds EnsureVolume, SnapshotDir, MeasureUsage (ADR-W003 W6b).
// 3: adds ListBlockDevices, EnsureMount, TestNfs, TestSmb, ListPorts,
//
//	ProbeTcp, ProbeHttp, EnsureDockerNetwork (ADR-W003 W6c).
//
// 4: adds SystemdUnitStatus, TimeSyncStatus (ADR-W003 W6d).
// 5: EnsureContainer.labels carries Keystone's spec hash, so a replace
//
//	compares the whole spec rather than the image alone (ADR-W008 R2c).
//
// 6: adds EnsureContainer.command (ADR-W011 A3).
// 7: adds EnsureSecret, PutSecret, ListSecrets and env materialization
//
//	of secret/<name> references (ADR-W012).
//
// 8: adds EnsureContainer.network. EnsureDockerNetwork could create a
//
//	network since minor 3, but nothing could attach a container to one,
//	so the "container/<name>:<port>" upstreams every package declares
//	had no way to resolve (#40).
//
// 9: adds WriteManagedFile.materialize_secrets and
//
//	ReadManagedFileResult.source_sha256_hex. Secret references were
//	resolved in container env only, so a config FILE an app reads could
//	not carry one — which is every OIDC client secret, since Authelia
//	reads a file. Reads of such a file return the pre-materialization
//	hash and no content: the bytes on disk hold live secrets and must not
//	travel back to Keystone.
//
// 10: adds EnsureSecret.kind (SecretKind). An OIDC provider signs ID
//
//	tokens with an RSA key it cannot generate on demand, and a random
//	blob is not a key. UNSPECIFIED means RANDOM, so a Keystone that never
//	sets it still gets what it has always asked for.
//
// 11: adds InspectContainerResult.status and .exit_code. `running`
//
//	alone cannot describe a container that is SUPPOSED to stop, so a
//	one-shot provisioning job that succeeded and one that crashed
//	immediately were indistinguishable — and a job whose success cannot
//	be observed cannot be reconciled. An agent below 11 leaves status
//	empty, which readers treat as unknown and fall back to `running`.
//
// 12: adds PortBinding.protocol, EnsureContainer.devices and
//
//	EnsureContainer.cap_add. Every published port was TCP, so a DNS
//	resolver could not answer the UDP query every client sends first;
//	no container could be handed a Zigbee stick, a GPU or /dev/net/tun;
//	and nothing could hold NET_ADMIN, so a VPN client could not run.
//	Devices and capabilities are CLOSED allowlists on this side
//	(devices.go): the agent refuses /dev/sda and SYS_ADMIN for the reason
//	it refuses Exec(string). A pre-minor-12 agent ignores all three
//	fields — a UDP port is published as TCP, a device is absent, a
//	capability is not granted — and the app reports what it is missing.
//
// 18: adds EnsureContainer.read_only_rootfs, .cap_drop,
//
//	.no_new_privileges, .resources and .shm_size (ADR-W039 Phase 6/H1).
//	Unlike minor 12's fields, these NARROW a container rather than widen
//	it, so an agent that silently ignored one would start the container
//	LESS contained than the plan asked for — the wrong direction to fail
//	quiet in. Keystone's reconciler therefore refuses to dispatch a spec
//	using any of them to an agent below this minor
//	(resource.HardeningProtocolMinor), rather than letting an older
//	agent apply it partially.
//
// 19: adds DiskSpace and MeasureUsage.volume (ADR-W045 A3.1/A3.2), so a
//
//	backup can refuse to fill the disk its local copy lives on and can
//	tell a mounted NAS share from the empty directory under it. An older
//	agent answers DiskSpace as an unknown operation; Keystone treats that
//	as "cannot check" and, for a NAS path, refuses to run.
//
// 20: adds recovery-key-sealed node secret export/import. Export never
// returns plaintext; import is restricted to an empty fresh-node store.
//
// 21: adds IPC_LOCK to the closed capability allowlist (#441). An older
// agent refuses it as FORBIDDEN, so Keystone gates a spec adding it on
// this minor (resource.IPCLockProtocolMinor).
//
// 22: adds EnsureContainer.network_mode (CONTAINER_NETWORK_MODE_HOST) and
//
//	InspectContainerResult.network_mode. A home-automation hub has to
//	share the node's network namespace to hear mDNS, SSDP and the other
//	link-local discovery a bridge never forwards. Host mode excludes
//	ports and network; both are refused before an existing container is
//	touched. A pre-minor-22 agent would ignore the field and start the
//	container on the bridge with nothing published, so Keystone refuses
//	to dispatch a host-mode spec to one (resource.HostNetworkProtocolMinor).
//
// 23: EnsureContainer.host_aliases gives one closed name,
//
//	"host.docker.internal", Docker's "host-gateway" value instead of a
//	node DNS answer (#433), so a bridge-networked proxy can reach a
//	"host/<port>" Route with no machine-specific address in desired
//	state. No schema change. An older agent tries to resolve the name,
//	fails and skips it, leaving the proxy with no route to the host, so
//	Keystone withholds the alias from and refuses host Routes on one
//	(resource.HostGatewayProtocolMinor).
const ProtocolMinor = 23

// maxUpdateMsgBytes is the gRPC message size cap on both ends of the
// session/join connections (ADR-W003 W5). SelfUpdate ships the new binary
// inline; 64 MiB gives Wardstone's ~10-20 MiB static binaries headroom
// without needing chunked delivery yet (see the .proto comment on
// SelfUpdate, and keystone/gateway's matching server-side option).
const maxUpdateMsgBytes = 64 << 20

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

	// MountRoot is the second hard boundary EnsureMount enforces on top of
	// ManagedPrefixes (ADR-W003 W6c): the target must resolve under BOTH a
	// managed prefix and MountRoot. Prod default /srv/wildstone; tests set
	// it to a temp dir that is also listed in ManagedPrefixes.
	MountRoot string
	// FstabPath is where EnsureMount reads/writes its marker-delimited
	// block. Prod default /etc/fstab; tests use a temp file.
	FstabPath string

	// SystemdUnitAllowlist bounds SystemdUnitStatus (ADR-W003 W6d): the
	// requested unit must exactly match an entry here or the agent refuses
	// with FORBIDDEN, regardless of what Keystone asks — defense in depth
	// so even a compromised Keystone can only observe these units. Default:
	// wardstoned.service, docker.service.
	SystemdUnitAllowlist []string

	// Reconnect backoff (full jitter). Defaults: 500ms base, 15s cap.
	BackoffBase time.Duration
	BackoffCap  time.Duration

	// RenewCheckInterval is how often a live session re-checks certificate
	// lifetime (W4). Default 24h; tests shorten it.
	RenewCheckInterval time.Duration

	// ExecSelf, if set, replaces the real re-exec performed after a
	// promoted self-update (ADR-W003 W5) and after CheckUpdateState reverts
	// a promotion that never confirmed. Production code leaves this nil,
	// which uses syscall.Exec(BinaryPath, os.Args, os.Environ()) on linux
	// (a no-op log on other platforms). Tests set it to observe/avoid the
	// real re-exec.
	ExecSelf func()

	Logger *slog.Logger
}

type Agent struct {
	cfg    Config
	log    *slog.Logger
	docker *dockerClient

	// execSelf performs (or, in tests, stands in for) the re-exec that
	// takes effect after a promoted self-update — see selfUpdate and the
	// session send-path comment below for the ordering guarantee.
	execSelf func()
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
	if cfg.MountRoot == "" {
		cfg.MountRoot = "/srv/wildstone"
	}
	cfg.MountRoot = filepath.Clean(cfg.MountRoot)
	if cfg.FstabPath == "" {
		cfg.FstabPath = "/etc/fstab"
	}
	if len(cfg.SystemdUnitAllowlist) == 0 {
		cfg.SystemdUnitAllowlist = []string{"wardstoned.service", "docker.service"}
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
	if cfg.RenewCheckInterval == 0 {
		cfg.RenewCheckInterval = defaultRenewCheckInterval
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
	a := &Agent{cfg: cfg, log: cfg.Logger.With("node", cfg.NodeID), docker: docker}
	if cfg.ExecSelf != nil {
		a.execSelf = cfg.ExecSelf
	} else {
		a.execSelf = a.defaultExecSelf
	}
	return a, nil
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
		grpc.WithTransportCredentials(credentials.NewTLS(joinTLS)),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxUpdateMsgBytes),
			grpc.MaxCallSendMsgSize(maxUpdateMsgBytes),
		))
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
		})),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxUpdateMsgBytes),
			grpc.MaxCallSendMsgSize(maxUpdateMsgBytes),
		))
	if err != nil {
		return err
	}
	defer conn.Close()

	// sessionCtx (not the outer ctx) drives the Session stream, so that
	// renewal (ADR-W003 W4) can force a reconnect — by cancelling
	// sessionCtx — without tearing down Run's outer loop or racing the
	// caller's cancellation. cancelSession must run before we wait for the
	// renewal goroutine to notice and exit, hence the defer ordering below
	// (defers run LIFO: the later `defer cancelSession()` fires first).
	sessionCtx, cancelSession := context.WithCancel(ctx)
	stream, err := wsv1.NewKeystoneGatewayClient(conn).Session(sessionCtx)
	if err != nil {
		cancelSession()
		return err
	}
	if err := stream.Send(&wsv1.AgentMessage{Msg: &wsv1.AgentMessage_Hello{Hello: &wsv1.Hello{
		NodeId:        a.cfg.NodeID,
		AgentVersion:  a.cfg.Version,
		ProtocolMinor: ProtocolMinor,
	}}}); err != nil {
		cancelSession()
		return err
	}
	a.log.Info("session established", "keystone", a.cfg.KeystoneAddr)

	// Self-update health gate (ADR-W003 W5): the first successful session
	// establishment after a promotion is proof the promoted binary works
	// well enough to talk to Keystone. Clear the pending state so a later
	// startup (e.g. after a routine restart) does not mistake this healthy
	// binary for one that never confirmed and revert it.
	a.confirmUpdateIfPending()

	pinnedPub, pinned := a.loadOpSigningPub()
	if !pinned {
		a.log.Warn("no pinned op-signing key — accepting unsigned operations this session " +
			"(pre-W1 identity; re-join against a Keystone that serves op_signing_public_key to pin one)")
	}

	// Per-session replay guard (ADR-W003 W1): rejects a validly-signed op
	// whose op_id was already processed on this connection. Scoped to this
	// session — a fresh guard each time session() runs — so it is discarded
	// on reconnect and never falsely rejects op_ids from a new gateway
	// generation. See replayGuard for the memory-bounding rationale.
	guard := newReplayGuard()

	// Certificate rotation (ADR-W003 W4): check on establish and then every
	// 24h for the remainder of the session's life; force a reconnect via
	// cancelSession after a successful renewal so the next dial in Run's
	// loop picks up the new cert. Runs on conn, the same connection the
	// session stream uses — gRPC connections are safe for concurrent RPCs,
	// so this does not race the Recv/dispatch loop below.
	renewalDone := make(chan struct{})
	go func() {
		defer close(renewalDone)
		a.renewalLoop(sessionCtx, conn, cancelSession)
	}()
	defer func() { <-renewalDone }()
	defer cancelSession()

	var sendMu sync.Mutex
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		go func(m *wsv1.KeystoneMessage) {
			res := a.verifyAndDispatch(ctx, m.GetOpId(), m.GetOp(), m.GetOpSignature(), pinnedPub, guard)
			sendMu.Lock()
			sendErr := stream.Send(&wsv1.AgentMessage{Msg: &wsv1.AgentMessage_Result{Result: res}})
			sendMu.Unlock()
			if sendErr != nil {
				a.log.Warn("result send failed", "op_id", m.GetOpId(), "error", sendErr)
				return
			}
			// Re-exec ordering guarantee (ADR-W003 W5): selfUpdate must not
			// re-exec itself before Keystone has the OpResult — Keystone's
			// Dispatch is waiting on it, and re-exec replaces this process
			// image immediately with no chance to flush anything after. So
			// selfUpdate only stages+promotes+writes the state file; it
			// never calls execSelf directly. Instead, THIS goroutine — the
			// one handling the SelfUpdate op — checks its own just-sent
			// result after the send succeeds (outside sendMu, so it never
			// blocks other in-flight result sends) and re-execs only then.
			// Checking res (not a shared flag) ties the re-exec to exactly
			// the op that requested it, so a concurrent unrelated op never
			// triggers a spurious re-exec.
			if su := res.GetSelfUpdate(); su.GetPromoted() {
				a.execSelf()
			}
		}(msg)
	}
}

// ---------------------------------------------------------------- rotation

// defaultRenewCheckInterval is how often a live session re-checks its certificate's
// remaining lifetime (ADR-W003 W4), beyond the check done on session
// establishment.
const defaultRenewCheckInterval = 24 * time.Hour

// renewalLoop checks the on-disk client certificate's remaining lifetime
// immediately, then on renewCheckInterval, until ctx is cancelled (i.e. the
// session ends). A renewal failure is logged at Warn and retried on the
// next tick or the next session establishment — it never tears down the
// live session.
func (a *Agent) renewalLoop(ctx context.Context, conn *grpc.ClientConn, forceReconnect context.CancelFunc) {
	a.checkAndRenew(ctx, conn, forceReconnect)
	ticker := time.NewTicker(a.cfg.RenewCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.checkAndRenew(ctx, conn, forceReconnect)
		}
	}
}

// checkAndRenew loads the current client certificate and, if less than 1/3
// of its total lifetime remains, renews it and forces a reconnect so the
// new cert takes effect. Errors (loading the cert, the Renew RPC, or
// persisting the result) are logged at Warn and otherwise swallowed — the
// live session must not be affected by a renewal failure.
func (a *Agent) checkAndRenew(ctx context.Context, conn *grpc.ClientConn, forceReconnect context.CancelFunc) {
	leaf, err := a.loadLeafCert()
	if err != nil {
		a.log.Warn("renewal: could not load current certificate", "error", err)
		return
	}
	total := leaf.NotAfter.Sub(leaf.NotBefore)
	remaining := time.Until(leaf.NotAfter)
	if total <= 0 || remaining >= total/3 {
		return
	}
	a.log.Info("certificate nearing expiry, renewing", "remaining", remaining, "total", total)
	if err := a.renew(ctx, conn); err != nil {
		a.log.Warn("certificate renewal failed, will retry", "error", err)
		return
	}
	a.log.Info("certificate renewed, forcing reconnect to pick it up")
	forceReconnect()
}

// renew generates a new key + CSR, exchanges it for a new certificate over
// the Renew RPC on the given (already-authenticated) connection, and
// persists the new key+cert atomically — tmp+rename, key first then cert,
// matching the writeManagedFile pattern in ops.go. The node identity is not
// sent by the agent at all beyond the CSR's (ignored) subject: the gateway
// derives it solely from this connection's mTLS client-cert CN.
func (a *Agent) renew(ctx context.Context, conn *grpc.ClientConn) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: a.cfg.NodeID}, // ignored server-side; CN comes from the peer cert
	}, key)
	if err != nil {
		return err
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	resp, err := wsv1.NewKeystoneGatewayClient(conn).Renew(ctx, &wsv1.RenewRequest{CsrPem: csrPEM})
	if err != nil {
		return fmt.Errorf("Renew RPC: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := writeFileAtomic(a.keyPath(), keyPEM, 0o600); err != nil {
		return fmt.Errorf("persist renewed key: %w", err)
	}
	if err := writeFileAtomic(a.certPath(), resp.GetCertPem(), 0o600); err != nil {
		return fmt.Errorf("persist renewed cert: %w", err)
	}
	return nil
}

// loadLeafCert parses the currently-persisted client certificate (node.crt)
// so its NotBefore/NotAfter can be inspected for rotation.
func (a *Agent) loadLeafCert() (*x509.Certificate, error) {
	b, err := os.ReadFile(a.certPath())
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("node.crt: not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// writeFileAtomic writes data to path via tmp+rename, matching the
// writeManagedFile pattern in ops.go, so a crash mid-write never leaves a
// torn identity file on disk.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".wildstone-tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
