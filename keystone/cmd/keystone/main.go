// Command keystone is the Wildstone control plane: API gateway, Reconciler,
// Capability Engine, Policy Engine, Statefile, and Butler host process.
// E2 scope: it serves both planes from one DataDir — the agent-facing
// gRPC/mTLS gateway (S1/E1) and the public HTTP API (ADR-W007) — plus the
// Statefile store, the runtime DB, and the out-of-band ingestion loop.
// The reconciler and Butler processes land in E3/E12.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/api"
	"github.com/chickenrat17/wildstone-homelab/keystone/backup"
	"github.com/chickenrat17/wildstone-homelab/keystone/gateway"
	"github.com/chickenrat17/wildstone-homelab/keystone/ingest"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile/loop"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

func main() {
	listen := flag.String("listen", ":9440", "agent gateway listen address")
	apiListen := flag.String("api-listen", "127.0.0.1:9441", "public API listen address (loopback/tailnet at MVP)")
	joinTokens := flag.String("join-tokens", "", "comma-separated one-time join tokens (dev/test; MintJoinToken is the production path)")
	hosts := flag.String("hosts", "localhost,127.0.0.1", "comma-separated SANs for the gateway certificate")
	dataDir := flag.String("data-dir", "/var/lib/wildstone/keystone", "directory persisting the CA, op-signing key, join tokens, Statefile, and runtime DB across restarts")
	ingestEvery := flag.Duration("ingest-interval", 15*time.Second, "how often to check the Statefile for out-of-band edits")
	reconcileEvery := flag.Duration("reconcile-interval", 30*time.Second, "how often the reconcile service ticks (ADR-W008)")
	managedPrefix := flag.String("managed-prefix", "/etc/wildstone", "managed-file prefix Route/OidcClient fragments render under (must be a Wardstone ManagedPrefix)")
	backupEvery := flag.Duration("backup-interval", 5*time.Minute, "how often the backup engine evaluates BackupPlan schedules (ADR-W011 A4)")
	resticImage := flag.String("restic-image", defaultResticImage, "digest-pinned restic image every backup run uses")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*listen, *apiListen, *joinTokens, *hosts, *dataDir, *ingestEvery, *reconcileEvery, *managedPrefix, *backupEvery, *resticImage, log); err != nil {
		fmt.Fprintln(os.Stderr, "keystone:", err)
		os.Exit(1)
	}
}

// defaultResticImage is the pinned restic image backup runs use, resolved
// 2026-08-25 via the OCI registry API (`wild pkg refresh-digests`'s
// resolver): restic/restic:latest.
const defaultResticImage = "restic/restic@sha256:9940e6c7421ab3a21ee9fdc4e91596d04c792b600d31b102696c75cbf3ac4481"

func run(listen, apiListen, joinTokens, hosts, dataDir string, ingestEvery, reconcileEvery time.Duration, managedPrefix string, backupEvery time.Duration, resticImage string, log *slog.Logger) error {
	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	apiLis, err := net.Listen("tcp", apiListen)
	if err != nil {
		return err
	}
	return serve(lis, apiLis, joinTokens, hosts, dataDir, ingestEvery, reconcileEvery, managedPrefix, backupEvery, resticImage, log, os.Stdout)
}

