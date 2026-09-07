// Command keystone is the Wildstone control plane: API gateway, Reconciler,
// Capability Engine, Policy Engine, Statefile, and Butler host process.
// E2 scope: it serves both planes from one DataDir — the agent-facing
// gRPC/mTLS gateway (S1/E1) and the public HTTP API (ADR-W007) — plus the
// Statefile store, the runtime DB, and the out-of-band ingestion loop.
// The reconciler and Butler processes land in E3/E12.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/api"
	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/backup"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/gateway"
	"github.com/chickenrat17/wildstone-homelab/keystone/ingest"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile/loop"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/report"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

var version = "dev" // set via -ldflags at release

func main() {
	// Subcommands are matched before flag parsing, because they take
	// their own flags and must work with the service STOPPED.
	if len(os.Args) > 1 && os.Args[1] == "claim-code" {
		if err := claimCode(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "keystone claim-code:", err)
			os.Exit(1)
		}
		return
	}

	showVersion := flag.Bool("version", false, "print version and exit")
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
	catalogDir := flag.String("catalog-dir", "", "directory of packages this control plane can install (ADR-W021 A3); empty leaves the package/capability endpoints answering 503")
	consoleDir := flag.String("console-dir", "", "directory of built Console static files to serve beside /api (ADR-W016 A5); empty serves the API only")
	butlerProvider := flag.String("butler-provider", "off", "Butler LLM provider: off|anthropic|openai|ollama (ADR-W017 A4); API keys come ONLY from ANTHROPIC_API_KEY/OPENAI_API_KEY env vars, never a flag")
	butlerModel := flag.String("butler-model", "", "Butler model name; empty uses the provider's default")
	butlerEndpoint := flag.String("butler-endpoint", "", "Butler provider base URL (openai-compatible gateways, ollama host); empty uses the provider's default")
	automationEvery := flag.Duration("automation-interval", 15*time.Second, "how often the household automation engine ticks between event pokes (ADR-W024 H3)")
	householdTZ := flag.String("household-timezone", "Local", "IANA time zone the household's rules keep (timeOfDay/weekday conditions), e.g. Europe/London")
	ntfyURL := flag.String("ntfy-url", "", "ntfy base URL the notifications.notify household action publishes to (ADR-W024 H4); empty leaves that action unregistered. The access token comes ONLY from NTFY_TOKEN, never a flag")
	replyURL := flag.String("household-reply-url", "", "household event intake URL a notification's reply buttons post to, as a PHONE can reach it (not Keystone's loopback address); empty means notifications carry no reply buttons and a rule that asks for them fails loudly")
	replyTTL := flag.Duration("household-reply-ttl", 12*time.Hour, "how long a notification's reply token stays valid")
	flag.Parse()

	if *showVersion {
		fmt.Println("keystone", version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	butlerCfg := butlerConfig{Provider: *butlerProvider, Model: *butlerModel, Endpoint: *butlerEndpoint}
	householdCfg := householdConfig{
		Interval: *automationEvery, Timezone: *householdTZ,
		NtfyURL: *ntfyURL, ReplyURL: *replyURL, ReplyTTL: *replyTTL,
	}
	if err := run(*listen, *apiListen, *joinTokens, *hosts, *dataDir, *ingestEvery, *reconcileEvery, *managedPrefix, *backupEvery, *resticImage, *consoleDir, *catalogDir, butlerCfg, householdCfg, log); err != nil {
		fmt.Fprintln(os.Stderr, "keystone:", err)
		os.Exit(1)
	}
}

// defaultResticImage is the pinned restic image backup runs use, resolved
// 2026-08-25 via the OCI registry API (`wild pkg refresh-digests`'s
// resolver): restic/restic:latest.
const defaultResticImage = "restic/restic@sha256:9940e6c7421ab3a21ee9fdc4e91596d04c792b600d31b102696c75cbf3ac4481"

// claimCode mints an admin token against a data directory directly,
// with Keystone stopped (ADR-W020 A2, closing F44).
//
// Why this is not an API call: an API call needs a token, and the state
// this exists to repair is "there is no usable token". Every other
// recovery path documented for that state ends in destroying the runtime
// DB — which discards every principal, every plan and the CA, to recover
// one credential.
//
// The authority check is possession of the data directory. That is root
// on the host, which is the same proof of physical access the join-token
// pattern already rests on (ADR-W003 A1), so this adds no new trust
// assumption; it makes an existing one recoverable.
//
// The token is written to a file rather than printed, and this is the
// point of the change rather than a detail. Printing is what put the
// only copy in the journal, where scrubbing loses it and where a journal
// surviving a wipe hands back tokens from three different installs with
// nothing to distinguish them (F66). A file has one value, is
// root-readable only, and can be deleted once used.
func claimCode(args []string) error {
	fs := flag.NewFlagSet("claim-code", flag.ExitOnError)
	dataDir := fs.String("data-dir", "/var/lib/wildstone/keystone", "Keystone data directory (the service should be stopped)")
	name := fs.String("name", "admin", "principal to mint for; created as an admin if absent")
	out := fs.String("out", "", "file to write the token to (default <data-dir>/claim-code)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dest := *out
	if dest == "" {
		dest = filepath.Join(*dataDir, "claim-code")
	}

	db, err := runtimedb.Open(filepath.Join(*dataDir, "keystone.db"))
	if err != nil {
		return fmt.Errorf("opening the runtime DB (is keystone still running?): %w", err)
	}
	defer db.Close()

	token, err := (&principal.Store{DB: db}).EnsureAdmin(*name)
	if err != nil {
		return err
	}
	// 0600 before any content is written: the file must never exist,
	// even briefly, with a readable mode.
	if err := os.WriteFile(dest, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	fmt.Printf("admin token for %q written to %s (mode 0600)\n", *name, dest)
	fmt.Println("read it once, use it, then delete the file.")
	return nil
}

func run(listen, apiListen, joinTokens, hosts, dataDir string, ingestEvery, reconcileEvery time.Duration, managedPrefix string, backupEvery time.Duration, resticImage, consoleDir, catalogDir string, butlerCfg butlerConfig, householdCfg householdConfig, log *slog.Logger) error {
	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	apiLis, err := net.Listen("tcp", apiListen)
	if err != nil {
		return err
	}
	return serve(lis, apiLis, joinTokens, hosts, dataDir, ingestEvery, reconcileEvery, managedPrefix, backupEvery, resticImage, consoleDir, catalogDir, butlerCfg, householdCfg, log, os.Stdout)
}

// householdConfig carries the household automation flags into serve
// (ADR-W024 H3).
type householdConfig struct {
	// Interval between engine ticks when no event pokes it.
	Interval time.Duration
	// Timezone is the IANA name the household's rules keep; "" or
	// "Local" is the host's.
	Timezone string
	// NtfyURL is where the notifications.notify action publishes; empty
	// leaves that executor unregistered.
	NtfyURL string
	// ReplyURL is the intake address a notification's reply buttons
	// post to, as a household device can reach it.
	ReplyURL string
	// ReplyTTL bounds a reply token's life.
	ReplyTTL time.Duration
}

// butlerSessionPrincipal is the acting principal on every Butler session
// record (ADR-W001 A1). The small-duplicated-constant pattern: the API
// owns the same string for the sessions it creates, and this file needs
// the value rather than that package.
const butlerSessionPrincipal = "butler"

// replyPrincipal publishes a household member's answer to a
// notification (ADR-W024 H4). It holds the publisher role — one verb,
// emit — because its token travels to a phone inside a notification.
// Whoever holds one can claim the household answered something, and can
// do nothing else: not read state, not propose, not approve.
const replyPrincipal = "publisher-replies"

// replyMinter returns the seam the notifier uses to mint a short-lived
// reply token per notification. The principal is created once here
// rather than lazily per call, so two notifications racing cannot both
// try to create it.
func replyMinter(principals *principal.Store, db *runtimedb.DB, ttl time.Duration) (func(context.Context) (string, error), error) {
	if _, err := db.GetPrincipal(replyPrincipal); errors.Is(err, runtimedb.ErrNotFound) {
		if cerr := principals.Create(replyPrincipal, runtimedb.RolePublisher, ""); cerr != nil {
			return nil, fmt.Errorf("creating the %s principal: %w", replyPrincipal, cerr)
		}
	} else if err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return func(context.Context) (string, error) {
		return principals.Mint(replyPrincipal, "", ttl)
	}, nil
}

// agentToolSources flattens a catalog's agentTools declarations for the
// domestic gateway's declaration check (ADR-W024 A5).
func agentToolSources(cat *catalog.Catalog) []domestic.AgentToolSource {
	if cat == nil {
		return nil
	}
	var out []domestic.AgentToolSource
	for _, p := range cat.List() {
		if len(p.AgentTools) == 0 {
			continue
		}
		tools := map[string]string{}
		for _, t := range p.AgentTools {
			tools[t.Name] = t.Risk
		}
		out = append(out, domestic.AgentToolSource{Capability: p.Metadata.Capability, Tools: tools})
	}
	return out
}

// butlerConfig carries the -butler-* flags into serve (ADR-W017 A4).
type butlerConfig struct {
	Provider string // off | anthropic | openai | ollama
	Model    string
	Endpoint string
}

// butlerBackend is what the flags RESOLVED to: the model and base URL the
// provider will actually talk to, including each client's own default
// when the flag was left empty. It carries no credential — it exists so
// the Console's Butler surface (GET /api/v1/butler) can show an operator
// where their Butler points without them reading the command line.
type butlerBackend struct {
	Model    string
	Endpoint string
}

// butlerProviderRef is a provider.Provider that resolves to whatever the
// Butler is configured with RIGHT NOW.
//
// Household escalation (ADR-W024 A6) needs a provider at wiring time,
// long before an operator may change their mind. Binding it to the
// process's startup provider meant switching from a hosted model to a
// local one — or to off — kept sending escalation facts to the old
// hosted provider: contrary to the saved configuration, and contrary to
// the reason someone chooses local.
//
// Chat on an unconfigured Butler returns an error rather than panicking,
// and the caller already falls back to the rule's own wording.
type butlerProviderRef struct{ host *api.ButlerHost }

func (r butlerProviderRef) Name() string {
	p, _, _, _, _ := r.host.Current()
	if p == nil {
		return "unconfigured"
	}
	return p.Name()
}

func (r butlerProviderRef) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	p, _, _, _, _ := r.host.Current()
	if p == nil {
		return nil, fmt.Errorf("butler: no provider is configured")
	}
	return p.Chat(ctx, req)
}

// probeButler runs the S4 tool-reliability probe and turns it into a
// mode plus a sentence an operator can read (ADR-W017 A4/A9).
//
// Shared by startup and by a runtime reconfiguration, so a model
// selected in the Console is held to exactly the check a flagged one is.
func probeButler(ctx context.Context, p provider.Provider, log *slog.Logger) (runtimedb.ButlerSessionMode, string) {
	probeCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	verdict, err := provider.Probe(probeCtx, p)
	if err != nil || verdict != provider.Full {
		detail := verdict.String()
		if err != nil {
			// The common case on a local backend is a model that has not
			// been loaded yet, and "context deadline exceeded" is worth
			// showing verbatim rather than flattening to "failed" — it
			// tells the operator to try again once it is warm.
			detail = err.Error()
		}
		log.Warn("butler probe did not pass; sessions run QA-only (read tools, no propose_change)",
			"provider", p.Name(), "verdict", verdict.String(), "err", err)
		return runtimedb.ButlerModeQAOnly, detail
	}
	return runtimedb.ButlerModeFull, "probe passed: full mode"
}

// newButlerProvider builds the configured Butler provider, or a nil
// provider when the Butler is off. API keys come ONLY from the provider's
// environment variable (ADR-W017 A4) — there is deliberately no key flag.
func newButlerProvider(cfg butlerConfig) (provider.Provider, butlerBackend, error) {
	switch cfg.Provider {
	case "", "off":
		return nil, butlerBackend{}, nil
	case "anthropic":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			return nil, butlerBackend{}, fmt.Errorf("-butler-provider anthropic requires ANTHROPIC_API_KEY in the environment")
		}
		p := provider.NewAnthropicProvider(key)
		if cfg.Model != "" {
			p.Model = cfg.Model
		}
		if cfg.Endpoint != "" {
			p.BaseURL = cfg.Endpoint
		}
		return p, butlerBackend{Model: p.Model, Endpoint: p.BaseURL}, nil
	case "openai":
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" {
			return nil, butlerBackend{}, fmt.Errorf("-butler-provider openai requires OPENAI_API_KEY in the environment")
		}
		base := cfg.Endpoint
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		p := provider.NewOpenAIProvider(base, key)
		if cfg.Model != "" {
			p.Model = cfg.Model
		}
		return p, butlerBackend{Model: p.Model, Endpoint: p.BaseURL}, nil
	case "ollama":
		host := cfg.Endpoint
		if host == "" {
			host = "http://localhost:11434"
		}
		p := provider.NewOllamaProvider(host)
		if cfg.Model != "" {
			p.Model = cfg.Model
		}
		return p, butlerBackend{Model: p.Model, Endpoint: p.Host}, nil
	default:
		return nil, butlerBackend{}, fmt.Errorf("unknown -butler-provider %q (want off|anthropic|openai|ollama)", cfg.Provider)
	}
}