// serve wires and runs both planes over already-bound listeners —
// separated from run so the boot integration test can use :0 listeners.
// bootstrapOut receives the one-time admin token on first start.
func serve(lis, apiLis net.Listener, joinTokens, hosts, dataDir string, ingestEvery, reconcileEvery time.Duration, managedPrefix string, backupEvery time.Duration, resticImage string, log *slog.Logger, bootstrapOut io.Writer) error {
	gw, err := gateway.New(gateway.Config{
		JoinTokens: splitNonEmpty(joinTokens),
		Hosts:      splitNonEmpty(hosts),
		DataDir:    dataDir,
		Logger:     log,
	})
	if err != nil {
		return err
	}

	db, err := runtimedb.Open(filepath.Join(dataDir, "keystone.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	state, err := statefile.New(filepath.Join(dataDir, "statefile"))
	if err != nil {
		return err
	}
	principals := &principal.Store{DB: db}

	// Policy engine (ADR-W009): exemptions load from a root-owned file in
	// DataDir; a malformed file fails startup loudly rather than lurking.
	exemptions, err := policy.LoadExemptions(filepath.Join(dataDir, "policy-exemptions.yaml"))
	if err != nil {
		return err
	}
	if len(exemptions) > 0 {
		for _, ex := range exemptions {
			log.Info("policy exemption active", "rule", ex.Rule, "target", ex.Kind+"/"+ex.Name,
				"reason", ex.Reason, "approvedBy", ex.ApprovedBy)
		}
	}
	pol := policy.NewEngine(exemptions)

	// First start mints the admin principal and prints its token exactly
	// once — the ADR-W003 join-token pattern (ADR-W007 A3).
	bootstrap, err := principals.Bootstrap()
	if err != nil {
		return err
	}
	if bootstrap != "" {
		// Stdout, not the structured log: this is for the human running
		// first-start, and must not end up in shipped log files.
		fmt.Fprintf(bootstrapOut, "first start: admin principal created; API token (shown once):\n%s\n", bootstrap)
	}

	// Reconcile service (ADR-W008): converges approved plans and drift.
	// Providers (Cloudflare DnsRecord) are wired when the owner supplies
	// a token — an E6 configuration step; until then a DnsRecord in
	// desired surfaces as an incident, never a silent skip (ADR-W004 A5).
	svc := &loop.Loop{
		State: state,
		DB:    db,
		Rec: &reconcile.Reconciler{
			Applier:       &reconcile.GatewayApplier{Gateway: gw},
			ManagedPrefix: managedPrefix,
		},
		Connected: gw.Connected,
		Log:       log,
	}
	go svc.Run(context.Background(), reconcileEvery)

	// Backup engine (ADR-W011 A4): the timer duty that turns BackupPlan
	// declarations into restic container runs with provenance. Shares the
	// gateway applier — a run is an ordinary owner-labeled container.
	bak := &backup.Engine{
		State:       state,
		DB:          db,
		Run:         &backup.ApplierRunner{Applier: &reconcile.GatewayApplier{Gateway: gw}},
		ResticImage: resticImage,
		Log:         log,
	}
	go bak.RunLoop(context.Background(), backupEvery)

	// Out-of-band ingestion loop (ADR-W007 A6). An accepted or adopted
	// edit pokes the reconcile service (ADR-W008 R2a) — a hand-committed
	// change converges as promptly as an API-approved one.
	ing := &ingest.Ingestor{State: state, DB: db, Log: log, Policy: pol}
	go func() {
		for {
			res, err := ing.Tick()
			if err != nil {
				log.Error("statefile ingest", "err", err)
			} else if res.Outcome != ingest.OutcomeNoChange {
				log.Info("statefile ingest", "outcome", string(res.Outcome), "head", res.Head, "plan", res.PlanID)
				svc.Poke()
			}
			time.Sleep(ingestEvery)
		}
	}()

	// Public plane. Plan approval pokes the reconcile service (R2a).
	apiSrv := api.New(principals, db, state, log)
	apiSrv.OnApproved = svc.Poke
	apiSrv.Policy = pol
	apiSrv.Agents = &reconcile.GatewayApplier{Gateway: gw} // secrets endpoints (ADR-W012 W2)
	apiSrv.MintJoin = gw.MintJoinToken                     // pairing endpoint (ADR-W014 A3)
	apiSrv.CAFingerprint = gw.CA().Fingerprint()
	go func() {
		log.Info("public API listening", "addr", apiLis.Addr().String())
		if err := http.Serve(apiLis, apiSrv.Handler()); err != nil {
			log.Error("public API exited", "error", err)
		}
	}()

	// Agent plane (blocks).
	log.Info("gateway listening", "addr", lis.Addr().String())
	return gw.Serve(lis)
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