// withConsole mounts the built Console's static files beside the API
// (ADR-W016 A5): /api/* stays the API handler untouched; everything else
// serves a file from dir, falling back to index.html for client-side
// routes (the SPA convention). Path traversal is handled by
// http.ServeFile/FileServer's own cleaning.
func withConsole(apiHandler http.Handler, dir string) http.Handler {
	fileServer := http.FileServer(http.Dir(dir))
	mux := http.NewServeMux()
	mux.Handle("/api/", apiHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(path.Clean(r.URL.Path), "/")))); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})
	return mux
}

// serve wires and runs both planes over already-bound listeners —
// separated from run so the boot integration test can use :0 listeners.
// bootstrapOut receives the one-time admin token on first start.
func serve(lis, apiLis net.Listener, joinTokens, hosts, dataDir string, ingestEvery, reconcileEvery time.Duration, managedPrefix string, backupEvery time.Duration, resticImage, consoleDir, catalogDir string, butlerCfg butlerConfig, householdCfg householdConfig, log *slog.Logger, bootstrapOut io.Writer) error {
	// recordNode is bound once the runtime DB is open, a few lines below.
	// The indirection is safe without synchronisation: the gateway cannot
	// deliver a node event before gw.Serve, which is the last statement in
	// this function, long after the assignment.
	var recordNode func(gateway.NodeEvent)
	gw, err := gateway.New(gateway.Config{
		JoinTokens: splitNonEmpty(joinTokens),
		Hosts:      splitNonEmpty(hosts),
		DataDir:    dataDir,
		Logger:     log,
		OnNodeEvent: func(e gateway.NodeEvent) {
			if recordNode != nil {
				recordNode(e)
			}
		},
	})
	if err != nil {
		return err
	}

	db, err := runtimedb.Open(filepath.Join(dataDir, "keystone.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	// Node inventory (#30): the gateway keeps no database dependency, so
	// it reports node lifecycle through this hook and Keystone persists
	// it. A failure here must never break a join or drop a session — the
	// inventory is bookkeeping, not part of the trust path.
	recordNode = func(e gateway.NodeEvent) {
		var err error
		switch e.Kind {
		case gateway.NodeJoined:
			err = db.RecordNodeJoin(e.NodeID)
		default:
			err = db.RecordNodeSeen(e.NodeID, e.AgentVersion, e.ProtocolMinor)
		}
		if err != nil {
			log.Warn("recording node event", "node", e.NodeID, "kind", e.Kind, "err", err)
		}
	}

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
	// Restore what the reconciler manages before the first tick (F76).
	//
	// Secret, Volume and BackupPlan cannot be observed on a node — no op
	// answers "does this declared secret exist" — so the reconciler
	// remembers, and an unremembered key reads as absent. That memory was
	// in-process, so every restart emptied it and the next tick planned
	// creates for every one of them on a healthy node.
	//
	// The observed rows are the previous answer, written by this same
	// loop before it stopped.
	//
	// Only the HEALTHY rows. An unhealthy row is the previous loop saying
	// it did not get this resource into its desired state — a create that
	// failed leaves `absent: create planned, not applied` — and seeding
	// that would restore it as present: for a kind that cannot be
	// observed, the missing resource would then compare equal to desired,
	// no create would ever be planned again, and the next status write
	// would even call it healthy. A resource that does not exist would be
	// green forever. Re-planning the create instead is both correct and
	// safe, because these applies are idempotent: EnsureSecret returns an
	// existing secret untouched unless rotation is asked for, and
	// EnsureVolume is a no-op on a volume that is already there.
	if rows, oerr := db.ListObserved(); oerr != nil {
		log.Error("seeding managed state; the first tick may re-plan resources that already exist", "err", oerr)
	} else {
		byNode := seedRefs(rows)
		seeded := 0
		for node, refs := range byNode {
			svc.Rec.SeedManaged(node, refs)
			seeded += len(refs)
		}
		log.Info("managed state restored", "nodes", len(byNode), "resources", seeded, "rows", len(rows))
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

	// The catalog is what this control plane can install. A directory
	// that cannot be read is fatal rather than skipped: an operator who
	// passed -catalog-dir meant it, and silently serving an empty
	// catalog would make "the package is not there" the symptom of a
	// typo in a path.
	//
	// Loaded before the household engine because the domestic gateway
	// checks every household action against the agentTools its owning
	// package declares (ADR-W024 A5).
	var cat *catalog.Catalog
	if catalogDir != "" {
		loaded, cerr := catalog.LoadDir(catalogDir)
		if cerr != nil {
			return cerr
		}
		cat = loaded
		log.Info("catalog loaded", "dir", catalogDir, "packages", len(cat.List()))
	}

	// Domestic gateway (ADR-W024 A5, H4): the only way a household rule
	// acts. Executors are registered here and nowhere else; a rule that
	// names a tool this deployment has not wired is refused with a
	// reason on the household stream rather than failing silently.
	actions := &domestic.Gateway{Log: log}
	if cat != nil {
		// Only with a catalog: DeclaredFromPackages over an empty set
		// would refuse every action, which is not what "no catalog" means.
		actions.Declared = domestic.DeclaredFromPackages(agentToolSources(cat))
	}
	actions.Audit = func(c domestic.Call, r domestic.Result, aerr error) {
		ev := runtimedb.Event{
			Type: "domestic.acted", Kind: "Automation", Name: c.Rule, Message: r.Summary,
			Attrs: map[string]string{"tool": c.Tool, "tier": c.Tier, "subject": c.Subject},
		}
		if aerr != nil {
			var refusal *domestic.RefusalError
			ev.Type = "domestic.failed"
			if errors.As(aerr, &refusal) {
				ev.Type = "domestic.refused"
			}
			ev.Message = aerr.Error()
		}
		if _, eerr := db.AppendEvent(ev); eerr != nil {
			log.Error("domestic audit append failed", "tool", c.Tool, "err", eerr)
		}
	}
	// Chat as a household delivery target (ADR-W026 A8, G6). One
	// executor per installed chat capability that declares `send`, so a
	// rule names the capability it means rather than a transport the
	// deployment might run twice. Registration reads the catalog: a
	// capability whose package does not declare the tool is not wired,
	// and the gateway's declaration check refuses it either way.
	//
	// Family surfaces only. The admin packages declare no agentTools at
	// all, because a household rule notifies the HOUSEHOLD — pointing one
	// at the operator's private bot would put household events into the
	// one conversation that can also propose changes.
	var chatCapabilities []string
	if cat != nil {
		for _, p := range cat.List() {
			capName := p.Metadata.Capability
			if !strings.HasPrefix(capName, "chat-") || !strings.HasSuffix(capName, "-family") {
				continue
			}
			declaresSend := false
			for _, t := range p.AgentTools {
				if t.Name == "send" {
					declaresSend = true
				}
			}
			if declaresSend {
				chatCapabilities = append(chatCapabilities, capName)
			}
		}
	}

	if householdCfg.NtfyURL != "" {
		mintReply, merr := replyMinter(principals, db, householdCfg.ReplyTTL)
		if merr != nil {
			return merr
		}
		actions.Register(&domestic.Notifier{
			BaseURL:   householdCfg.NtfyURL,
			Token:     os.Getenv("NTFY_TOKEN"),
			ReplyURL:  householdCfg.ReplyURL,
			MintReply: mintReply,
		})
		log.Info("household actions wired", "tools", actions.Tools(), "replies", householdCfg.ReplyURL != "")
	}

	// The chat executors are registered after the API server exists,
	// since their two seams are its methods — see below, where apiSrv is
	// built. Their names are settled here so the log line above is not
	// the only place they appear.

	// Household automation engine (ADR-W024 H3): follows the household
	// stream, records every decision, and hands a fired rule's action to
	// the gateway above.
	householdLoc, err := time.LoadLocation(householdCfg.Timezone)
	if err != nil {
		return fmt.Errorf("-household-timezone %q: %w", householdCfg.Timezone, err)
	}
	automationEvery := householdCfg.Interval
	if automationEvery <= 0 {
		automationEvery = 15 * time.Second
	}
	auto := &automation.Engine{
		DB: db, State: state, Log: log, Location: householdLoc, Actions: actions,
	}
	// Run starts below, once the Butler wiring has had its chance to
	// install the escalator: the engine's fields are read by its loop,
	// so everything it needs is set before that loop exists.

	// Public plane. Plan approval pokes the reconcile service (R2a); a
	// new household event pokes the automation engine.
	apiSrv := api.New(principals, db, state, log)
	apiSrv.Automation = auto
	apiSrv.OnHouseholdEvent = auto.Poke
	apiSrv.Catalog = cat
	apiSrv.OnApproved = svc.Poke
	apiSrv.Policy = pol
	apiSrv.Agents = &reconcile.GatewayApplier{Gateway: gw} // secrets endpoints (ADR-W012 W2)
	apiSrv.MintJoin = gw.MintJoinToken                     // pairing endpoint (ADR-W014 A3)
	apiSrv.LiveNodes = func() []api.LiveNode {             // node inventory liveness (#30)
		live := gw.LiveNodes()
		out := make([]api.LiveNode, 0, len(live))
		for _, n := range live {
			out = append(out, api.LiveNode{
				NodeID: n.NodeID, AgentVersion: n.AgentVersion,
				ProtocolMinor: n.ProtocolMinor, Since: n.Since,
			})
		}
		return out
	}
	// Wire the chat delivery executors now that the API server exists:
	// both seams are its methods, so keystone/domestic learns neither
	// the runtime DB nor a chat vendor (ADR-W026 G6).
	for _, capName := range chatCapabilities {
		transport := chatTransportFor(capName)
		capName := capName
		actions.Register(&domestic.ChatSender{
			Capability: capName,
			Transport:  transport,
			// Registered from the CATALOG, gated on the STATEFILE. The
			// catalog says a package exists; only an install says this
			// deployment has it. Checking per call rather than filtering
			// at registration means enabling a chat capability works
			// without a restart, and disabling one stops it at once
			// (ADR-W026 A8).
			Enabled: func() (bool, error) { return apiSrv.CapabilityEnabled(capName) },
			Lookup: func(_ context.Context, member string) (domestic.ChatRecipient, error) {
				target, bindingID, lerr := apiSrv.ChatRecipientFor(transport, member)
				if lerr != nil {
					return domestic.ChatRecipient{}, lerr
				}
				return domestic.ChatRecipient{Transport: transport, Target: target, BindingID: bindingID}, nil
			},
			Ask: func(ctx context.Context, r domestic.ChatRecipient, body, correlationID, rule, subject string) (int64, error) {
				return apiSrv.AskOverChat(ctx, r.Transport, r.Target, r.BindingID, body, correlationID, rule, subject)
			},
		})
	}
	if len(chatCapabilities) > 0 {
		log.Info("chat delivery wired", "capabilities", chatCapabilities, "tools", actions.Tools())
	}

	apiSrv.CAFingerprint = gw.CA().Fingerprint()
	apiSrv.GatewayAddr = advertisedGatewayAddr(hosts, lis.Addr().String())
	apiSrv.APIURL = advertisedAPIURL(hosts, apiLis.Addr().String())
	apiSrv.BackupRun = bak.RunNow // on-demand backup endpoints (ADR-W015 A3)
	apiSrv.BackupRestore = bak.RestoreAlongside

	// Butler host (ADR-W017 A4): build the configured provider, run the
	// tool-reliability probe once at startup, and wire the ask endpoint in
	// the mode the verdict earns. Provider off leaves ButlerHost nil — the
	// ask endpoint answers 503 with a clear reason.
	// Stored config wins over the flags (ADR-W017 A9). The flags remain
	// the bootstrap — a fresh install with -butler-provider still works
	// and needs no Console — but an operator who has made a choice is
	// not overruled at the next restart.
	if stored, ok, serr := db.GetButlerConfig(); serr != nil {
		log.Error("reading stored butler config; falling back to flags", "err", serr)
	} else if ok {
		log.Info("butler config from runtime state", "provider", stored.Provider,
			"model", stored.Model, "endpoint", stored.Endpoint)
		butlerCfg = butlerConfig{Provider: stored.Provider, Model: stored.Model, Endpoint: stored.Endpoint}
	}

	if bp, backend, err := newButlerProvider(butlerCfg); err != nil {
		return err
	} else {
		// Wired even when no provider is configured (ADR-W017 A9). A
		// fresh install defaults to -butler-provider off, and building
		// this only for an already-enabled one left the settings screen
		// answering 503 — so the Console could never turn the Butler ON,
		// and saving "off" made the state permanent. The one surface for
		// configuring the Butler must work from the state every
		// installation starts in.
		mode := runtimedb.ButlerModeQAOnly
		if bp != nil {
			mode, _ = probeButler(context.Background(), bp, log)
		}
		bgw := butler.NewToolGateway(state, &butler.AgentReader{Gateway: gw})
		// The diagnose tool's injected observed seam (ADR-W017 A5): the
		// butler package reads observed health through this closure and
		// never imports runtimedb/reconcile itself.
		bgw.Observed = func() ([]butler.ObservedResource, error) {
			rows, err := db.ListObserved()
			if err != nil {
				return nil, err
			}
			out := make([]butler.ObservedResource, 0, len(rows))
			for _, o := range rows {
				out = append(out, butler.ObservedResource{
					Kind: o.Kind, Name: o.Name, Node: o.Node, Healthy: o.Healthy, Detail: o.Detail,
				})
			}
			return out, nil
		}
		// Household rule authoring (ADR-W024 H7). Both seams are read
		// paths: one describes what a rule may say HERE, the other
		// replays a draft. Neither can enable a rule — proposing it
		// still produces a plan a human approves.
		bgw.AutomationVocabulary = func() (butler.HouseholdVocabulary, error) {
			v := butler.HouseholdVocabulary{}
			if cat != nil {
				seen := map[string]bool{}
				for _, p := range cat.List() {
					if p.Publishes == nil {
						continue
					}
					for _, ev := range p.Publishes.Events {
						if !seen[ev.Type] {
							seen[ev.Type] = true
							v.EventTypes = append(v.EventTypes, ev.Type)
						}
					}
				}
			}
			// What is REGISTERED, not what is declared: a rule naming a
			// tool no executor backs would be refused at dispatch, and
			// the household would find out when it did not happen.
			//
			// Chat executors are registered for every chat package in the
			// catalog and gated on the install at dispatch, so the same
			// gate applies here: a Butler that lists an action this
			// deployment has not installed will promise the household
			// something that cannot happen (ADR-W026 A8).
			for _, tool := range actions.Tools() {
				if capName, _, ok := strings.Cut(tool, "."); ok && strings.HasPrefix(capName, "chat-") {
					on, cerr := apiSrv.CapabilityEnabled(capName)
					if cerr != nil {
						return v, cerr
					}
					if !on {
						continue
					}
				}
				tier := "domestic"
				if actions.Declared != nil {
					capName, name, _ := strings.Cut(tool, ".")
					if declared, ok := actions.Declared(capName, name); ok {
						tier = declared
					}
				}
				v.Actions = append(v.Actions, butler.HouseholdAction{Tool: tool, Tier: tier})
			}
			return v, nil
		}
		bgw.DryRunAutomation = func(ctx context.Context, doc string, days int) (string, error) {
			rule, perr := automation.ParseDraft(doc)
			if perr != nil {
				return "", perr
			}
			if days <= 0 {
				days = int(automation.DefaultDraftWindow / (24 * time.Hour))
			}
			to := time.Now()
			replay, rerr := auto.ReplayDraft(ctx, rule, pol,
				butlerSessionPrincipal, string(runtimedb.RoleButler), to.AddDate(0, 0, -days), to, 0)
			if rerr != nil {
				return "", rerr
			}
			return replay.Summary(20), nil
		}

		// The family surface (ADR-W026 A1, G1): the SAME provider, a
		// different gateway. Its seams are the household timeline and
		// the domestic action gateway — the two things a household rule
		// already uses — and deliberately nothing else: there is no
		// Statefile, no node reader and no plan pipeline in this
		// wiring, so a family session has no path to one.
		// Which event types are ABOUT A PERSON, from the `subject:
		// member` each package declares for what it publishes (ADR-W024
		// A10). It is what lets a group session hear about the front
		// door and not about who came home (ADR-W026 A7), and it is read
		// per call so enabling a package mid-run changes the answer.
		memberScoped := api.MemberScopedTypes(func() map[string]bool {
			out := map[string]bool{}
			if cat == nil {
				return out
			}
			for _, p := range cat.List() {
				if p.Publishes == nil {
					continue
				}
				for _, ev := range p.Publishes.Events {
					if ev.Subject == "member" {
						out[ev.Type] = true
					}
				}
			}
			return out
		})
		household := butler.HouseholdSeams{
			Events:     api.HouseholdEventSeam(db, memberScoped),
			Vocabulary: bgw.AutomationVocabulary,
		}
		if len(actions.Tools()) > 0 {
			household.Actions = func(ctx context.Context, c butler.HouseholdActionCall) (string, error) {
				res, aerr := actions.Dispatch(ctx, domestic.Call{
					Tool:   c.Tool,
					Tier:   c.Tier,
					Params: c.Params,
					// The rule slot names WHO asked, because on this
					// surface nothing else did: a family session's action
					// has no Automation behind it, and an audit line
					// reading "butler-session" with the member as the
					// subject is the honest record of what happened.
					Rule:        "butler-session",
					Subject:     c.Member,
					TriggerType: "butler.household_session",
				})
				if aerr != nil {
					return "", aerr
				}
				return res.Summary, nil
			}
		}

		apiSrv.ButlerHost = &api.ButlerHost{
			Provider: bp, Gateway: bgw, Mode: mode, Household: household,
			Model: backend.Model, Endpoint: backend.Endpoint,
		}
		if bp != nil {
			log.Info("butler enabled", "provider", bp.Name(), "model", backend.Model,
				"endpoint", backend.Endpoint, "mode", string(mode))
		} else {
			log.Info("butler not configured; it can be enabled from the Console (ADR-W017 A9)")
		}

		// Runtime reconfiguration (ADR-W017 A9). Rebuilds the provider
		// for a new choice, re-runs the probe against THAT provider and
		// model, and swaps it in — no restart, so changing a model does
		// not drop agent sessions or propose a drift repair that reports
		// healthy resources as absent (F75/F76).
		//
		// The probe verdict is earned per configuration. A9 exists partly
		// because a startup probe that timed out against a cold model
		// pinned the Butler to QAOnly until the next restart, while the
		// model — once warm — called tools correctly.
		apiSrv.ApplyButlerConfig = func(ctx context.Context, c runtimedb.ButlerConfig) (runtimedb.ButlerSessionMode, string, error) {
			np, nbackend, err := newButlerProvider(butlerConfig{
				Provider: c.Provider, Model: c.Model, Endpoint: c.Endpoint,
			})
			if err != nil {
				return "", "", err
			}
			if np == nil {
				// "off" is a choice, not a failure.
				apiSrv.ButlerHost.Swap(nil, runtimedb.ButlerModeQAOnly, "", "")
				log.Info("butler disabled by configuration")
				return runtimedb.ButlerModeQAOnly, "provider set to off", nil
			}
			nmode, detail := probeButler(ctx, np, log)
			apiSrv.ButlerHost.Swap(np, nmode, nbackend.Model, nbackend.Endpoint)
			log.Info("butler reconfigured", "provider", np.Name(), "model", nbackend.Model,
				"endpoint", nbackend.Endpoint, "mode", string(nmode))
			return nmode, detail, nil
		}

		// Household escalation (ADR-W024 A6): the same provider, asked a
		// much smaller question. A Phraser is given no tools at all, and
		// its answer only ever replaces the wording of one already-
		// approved action param — so this wiring cannot widen what a
		// household rule may do, whatever the model is told.
		//
		// The QA-only verdict is not consulted: it grades tool-calling
		// reliability, and this call offers no tools. A model too
		// unreliable to operate the homelab can still write a sentence,
		// and if it cannot, the rule's own wording is used.
		// The Phraser follows the CONFIGURED provider, not the one this
		// process started with. Binding it to `bp` meant that switching
		// from a hosted provider to a local Ollama — or to off — still
		// sent household escalation facts to the old hosted provider,
		// which is contrary to the saved configuration and to the
		// privacy intent behind choosing local in the first place.
		phraser := &butler.Phraser{
			Provider: butlerProviderRef{host: apiSrv.ButlerHost},
			Model:    butlerCfg.Model,
			Record: func(_ context.Context, providerName, prompt, reply string) error {
				_, rerr := db.InsertButlerSession(runtimedb.ButlerSession{
					Principal: butlerSessionPrincipal,
					// An escalation has no human caller. Recording the
					// engine as the acting identity keeps ADR-W001 A1's
					// "no anonymous path" true rather than exempting the
					// one session type a person did not start.
					ActingFor:      automation.Source,
					Provider:       providerName,
					Mode:           runtimedb.ButlerModeFull,
					FinalText:      redact.String(reply),
					TranscriptJSON: redact.String(`{"kind":"household-escalation","prompt":` + strconv.Quote(prompt) + `}`),
				})
				return rerr
			},
		}
		auto.Escalator = automation.EscalatorFunc(func(ctx context.Context, req automation.EscalationRequest) (string, error) {
			facts := make([]butler.Fact, 0, len(req.Facts))
			for _, f := range req.Facts {
				facts = append(facts, butler.Fact{Label: f.Label, Content: f.Content})
			}
			return phraser.Phrase(ctx, butler.PhraseRequest{
				Rule: req.Rule, Instruction: req.Instruction, Fallback: req.Fallback, Facts: facts,
			})
		})
		// Named through the live reference, not the startup provider:
		// with no provider configured this says so, and after a change
		// it says what escalation is actually reaching.
		log.Info("household escalation enabled", "provider", butlerProviderRef{host: apiSrv.ButlerHost}.Name())
	}

	// Everything the engine needs is wired; start its loop.
	go auto.Run(context.Background(), automationEvery)

	// Morning report timer (ADR-W017 A5): once a day at 07:00 local, build
	// the deterministic report and append its summary to the event stream
	// (Console/ntfy consumers pick it up from there; GET
	// /api/v1/reports/morning serves the full report on demand).
	go func() {
		for {
			time.Sleep(time.Until(nextMorning(time.Now())))
			rep, err := report.Morning(db, time.Now())
			if err != nil {
				log.Error("morning report", "err", err)
				continue
			}
			if _, err := db.AppendEvent(runtimedb.Event{Type: "report.morning", Message: rep.Summary}); err != nil {
				log.Error("morning report event append", "err", err)
			}
			log.Info("morning report", "summary", rep.Summary)
		}
	}()

	var publicHandler http.Handler = apiSrv.Handler()
	if consoleDir != "" {
		publicHandler = withConsole(publicHandler, consoleDir)
		log.Info("serving console", "dir", consoleDir)
	}
	go func() {
		log.Info("public API listening", "addr", apiLis.Addr().String())
		if err := http.Serve(apiLis, publicHandler); err != nil {
			log.Error("public API exited", "error", err)
		}
	}()

	// Agent plane (blocks).
	log.Info("gateway listening", "addr", lis.Addr().String())
	return gw.Serve(lis)
}

// nextMorning returns the next 07:00 in now's location — the daily
// morning-report fire time.
func nextMorning(now time.Time) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), 7, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
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

// advertisedGatewayAddr derives the host:port a node should dial from the
// gateway's own configuration: the first -hosts SAN that isn't loopback,
// plus the port the gateway actually bound. The SANs are already the names the gateway
// certificate is valid for, so a name that isn't in there could not be
// used to join anyway. No routable SAN means no address to advertise —
// the pairing endpoint says so rather than emitting a loopback address a
// second machine cannot reach.
func advertisedGatewayAddr(hosts, listen string) string {
	return advertisedAddr(hosts, listen)
}

// advertisedAPIURL is the same answer for the PUBLIC API, as a URL: the
// address a container on a node dials to reach the control plane. It
// fills the `keystone_url` param source.
//
// It deliberately reuses the gateway's own selection rather than
// choosing differently. Two rules for "which of our addresses can a
// node reach" would drift, and an operator debugging one would have to
// discover the other; whichever SAN they put first governs both.
func advertisedAPIURL(hosts, listen string) string {
	addr := advertisedAddr(hosts, listen)
	if addr == "" {
		return ""
	}
	return "http://" + addr
}

// advertisedAddr picks the first -hosts SAN that isn't loopback and
// joins it to the port actually bound. The SANs are already the names
// the certificate is valid for, so a name that isn't in there could not
// be used anyway.
func advertisedAddr(hosts, listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return ""
	}
	for _, h := range strings.Split(hosts, ",") {
		h = strings.TrimSpace(h)
		switch h {
		case "", "localhost", "127.0.0.1", "::1":
			continue
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			continue
		}
		return net.JoinHostPort(h, port)
	}
	return ""
}

// seedRefs turns observed rows into per-node managed references, keeping
// only the rows that say a resource was actually there. See the seeding
// block in run() for why unhealthy rows are dropped.
func seedRefs(rows []runtimedb.Observed) map[string][]reconcile.ManagedRef {
	byNode := map[string][]reconcile.ManagedRef{}
	for _, o := range rows {
		if !o.Healthy {
			continue
		}
		byNode[o.Node] = append(byNode[o.Node], reconcile.ManagedRef{Kind: resource.Kind(o.Kind), Name: o.Name})
	}
	return byNode
}

// chatTransportFor reads the transport out of a chat capability's name.
// The catalog convention is chat-<transport>-<surface>, which is what
// lets one deployment run Telegram and Matrix side by side with a rule
// naming which it means.
func chatTransportFor(capability string) string {
	rest := strings.TrimPrefix(capability, "chat-")
	if i := strings.LastIndex(rest, "-"); i > 0 {
		return rest[:i]
	}
	return rest
}
