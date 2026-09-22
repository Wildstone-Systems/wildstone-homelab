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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
	"github.com/chickenrat17/wildstone-homelab/keystone/api"
	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/backup"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/consoleauth"
	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/gateway"
	"github.com/chickenrat17/wildstone-homelab/keystone/householdtz"
	"github.com/chickenrat17/wildstone-homelab/keystone/ingest"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile/loop"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/report"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
	"github.com/chickenrat17/wildstone-homelab/keystone/upkeep"
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
	if len(os.Args) > 1 && os.Args[1] == "person-refs" {
		if err := personRefs(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "keystone person-refs:", err)
			os.Exit(1)
		}
		return
	}
	// Host recovery (design §2.6). Here rather than on `wild` because
	// `wild` is an API client, and the whole point of this one is that
	// it works when the API will not let anybody in.
	if len(os.Args) > 1 && os.Args[1] == "owner" {
		if err := ownerCommand(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "keystone owner:", err)
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
	wallpaperDir := flag.String("wallpaper-dir", "", "directory of the pictures that ship with the themes, one per theme named <slug>.<ext> (ADR-W036); empty means the shipped themes arrive without pictures and only a household's own uploads are drawn")
	butlerProvider := flag.String("butler-provider", "off", "legacy single-provider Butler: off|anthropic|openai|ollama (ADR-W017 A4). Used ONLY while this deployment has created no Butlers; once one exists, each Butler answers on the model it was assigned (ADR-W031 A2). API keys come ONLY from ANTHROPIC_API_KEY/OPENAI_API_KEY env vars, never a flag")
	butlerModel := flag.String("butler-model", "", "model name for the legacy single-provider Butler; empty uses the provider's default. A named Butler's model comes from its assigned route, not from here")
	butlerEndpoint := flag.String("butler-endpoint", "", "base URL for the legacy single-provider Butler (openai-compatible gateways, ollama host); empty uses the provider's default. A named Butler's endpoint comes from its assigned route")
	automationEvery := flag.Duration("automation-interval", 15*time.Second, "how often the household automation engine ticks between event pokes (ADR-W024 H3)")
	householdTZ := flag.String("household-timezone", "Local", "IANA time zone the household's rules keep (timeOfDay/weekday conditions), e.g. Europe/London")
	ntfyURL := flag.String("ntfy-url", "", "ntfy base URL the notifications.notify household action publishes to (ADR-W024 H4); empty leaves that action unregistered. The access token comes ONLY from NTFY_TOKEN, never a flag")
	replyURL := flag.String("household-reply-url", "", "household event intake URL a notification's reply buttons post to, as a PHONE can reach it (not Keystone's loopback address); empty means notifications carry no reply buttons and a rule that asks for them fails loudly")
	replyTTL := flag.Duration("household-reply-ttl", 12*time.Hour, "how long a notification's reply token stays valid")
	// ADR-W033 A4.1: how old a Console sign-in may be to stay in the
	// Console, and to perform a consequential operation.
	consoleEntry := flag.Duration("console-entry-window", consoleauth.DefaultEntryWindow, "how long a Console sign-in lasts before Authelia is asked to authenticate again")
	consoleStepUp := flag.Duration("console-stepup-window", consoleauth.DefaultStepUpWindow, "how recent a sign-in a consequential Console operation requires")
	calendarURL := flag.String("calendar-url", "", "CalDAV collection the calendar.* household actions read and write, e.g. http://radicale:5232/wildstone-calendar/household/ (ADR-W034). Empty leaves them all unregistered, so a Butler does not offer the household a calendar this deployment has not got. The password comes ONLY from CALENDAR_PASSWORD, never a flag")
	calendarUser := flag.String("calendar-user", "", "directory account the calendar actions sign in to the CalDAV collection as; its password comes from CALENDAR_PASSWORD")
	householdIngress := flag.String("household-ingress", "", "comma-separated EXACT remote addresses of the identity proxy (Authelia). Empty fails closed. This names ONE trust boundary and it now gates TWO things: the household review page (ADR-W030 A8.1), and Console sign-in by forwarded identity (ADR-W032) — a request from one of these addresses carrying Remote-User and a Remote-Groups list containing console_users is authenticated as that person, with admin authority. Setting this does not by itself grant anyone the Console: they must also be in the console_users directory group, which packages/wildstone-console declares")
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
		CalendarURL: *calendarURL, CalendarUser: *calendarUser,
		Ingress:            splitNonEmpty(*householdIngress),
		ConsoleEntryWindow: *consoleEntry, ConsoleStepUpWindow: *consoleStepUp,
	}
	if err := run(*listen, *apiListen, *joinTokens, *hosts, *dataDir, *ingestEvery, *reconcileEvery, *managedPrefix, *backupEvery, *resticImage, *consoleDir, *catalogDir, *wallpaperDir, butlerCfg, householdCfg, log); err != nil {
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
	// A token minted for one job should not outlive it. The installer
	// asks for one that lasts the install: a claim code left behind with
	// no expiry is a whole-control-plane credential sitting in a file,
	// and bearer tokens do not need a fresh sign-in (ADR-W033 A4.1).
	ttl := fs.Duration("ttl", 0, "how long the token is valid (0 = no expiry, for recovery by hand)")
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

	if *ttl < 0 {
		return fmt.Errorf("-ttl must not be negative")
	}
	token, err := (&principal.Store{DB: db}).EnsureAdminFor(*name, *ttl)
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

// coordinatorHolder identifies THIS deployment for the runtime
// database's single-coordinator lease (ADR-W030 A11).
//
// Two properties are required of it, and both are easy to get wrong.
//
// It must be STABLE across restarts, because AcquireCoordinator treats
// re-acquiring your own lease as a normal restart and keeps the epoch.
// A holder built from a pid, a boot id or a random value would make
// every restart look like a second coordinator arriving, and recovering
// from an ordinary crash would need an administrative promotion.
//
// It must be DISTINCT per host, because two machines pointed at one
// data directory is precisely the situation the lease exists to refuse.
// A holder that ignored the host would let both believe they held it,
// which is the racing-schedulers failure with the safeguard switched on.
func coordinatorHolder(dataDir string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		// A host that cannot name itself still needs one id rather than
		// a different one each boot; the data directory keeps it
		// distinct from another deployment on the same machine.
		host = "unknown-host"
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	return host + ":" + abs
}

func run(listen, apiListen, joinTokens, hosts, dataDir string, ingestEvery, reconcileEvery time.Duration, managedPrefix string, backupEvery time.Duration, resticImage, consoleDir, catalogDir, wallpaperDir string, butlerCfg butlerConfig, householdCfg householdConfig, log *slog.Logger) error {
	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	apiLis, err := net.Listen("tcp", apiListen)
	if err != nil {
		return err
	}
	return serve(lis, apiLis, joinTokens, hosts, dataDir, ingestEvery, reconcileEvery, managedPrefix, backupEvery, resticImage, consoleDir, catalogDir, wallpaperDir, butlerCfg, householdCfg, log, os.Stdout)
}

// householdConfig carries the household automation flags into serve
// (ADR-W024 H3).
type householdConfig struct {
	// How old a Console sign-in may be to stay in the Console and to
	// perform a consequential operation (ADR-W033 A4.1).
	ConsoleEntryWindow, ConsoleStepUpWindow time.Duration
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
	// Ingress is the exact set of remote addresses the identity proxy
	// speaks from. Empty leaves the household review page unavailable
	// rather than open (ADR-W030 A8.1).
	Ingress []string
	// CalendarURL is the CalDAV collection the calendar.* actions act
	// on; empty leaves them all unregistered (ADR-W034).
	CalendarURL string
	// CalendarUser is the directory account those actions sign in as.
	// Its password arrives in CALENDAR_PASSWORD, never in a flag — the
	// same division -ntfy-url and NTFY_TOKEN already make.
	CalendarUser string
}

// confirmWindow is how long a confirm-tier question stays open, and
// therefore also how long one yes is good for. Short, because the whole
// premise is that somebody is there: a question still unanswered ten
// minutes later is one the household has moved on from.
const confirmWindow = 10 * time.Minute

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
	// Keystone's own household actions, declared like a package's.
	out := append([]domestic.AgentToolSource(nil), domestic.BuiltinAgentTools...)
	for _, p := range cat.List() {
		if len(p.AgentTools) == 0 {
			continue
		}
		tools := map[string]string{}
		optIn := map[string]bool{}
		for _, t := range p.AgentTools {
			tools[t.Name] = t.Risk
			if t.OptIn {
				optIn[t.Name] = true
			}
		}
		out = append(out, domestic.AgentToolSource{
			Capability: p.Metadata.Capability, Tools: tools, OptIn: optIn})
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

// butlerProviderFor builds the client for ONE Butler, from its own
// assignment and from nothing else. There is no argument here that
// could come from a message: it is handed a Butler and reads what its
// assignment says.
//
// It is a named function rather than a closure because it is the one
// place a Butler's assignment becomes a provider call, and it had a bug
// nothing could see: it passed Model where ProviderModel belonged.
// Those are the same string for anything configured from the provider's
// list and differ for a row named before that, so a Butler on
// "ollama-cloud" asked Ollama for a model by that name and got a 404
// naming something the person had never typed.
func butlerProviderFor(b runtimedb.Butler) (provider.Provider, error) {
	p, _, err := newButlerProvider(butlerConfig{
		Provider: b.Provider, Model: b.ProviderModel, Endpoint: b.Endpoint,
	})
	return p, err
}

// newButlerProvider builds the configured Butler provider, or a nil
// provider when the Butler is off. API keys come ONLY from the provider's
// environment variable (ADR-W017 A4) — there is deliberately no key flag.
func newButlerProvider(cfg butlerConfig) (provider.Provider, butlerBackend, error) {
	if cfg.Provider == "" || cfg.Provider == "off" {
		return nil, butlerBackend{}, nil
	}
	// The catalog decides which kinds exist, which client each speaks
	// and which environment variable holds its key (provider.Kinds).
	kind, ok := provider.LookupKind(cfg.Provider)
	if !ok {
		names := []string{"off"}
		for _, k := range provider.Kinds {
			names = append(names, k.Name)
		}
		return nil, butlerBackend{}, fmt.Errorf("unknown -butler-provider %q (want %s)", cfg.Provider, strings.Join(names, "|"))
	}
	key := kind.Key()
	if kind.KeyRequired && key == "" {
		return nil, butlerBackend{}, fmt.Errorf("provider %s requires %s in the environment", kind.Name, kind.KeyEnv)
	}
	endpoint := kind.Endpoint(cfg.Endpoint)
	switch kind.Client {
	case "anthropic":
		p := provider.NewAnthropicProvider(key)
		if cfg.Model != "" {
			p.Model = cfg.Model
		}
		if endpoint != "" {
			p.BaseURL = endpoint
		}
		return p, butlerBackend{Model: p.Model, Endpoint: p.BaseURL}, nil
	case "openai":
		if endpoint == "" {
			return nil, butlerBackend{}, fmt.Errorf("provider %s needs an endpoint: the server's OpenAI-compatible base URL, ending in /v1", kind.Name)
		}
		p := provider.NewOpenAIProvider(endpoint, key)
		if cfg.Model != "" {
			p.Model = cfg.Model
		}
		return p, butlerBackend{Model: p.Model, Endpoint: p.BaseURL}, nil
	default: // ollama
		p := provider.NewOllamaProvider(endpoint)
		if cfg.Model != "" {
			p.Model = cfg.Model
		}
		return p, butlerBackend{Model: p.Model, Endpoint: p.Host}, nil
	}
}

// withConsole mounts the built Console's static files beside the API
// (ADR-W016 A5): /api/* stays the API handler untouched; everything else
// serves a file from dir, falling back to index.html for client-side
// routes (the SPA convention). Path traversal is handled by
// http.ServeFile/FileServer's own cleaning.
// uiReachable decides whether this peer may be served the Console's
// own pages. It is a parameter rather than a direct call into the API
// server so the refusal can be tested without one, and nil means the
// pages are served to everyone — the behaviour before this gate, and
// what a caller with no trust boundary to enforce wants.
func withConsole(apiHandler http.Handler, dir string, uiReachable func(*http.Request) bool) http.Handler {
	fileServer := http.FileServer(http.Dir(dir))
	mux := http.NewServeMux()
	mux.Handle("/api/", apiHandler)
	// The household portal's pages are served by the API handler too,
	// and they are NOT under /api — they are pages, at the addresses a
	// dashboard iframes. Without this the Console's catch-all answers
	// them with its own index.html, which renders as a blank frame on
	// the family's dashboard with nothing anywhere saying why.
	for _, prefix := range api.PortalPrefixes {
		mux.Handle(prefix, apiHandler)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Refused before any file is considered, including /assets.
		// Gating index.html alone would still hand out the bundle, and
		// the bundle is the Console — a copy served to a browser that
		// reached this port directly would talk to /api with a bearer
		// token and never involve the proxy, which is the arrangement
		// this gate exists to end.
		if uiReachable != nil && !uiReachable(r) {
			http.Error(w,
				"the Console is served through this household's identity proxy; "+
					"reach it at the Console's own address and sign in there",
				http.StatusForbidden)
			return
		}
		if r.URL.Path != "/" {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(path.Clean(r.URL.Path), "/")))); err == nil {
				// Everything under /assets carries a content hash in its
				// filename, so a given URL's bytes never change. Those
				// can be cached hard and forever; a new build produces
				// new names rather than new content at old names.
				if strings.HasPrefix(r.URL.Path, "/assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		// index.html is the one file whose URL stays the same while its
		// content changes, because it names the hashed bundle. Serving
		// it with no cache directive left browsers free to reuse an old
		// copy on their own judgement — which points at an asset the
		// deploy has since deleted, so an upgraded Console renders as
		// the version before it, or as nothing at all.
		//
		// no-cache is not "do not store": it stores and REVALIDATES, so
		// the usual response is a 304 and the cost is one conditional
		// request per load.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})
	return mux
}

// serve wires and runs both planes over already-bound listeners —
// separated from run so the boot integration test can use :0 listeners.
// bootstrapOut receives the one-time admin token on first start.
func serve(lis, apiLis net.Listener, joinTokens, hosts, dataDir string, ingestEvery, reconcileEvery time.Duration, managedPrefix string, backupEvery time.Duration, resticImage, consoleDir, catalogDir, wallpaperDir string, butlerCfg butlerConfig, householdCfg householdConfig, log *slog.Logger, bootstrapOut io.Writer) error {
	// recordNode is bound once the runtime DB is open, a few lines below.
	// The indirection is safe without synchronisation: the gateway cannot
	// deliver a node event before gw.Serve, which is the last statement in
	// this function, long after the assignment.
	// Provider keys entered in the Console live beside the runtime DB,
	// root-only, and are loaded before anything can build a provider
	// client (ADR-W017 A10). A file that cannot be read is logged and
	// left alone — the environment still works — rather than stopping
	// the control plane over an optional convenience.
	if err := provider.UseKeyFile(filepath.Join(dataDir, "provider-keys.json")); err != nil {
		log.Error("provider keys", "err", err)
	}
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

	// The single active coordinator lease (ADR-W030 A11), taken before
	// anything can dispatch: durable work is only ever written by the
	// process that owns this database.
	//
	// Finding another holder is deliberately NOT fatal. Refusing to
	// start would make the situation unrecoverable — promotion is an API
	// call, and an API that will not come up cannot serve it. So this
	// process runs, serves every read path, and wires no durable Butler
	// work. That is the store's own rule ("a second process that finds
	// another holder refuses to dispatch and says so") applied where
	// dispatch actually happens, rather than one step earlier where it
	// would take the recovery route down with it.
	//
	// Nothing releases the lease. There is no Release call and there is
	// deliberately no timeout takeover: a crash mid-call leaves the lease
	// held by this same holder id, and the restart re-acquires it at the
	// same epoch, which is what lets the runner resume a task from its
	// checkpoints instead of finding the work orphaned. Handing it to a
	// different host is PromoteCoordinator, an explicit act that fences
	// the old epoch.
	lease, leaseErr := db.AcquireCoordinator(coordinatorHolder(dataDir))
	coordinating := leaseErr == nil
	switch {
	case coordinating:
		log.Info("coordinator lease held", "holder", lease.Holder, "epoch", lease.Epoch,
			"acquiredAt", lease.AcquiredAt.Format(time.RFC3339))
	case errors.Is(leaseErr, runtimedb.ErrCoordinatorHeld):
		log.Error("another coordinator holds this runtime database; no durable Butler work is wired in this process. "+
			"Stop the other process and restart, or POST /api/v1/coordinator/promote to take it over deliberately",
			"err", leaseErr)
	default:
		return fmt.Errorf("taking the coordinator lease: %w", leaseErr)
	}

	// Heartbeats inform the human takeover decision; they never auto-promote.
	if coordinating {
		go func() {
			if err := db.RunCoordinatorHeartbeat(context.Background(), 30*time.Second); err != nil {
				log.Warn("coordinator heartbeat stopped", "err", err)
			}
		}()
	}

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

	// The catalog, loaded BEFORE the reconcile loop starts rather than
	// beside the gateway where it used to sit. The loop reads the access
	// groups every package declares (slice 8), and `go svc.Run` is a few
	// lines below — assigning the field after that is a data race on the
	// house's access rules, which is not a place to have one.
	var cat *catalog.Catalog
	if catalogDir != "" {
		loaded, cerr := catalog.LoadDir(catalogDir)
		if cerr != nil {
			return cerr
		}
		cat = loaded
		log.Info("catalog loaded", "dir", catalogDir, "packages", len(cat.List()))
	}

	// Reconcile service (ADR-W008): converges approved plans and drift.
	// Providers (Cloudflare DnsRecord) are wired when the owner supplies
	// a token — an E6 configuration step; until then a DnsRecord in
	// desired surfaces as an incident, never a silent skip (ADR-W004 A5).
	svc := &loop.Loop{
		State: state,
		DB:    db,
		// Capability -> the household groups its manifest declares it
		// is for. Plain data, so the loop needs no catalog of its own.
		AccessGroups: accessGroupsByCapability(cat),
		Rec: &reconcile.Reconciler{
			Applier:       &reconcile.GatewayApplier{Gateway: gw},
			ManagedPrefix: managedPrefix,
			// Which protocol minor each connected agent speaks, so a
			// spec that needs minor 12 is refused on an older node
			// rather than half-applied there.
			AgentProtocolMinor: func(nodeID string) (uint32, bool) {
				for _, n := range gw.LiveNodes() {
					if n.NodeID == nodeID {
						return n.ProtocolMinor, true
					}
				}
				return 0, false
			},
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
		seeded, machines, keystoneOwned := 0, 0, 0
		for node, refs := range byNode {
			svc.Rec.SeedManaged(node, refs)
			seeded += len(refs)
			// A KEYSTONE-OWNED RESOURCE HAS NO NODE, and counting its
			// bucket as a machine made this line say nodes=2 on a
			// household that runs one. Commitments are applied at no
			// node (ADR-W024's Automation says the same of itself), so
			// the empty key appeared the moment somebody added the
			// first routine — and "2" is exactly the sort of number an
			// operator reads once, believes, and spends an afternoon on.
			if node == "" {
				keystoneOwned = len(refs)
				continue
			}
			machines++
		}
		log.Info("managed state restored", "nodes", machines,
			"resources", seeded, "keystoneOwned", keystoneOwned, "rows", len(rows))
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

	// Domestic gateway (ADR-W024 A5, H4): the only way a household rule
	// acts. Executors are registered here and nowhere else; a rule that
	// names a tool this deployment has not wired is refused with a
	// reason on the household stream rather than failing silently.
	actions := &domestic.Gateway{Authority: db, Log: log}
	if cat != nil {
		// Only with a catalog: DeclaredFromPackages over an empty set
		// would refuse every action, which is not what "no catalog" means.
		actions.Declared = domestic.DeclaredFromPackages(agentToolSources(cat))
	}
	// Asking a household member to confirm one action (ADR-W024 A5.1).
	//
	// It rides the NOTICES table, which is the household's own
	// ask-and-answer channel and deliberately not a notification
	// capability: a notice is per member, expiring, single-use, and it
	// is rendered wherever that member actually is — the Home page
	// today, and any surface that learns to render one tomorrow. A
	// household with no ntfy installed can still be asked.
	//
	// What authorises a retry is a row this function reads, written by
	// runtimedb.AnswerNotice, which checks that the answerer is the
	// person the question was asked of, that it has not expired, that it
	// was not already answered, and that the reply resolves to a clear
	// yes or no. The model never sees a token, so there is nothing for
	// it to replay: it gets a refusal, and the next attempt succeeds
	// only because a person answered in the meantime.
	actions.Confirm = func(_ context.Context, req domestic.ConfirmRequest) (domestic.Confirmation, error) {
		settings, _, serr := db.GetButlerSettings()
		if serr != nil {
			return domestic.ConfirmAsked, serr
		}
		now := time.Now()
		// The dedupe key carries a TIME BUCKET as well as the call.
		//
		// Without the call, one yes would authorise any later action.
		// Without the bucket, the key would match forever — a notices
		// dedupe is not time-scoped — so the SAME question could never
		// be asked twice, and a member who let one expire could never
		// be asked again. One bucket is one confirmation window.
		bucket := now.Unix() / int64(confirmWindow.Seconds())
		decision, rerr := db.RaiseNotice(runtimedb.NoticeRequest{
			Kind:          "household.confirm",
			Recipient:     req.Member,
			CorrelationID: req.CorrelationID,
			DedupeKey:     fmt.Sprintf("household-confirm:%s:%d", req.Key, bucket),
			Body:          req.Question,
			Asks:          true,
			TTL:           confirmWindow,
		}, settings, now)
		if rerr != nil {
			return domestic.ConfirmAsked, rerr
		}
		if decision.Sent {
			return domestic.ConfirmAsked, nil
		}
		// Deduplicated onto a question already put to them: read where
		// they have got to with it.
		n, gerr := db.GetNotice(decision.Seq)
		if gerr != nil {
			return domestic.ConfirmAsked, gerr
		}
		if n.AnsweredAt.IsZero() {
			return domestic.ConfirmPending, nil
		}
		// An answer is spent when it is used. AcknowledgeNotice only
		// succeeds on a notice not already acknowledged, so this is the
		// atomic single-use marker — two dispatches racing on one yes
		// cannot both win it.
		if aerr := db.AcknowledgeNotice(n.Seq); aerr != nil {
			return domestic.ConfirmPending, nil
		}
		if approved, ok := runtimedb.AnswerApproves(n.Answer); ok && approved {
			return domestic.ConfirmApproved, nil
		}
		return domestic.ConfirmDenied, nil
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
		actions.Register(&domestic.Notifier{Authority: db,
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
	// The household's timezone is a SETTING (ButlerSettings.Timezone),
	// which an Owner can change — in the Console, or by approving an
	// Agent's proposal — and everything that keeps the household's time
	// reads it live through householdZone. -household-timezone seeds it
	// when it has never been set (no row, or still the UTC default).
	householdZone := householdtz.New(householdLoc)
	if settings, stored, serr := db.GetButlerSettings(); serr != nil {
		return fmt.Errorf("reading the household timezone: %w", serr)
	} else if explicit := householdCfg.Timezone != "" && householdCfg.Timezone != "Local"; explicit &&
		(!stored || settings.Timezone == "UTC") && settings.Timezone != householdCfg.Timezone {
		settings.Timezone = householdCfg.Timezone
		settings.UpdatedBy = "keystone -household-timezone"
		if err := db.SetButlerSettings(settings); err != nil {
			return fmt.Errorf("seeding the household timezone: %w", err)
		}
		householdZone.Set(householdLoc)
	} else if stored && settings.Timezone != "UTC" {
		householdZone.Set(settings.Location())
	}
	// A household clock that is UTC because nobody said otherwise is
	// almost never where the household lives — servers and VMs run on
	// UTC — and nothing else says so: the Butler is told UTC is the
	// household's clock, and "3pm" lands five hours off for a family in
	// Chicago. Said at every start until it is set.
	zoneName := householdZone.Location().String()
	if _, offset := time.Now().In(householdZone.Location()).Zone(); zoneName == "UTC" || zoneName == "Etc/UTC" ||
		(zoneName == "Local" && offset == 0) {
		log.Warn("the household timezone is UTC: times the household says (\"3pm\", \"tomorrow\") are read as UTC. "+
			"Set it in the Console (Butler settings), or with -household-timezone, to where the household lives",
			"zone", householdZone.Location().String(), "example", "America/Chicago")
	}
	// The calendar's household actions (ADR-W034 phase 4).
	//
	// Registered whenever a collection is configured, which is the human
	// decision this path has: an operator who pointed Keystone at a
	// calendar and gave it a credential has said the household's agents
	// may use it. That is the same gate -ntfy-url is, and it is a
	// DIFFERENT gate from the one on the adapter's work-queue path,
	// where a connection has to be widened before an action reaches an
	// adapter at all. Both are a person deciding; they are just
	// different people's decisions on different roads (ADR-W034 A5).
	var calendarActions *domestic.Calendar
	if householdCfg.CalendarURL != "" {
		cal := &domestic.Calendar{Authority: db,
			Upstream: &caldav.Upstream{BeforeWrite: db.CheckHouseholdExecution,
				CollectionURL: householdCfg.CalendarURL,
				Username:      householdCfg.CalendarUser,
				Password:      os.Getenv("CALENDAR_PASSWORD"),
				Location:      householdLoc,
				Zone:          householdZone,
				// Writable because the tools are the point. The read
				// tools work without it; the three write ones would
				// register and refuse, which is a Butler offering the
				// household something that cannot happen.
				Writable: true,
			},
			Location: householdLoc,
			Zone:     householdZone,
		}
		for _, e := range cal.Executors() {
			actions.Register(e)
		}
		// The hand-off seam is bound after apiSrv exists; see below.
		calendarActions = cal
		if householdCfg.CalendarUser == "" || os.Getenv("CALENDAR_PASSWORD") == "" {
			// Loud, because the alternative is silent: Radicale answers
			// 401 to every read, the tools are registered and fail,
			// and the capability reports as installed the whole time.
			// That is the shape ADR-W029 exists to catch — a converged
			// install that does not work — and the only place it can be
			// caught here is startup.
			log.Warn("calendar actions are wired but have no credential: "+
				"set -calendar-user and CALENDAR_PASSWORD, or the calendar will refuse every request",
				"user", householdCfg.CalendarUser, "password", os.Getenv("CALENDAR_PASSWORD") != "")
		}
		log.Info("calendar actions wired", "collection", householdCfg.CalendarURL,
			"user", householdCfg.CalendarUser, "credential", os.Getenv("CALENDAR_PASSWORD") != "")
	}

	automationEvery := householdCfg.Interval
	if automationEvery <= 0 {
		automationEvery = 15 * time.Second
	}
	auto := &automation.Engine{
		DB: db, State: state, Log: log, Location: householdLoc, Zone: householdZone, Actions: actions,
	}
	// Run starts below, once the Butler wiring has had its chance to
	// install the escalator: the engine's fields are read by its loop,
	// so everything it needs is set before that loop exists.

	// Public plane. Plan approval pokes the reconcile service (R2a); a
	// new household event pokes the automation engine.
	apiSrv := api.New(principals, db, state, log)
	secretVault, err := db.OpenSecretVault(filepath.Join(dataDir, "household-secrets.key"))
	if err != nil {
		return fmt.Errorf("household secrets: %w", err)
	}
	apiSrv.SecretVault = secretVault
	if calendarActions != nil {
		calendarActions.SecretUse = calendarSecretUse(secretVault)
	}

	// Every Agent is on the assigned-tools model (ADR-W033 design §6.3,
	// §6.4). Agents made before 0059 are brought onto it here, once,
	// because the default set is read off the gateways and a migration
	// cannot see them. Idempotent: an already-enrolled Agent is skipped,
	// so a tool an Owner removed does not come back on a restart.
	if err := apiSrv.BackfillAgentTools(); err != nil {
		return fmt.Errorf("backfilling agent tools: %w", err)
	}
	// The pictures the themes ship with. Counted and logged rather than
	// assumed: a wrong or unpopulated -wallpaper-dir is otherwise a
	// silent "the themes have no pictures", which is exactly the
	// failure this directory exists to end.
	apiSrv.WallpaperDir = wallpaperDir
	if wallpaperDir != "" {
		log.Info("theme wallpapers loaded", "dir", wallpaperDir,
			"pictures", apiSrv.ShippedWallpaperCount())
	}
	apiSrv.ConsoleEntryWindow, apiSrv.ConsoleStepUpWindow = householdCfg.ConsoleEntryWindow, householdCfg.ConsoleStepUpWindow
	apiSrv.HouseholdLoc = householdLoc
	apiSrv.HouseholdZone = householdZone
	// An Agent that hears the household's clock is wrong can propose the
	// right zone; an Owner approves it (ADR-W033 E2).
	actions.Register(&domestic.TimezoneProposer{Propose: apiSrv.ProposeHouseholdTimezone})
	// The household review page is reachable only from the identity
	// proxy. Exact addresses, not a CIDR or a hostname: the proxy is
	// one container on one network, and an exact match cannot be
	// widened by a DNS answer (ADR-W030 A8.1).
	apiSrv.SetTrustedIngress(householdCfg.Ingress)
	// Observed, unless the operator pinned it.
	//
	// An explicit -household-ingress is a decision about a trust
	// boundary and discovery must not quietly overrule it — someone who
	// names an address has a reason, and finding the Console trusting a
	// different peer than the flag says would be the worst kind of
	// surprise. With the flag unset, the reconciler supplies the value
	// instead, which is what keeps it right after the proxy's container
	// is recreated with a new address.
	if len(householdCfg.Ingress) == 0 {
		var lastIngress string
		svc.Rec.NoteIngressAddrs = func(nodeID string, addrs []string) {
			// Only on a change. This fires every reconcile tick, and a
			// log line per tick would bury the one that matters — the
			// tick where the address actually moved, which is the only
			// event anybody debugging a locked-out Console wants.
			key := nodeID + " " + strings.Join(addrs, ",")
			if key == lastIngress {
				return
			}
			lastIngress = key
			apiSrv.SetTrustedIngress(addrs)
			log.Info("identity proxy address observed",
				"node", nodeID, "addresses", addrs,
				"gates", "household review page, household portal, and the Console")
		}
	} else {
		log.Info("identity proxy address pinned by -household-ingress",
			"addresses", householdCfg.Ingress,
			"note", "discovery is off; update the flag if the proxy container is recreated with a new address")
	}
	if len(apiSrv.TrustedIngressAddrs()) == 0 {
		// A starting state, not a verdict. With the flag unset the
		// reconciler supplies the address on its first tick against a
		// node that has an ingress, so this says "not yet" rather than
		// "not ever" — the previous wording promised a permanence that
		// is no longer true, and an operator who read it would go
		// looking for a flag they do not need to set.
		log.Info("no identity proxy address yet: the household review page and Console " +
			"sign-in by forwarded identity are off until one is observed, so the Console " +
			"needs a bearer token for now. A household with no ingress installed stays " +
			"this way, which is the supported posture, not a fault")
	} else {
		// Said out loud because this address list is now the boundary
		// for an ADMIN identity, not only for a household member's
		// routine approval. An operator who set it for the review page
		// should learn here that it does more.
		log.Info("trusted identity proxy configured",
			"addresses", apiSrv.TrustedIngressAddrs(),
			"gates", "household review page, and Console sign-in for members of "+api.ConsoleGroup)
	}
	// Probing one model, for the Console's assignment and Re-check.
	//
	// It builds a provider for THAT model rather than reusing the
	// running one: the whole point is to check a model this deployment
	// may never have talked to, and asking the configured backend
	// whether a different model works would answer about the wrong one.
	apiSrv.ProbeModel = func(ctx context.Context, providerName, model, endpoint string) (string, string) {
		p, _, err := newButlerProvider(butlerConfig{Provider: providerName, Model: model, Endpoint: endpoint})
		if err != nil || p == nil {
			detail := "this deployment cannot build a client for that provider"
			if err != nil {
				detail = err.Error()
			}
			return runtimedb.ModelUnavailable, detail
		}
		mode, detail := probeButler(ctx, p, log)
		if mode == runtimedb.ButlerModeFull {
			return runtimedb.ModelReady, "probe passed: it called the tool it was asked to"
		}
		// A model that answered but would not call a tool is qa-only; one
		// that did not answer at all is unavailable, and the difference
		// is what a person needs to know before assigning it.
		if strings.Contains(strings.ToLower(detail), "deadline") ||
			strings.Contains(strings.ToLower(detail), "connection") ||
			strings.Contains(strings.ToLower(detail), "refused") {
			return runtimedb.ModelUnavailable, detail
		}
		return runtimedb.ModelQAOnly, detail
	}
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
	// The household drop box's actions (packages/filedrop-wildstone).
	//
	// Wired here rather than at the gateway's construction because both
	// its seams are the API server's methods, which is the same reason
	// the chat executors are below — keystone/domestic learns neither
	// the runtime DB nor the package format.
	//
	// UNCONDITIONAL REGISTRATION, GATED PER CALL. The storage root is a
	// ConfigFile the capability renders, so it arrives in desired state
	// weeks after this process started; reading it once here would pin
	// the answer to whatever was true before the household installed
	// anything. Enabled is the install gate (ADR-W026 A8's posture),
	// and the domestic gateway's declaration check is in front of both.
	if cat != nil {
		filedrop := &domestic.Filedrop{Authority: db,
			Settings: apiSrv.FiledropSettings,
			Enabled:  func() (bool, error) { return apiSrv.CapabilityEnabled(domestic.FiledropCapability) },
		}
		for _, e := range filedrop.Executors() {
			actions.Register(e)
		}
	}
	// Wire the chat delivery executors now that the API server exists:
	// both seams are its methods, so keystone/domestic learns neither
	// the runtime DB nor a chat vendor (ADR-W026 G6).
	for _, capName := range chatCapabilities {
		transport := chatTransportFor(capName)
		capName := capName
		actions.Register(&domestic.ChatSender{Authority: db,
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
				target, bindingID, connection, lerr := apiSrv.ChatRecipientFor(transport, member)
				if lerr != nil {
					return domestic.ChatRecipient{}, lerr
				}
				return domestic.ChatRecipient{
					Transport: transport, Target: target,
					BindingID: bindingID, Connection: connection,
				}, nil
			},
			Ask: func(ctx context.Context, r domestic.ChatRecipient, body, correlationID, rule, subject string) (int64, error) {
				// The Connection the recipient was found on: a rule's
				// message leaves by the same Agent's bot the person is
				// admitted to, rather than by whichever bot serves the
				// transport (ADR-W033 B7).
				at, aerr := db.ChatConnectionBySeq(r.Connection)
				if aerr != nil {
					return 0, aerr
				}
				return apiSrv.AskOverChat(ctx, at, r.Target, r.BindingID, body, correlationID, rule, subject)
			},
		})
	}
	if len(chatCapabilities) > 0 {
		log.Info("chat delivery wired", "capabilities", chatCapabilities, "tools", actions.Tools())
	}

	apiSrv.CAFingerprint = gw.CA().Fingerprint()
	apiSrv.GatewayAddr = advertisedGatewayAddr(hosts, lis.Addr().String())
	apiSrv.APIURL = advertisedAPIURL(hosts, apiLis.Addr().String())
	apiSrv.HouseholdTimezone = householdCfg.Timezone
	apiSrv.CalendarCollectionURL = householdCfg.CalendarURL
	// calendar.how_to_add answers from desired state, because where a
	// Route is actually served changes with the ingress posture — so it
	// is resolved per call through the API server's own resolver rather
	// than frozen into a string at startup. keystone/domestic holds no
	// Statefile; this closure is the whole of its access, and it returns
	// four strings about one member.
	if calendarActions != nil {
		// The home page's calendar card, reading the SHARED calendar
		// through the same upstream the actions use — one credential,
		// one client, one place a change to either lands.
		apiSrv.CalendarEvents = func(from, to time.Time) ([]api.CalendarCardEvent, error) {
			events, eerr := calendarActions.Upstream.Events(context.Background(), from, to)
			if eerr != nil {
				return nil, eerr
			}
			sort.Slice(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })
			out := make([]api.CalendarCardEvent, 0, len(events))
			for _, e := range events {
				out = append(out, api.CalendarCardEvent{Title: e.Summary, Start: e.Start, AllDay: e.AllDay})
			}
			return out, nil
		}
		// The calendar page writes through the very same upstream.
		apiSrv.CalendarBook = calendarActions.Upstream
		// Bookings follow the calendar events they made: an appointment
		// moved or deleted on a phone moves or cancels the booking here.
		// Every two minutes, because a household that moves the dentist
		// and then looks at Coming up should see it moved, and one read
		// per linked booking is cheap.
		go func() {
			for {
				time.Sleep(2 * time.Minute)
				if n, serr := apiSrv.SyncCalendarBookings(context.Background()); serr != nil {
					log.Warn("calendar bookings sync", "err", serr)
				} else if n > 0 {
					log.Info("calendar bookings sync", "changed", n)
				}
			}
		}()
		calendarActions.Handoff = func(member string) (domestic.CalendarHandoff, bool, error) {
			h, ok, herr := apiSrv.CalendarHandoffFor(member)
			if herr != nil || !ok {
				return domestic.CalendarHandoff{}, ok, herr
			}
			return domestic.CalendarHandoff{
				Server: h.Server, Username: h.Username,
				OwnCalendars: h.OwnCalendars, HouseholdCalendar: h.HouseholdCalendar,
				ProfileURL: h.ProfileURL,
			}, true, nil
		}
	}
	apiSrv.ManagedPrefix = managedPrefix
	// The coordinator routes need this process's own lease identity, so
	// a promotion writes the id THIS deployment will re-acquire at its
	// next restart rather than one nobody holds.
	apiSrv.CoordinatorHolder = coordinatorHolder(dataDir)
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

	// The commitment pass (ADR-W035 A15). Declared here because the
	// agent's read tool needs it just below and the timer starts it
	// further down — one engine either way, so the household's answer to
	// "what is due" is the same whoever asks.
	upkeepEngine := &upkeep.Engine{DB: db, State: state, Loc: householdLoc, Zone: householdZone, Log: log}

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
		// Where the household's commitments stand, for the agent's read
		// tool. The adapter is in this package because neither the pass
		// nor the model should import the other (commitmentstandings.go).
		bgw.Commitments = commitmentStandings{engine: upkeepEngine}
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
				// filedrop is registered from the catalog too, and is
				// gated on the install for the same reason: a Butler
				// that lists "find_file" to a household with no drop
				// box has promised them something that cannot happen.
				if capName, _, ok := strings.Cut(tool, "."); ok &&
					(strings.HasPrefix(capName, "chat-") || capName == domestic.FiledropCapability) {
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
				v.Actions = append(v.Actions, butler.HouseholdAction{Tool: tool, Tier: tier, Usage: actions.Usage(tool)})
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
		// The same rule the family timeline uses, so /today withholds
		// exactly what a spoken answer would.
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
		// /today reads the timeline directly rather than through the
		// seam, so it needs the same rule the seam applies.
		apiSrv.MemberScoped = memberScoped
		household := butler.HouseholdSeams{
			Events:     api.HouseholdEventSeam(db, memberScoped),
			Vocabulary: bgw.AutomationVocabulary,
			// The Home page's four readings, for a member who is in a
			// conversation rather than in front of the page
			// (keystone/butler/tools_home.go). All four are reads of
			// something KEYSTONE owns — the commitment pass, the
			// household timeline rendered, the notices table, the node
			// agent — which is why they are wired here rather than
			// declared by a package: they work on a deployment with no
			// capabilities installed at all.
			//
			// Wired unconditionally, for the reason RecordReading is:
			// a reading a household has to switch on is a reading
			// nobody switches on. Each seam still reports its own
			// unavailability honestly, and an Owner can turn any of
			// the tools off per agent through the ordinary control.
			Upkeep: func(context.Context) ([]butler.CommitmentLine, error) {
				return commitmentStandings{engine: upkeepEngine}.Describe()
			},
			Activity:      apiSrv.HouseholdActivitySeam(),
			Notifications: apiSrv.HouseholdNotificationsSeam(),
			Machines:      apiSrv.HouseholdMachinesSeam(),
			// The chat door a meter reading may come through (ADR-W035
			// A3), over the same store and the same refusals as the HTTP
			// intake. Wired unconditionally: the tracker works with no
			// capabilities installed (A4), so its doors are not
			// something a household has to switch on.
			RecordReading: apiSrv.MeterReadingSeam(),
		}
		if len(actions.Tools()) > 0 {
			household.ActionResult = func(ctx context.Context, c butler.HouseholdActionCall) (butler.ToolOutcome, error) {
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
					// A person asked and is reading the answer. This is
					// the second axis ADR-W024 A5.1 adds, and it is set
					// HERE rather than inferred from the rule name,
					// because a surface asserting "somebody is present"
					// should have to say so.
					Attended: true,
				})
				if aerr != nil {
					return butler.ToolOutcome{}, aerr
				}
				return butler.ToolOutcome{Content: res.Summary, EffectPending: res.EffectPending, EffectConfirmed: res.EffectConfirmed, ProposalConfirmed: res.ProposalConfirmed}, nil
			}
			household.Actions = func(ctx context.Context, c butler.HouseholdActionCall) (string, error) {
				result, err := household.ActionResult(ctx, c)
				return result.Content, err
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

		// Named Butlers on the live conversational path (ADR-W031),
		// over the durable runner and the fenced ledger (ADR-W030 P3).
		//
		// Wired only while this process holds the coordinator lease.
		// Durable work writes steps, attempts and traces, and a second
		// process writing them under a lease it does not hold is the
		// racing-schedulers failure the lease exists to refuse. Without
		// the lease this deployment keeps serving every read path and
		// answers conversations on the legacy provider, which is
		// degraded but honest.
		if coordinating {
			ledger := butlerstore.New(db, lease)
			// Derived Task authority is wired explicitly rather than
			// defaulted, for the reason admission always was: "no
			// admission records exist yet" and "this person is not
			// admitted" must not be the same answer, so turning it on
			// is an act (ADR-W030 A2.1, design §10.1).
			//
			// Every leg a TASK is re-evaluated against: the Person,
			// the session and the Connection it was admitted under. A
			// subset is not a fraction of the rule; it is a rule with a
			// hole, and a task that recorded an origin the deployment
			// cannot check is refused rather than waved through.
			//
			// A revoked GRANT is not among them by design: it refuses
			// its tool in the resolver, per call. See the note on
			// Ledger's fields.
			ledger.DerivedAuthorityChecks()
			butlers := &butlerlive.Butlers{
				DB:     db,
				Ledger: ledger,
				Log:    log,
				// Every Butler is told the date and time in the
				// household's zone; see butlerlive.clockLine.
				Location: householdLoc,
				Zone:     householdZone,
				// The provider is built from the MODEL the Butler was
				// assigned, and from nothing else. There is no argument
				// here that could come from a message: this function is
				// handed a Butler and reads what its assignment says.
				//
				// ProviderModel, not Model. Model is the assignment's
				// own name — which for anything configured from the
				// provider's list is the same string, and is not for a
				// row named before that. Sending the name to Ollama
				// asks it for a model called "ollama-cloud" and gets a
				// 404 naming something the person never typed.
				Providers: butlerProviderFor,
				Gateways: butlerlive.GatewayFunc(func(b runtimedb.Butler, t butlerlive.Turn) (butler.Toolset, error) {
					switch b.Surface {
					case runtimedb.ButlerSurfaceFamily:
						if household.Events == nil && household.Actions == nil {
							return nil, fmt.Errorf("this deployment has no household surface wired")
						}
						// The audience decides which CAPABILITIES the
						// session has, not merely what its prompt asks
						// for (ADR-W026 A7) — so a room gets the
						// narrowed gateway, built fresh and bound to the
						// member.
						// Memory is bound to THIS session: the
						// reader and the Butler are closed over in
						// butlerstore, from the turn and the Butler
						// record, so nothing a model types can name a
						// different one. That is what makes recall
						// safe to offer as a tool at all, and it is
						// what decides who owns anything remember
						// keeps.
						mine := household
						mine.ProposeRoutine = apiSrv.RoutineProposalSeam(b.Name)
						mine.Memory = butlerstore.MemorySeams(db, b.Name, t.Member, t.MemoryDestination(), log)
						if household.Actions != nil {
							mine.Actions = func(ctx context.Context, c butler.HouseholdActionCall) (string, error) {
								ctx = runtimedb.WithSecretUseContext(ctx, db.BindMemoryRead(t.Member, b.Name, t.MemoryDestination()))
								return household.Actions(ctx, c)
							}
						}
						if household.ActionResult != nil {
							mine.ActionResult = func(ctx context.Context, c butler.HouseholdActionCall) (butler.ToolOutcome, error) {
								ctx = runtimedb.WithSecretUseContext(ctx, db.BindMemoryRead(t.Member, b.Name, t.MemoryDestination()))
								return household.ActionResult(ctx, c)
							}
						}

						if t.Group {
							return butler.NewHouseholdGroupGateway(t.Member, mine), nil
						}
						return butler.NewHouseholdGateway(t.Member, mine), nil
					case runtimedb.ButlerSurfaceAdmin:
						// A copy per session, because butlerlive sets
						// this Butler's authorizer and task on it. A
						// shared instance would leak one Butler's scope
						// into another's session, which is exactly the
						// isolation A4 is about. The tool map is built
						// once and never written, so sharing it is safe.
						perSession := *bgw
						return &perSession, nil
					default:
						return nil, fmt.Errorf("unknown butler surface %q", b.Surface)
					}
				}),
			}
			// The stalled-task notice goes to the direct surface the
			// task's owner was already talking on (ADR-W030 A4/P4).
			butlers.Notifier = &butlerstore.Notifier{Ledger: ledger}
			apiSrv.Butlers = butlers

			named, lerr := db.ListButlers()
			switch {
			case lerr != nil:
				log.Error("listing butlers", "err", lerr)
			case len(named) == 0:
				log.Info("no butlers created yet; conversations answer on the legacy provider until one is (ADR-W031)")
			default:
				for _, nb := range named {
					log.Info("butler on the live path", "butler", nb.Name, "surface", nb.Surface,
						"model", nb.Model, "kind", nb.ModelKind, "answers_as", nb.ProviderModel,
						"readiness", nb.Readiness)
				}
			}
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
	if coordinating {
		// Reviewed Household routines have their own version-aware cursor and
		// effect ledger. Only the process holding the execution lease scans
		// and dispatches them; the domestic gateway rechecks that lease at
		// every external-effect boundary.
		auto.ReviewedActions = actions
		auto.ReviewedEscalator = func(ctx context.Context, agent string, req automation.EscalationRequest) (string, error) {
			b, err := db.GetButler(agent)
			if err != nil || !b.Reachable() {
				return "", fmt.Errorf("assigned Agent %s is unavailable", agent)
			}
			p, err := butlerProviderFor(b)
			if err != nil || p == nil {
				return "", fmt.Errorf("assigned Agent %s has no available provider: %v", agent, err)
			}
			facts := make([]butler.Fact, 0, len(req.Facts))
			for _, f := range req.Facts {
				facts = append(facts, butler.Fact{Label: f.Label, Content: f.Content})
			}
			phraser := &butler.Phraser{Provider: p, Model: b.ProviderModel,
				Record: func(_ context.Context, providerName, prompt, reply string) error {
					_, err := db.InsertButlerSession(runtimedb.ButlerSession{
						Principal: butlerSessionPrincipal, ActingFor: automation.Source,
						Butler: b.Name, AgentID: b.Seq, Provider: providerName,
						Mode: runtimedb.ButlerModeFull, Surface: b.Surface,
						FinalText:      redact.String(reply),
						TranscriptJSON: redact.String(`{"kind":"reviewed-routine-escalation","prompt":` + strconv.Quote(prompt) + `}`),
					})
					return err
				}}
			return phraser.Phrase(ctx, butler.PhraseRequest{
				Rule: req.Rule, Instruction: req.Instruction, Fallback: req.Fallback, Facts: facts,
			})
		}
		go auto.RunReviewed(context.Background(), automationEvery)
	}

	// The household's commitments (ADR-W035 A15): once a day, work out
	// what has come due and record the changes on the household
	// timeline. Nothing here notifies anybody — an Automation the
	// household wrote decides who hears about a commitment.* fact, on
	// which surface, at what hour (A12).
	//
	// Beside the morning report rather than in cron for ADR-W011 A4's
	// reason, and at the same hour because a household reads its house
	// once: a checkup with a sixty-day lead and an oil change with a
	// three-hundred-mile lead are both indifferent to the minute.
	apiSrv.Upkeep = upkeepEngine
	upkeepPoke := make(chan struct{}, 1)
	apiSrv.OnMeterReading = func() {
		// A reading can change an answer, so the pass runs again rather
		// than waiting for tomorrow: somebody who has just typed in a
		// mileage is standing there wanting to know.
		select {
		case upkeepPoke <- struct{}{}:
		default:
		}
	}
	go upkeepEngine.Run(context.Background().Done(), upkeepPoke, nextMorning(time.Now()))

	if coordinating {
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				if err := db.CheckCoordinator(lease.Holder, lease.Epoch); err != nil {
					return
				}
				if err := db.QueueOwnerReviewNotifications(time.Now()); err != nil {
					log.Warn("Owner review notification held", "err", err)
				}
			}
		}()
	}

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
		publicHandler = withConsole(publicHandler, consoleDir, apiSrv.ConsoleUIReachableFrom)
		// Which of the two postures this is, said out loud. The gated
		// one refuses a browser that reaches this port directly, and
		// that refusal is the kind an operator cannot diagnose from the
		// outside — the page simply stops loading, with the proxy still
		// insisting they are signed in.
		// A snapshot of the posture at startup, and it can change under
		// discovery: the gate itself is evaluated per request, so the
		// behaviour is always current even when this line is not. Said
		// anyway, because it is the first thing anybody checks when the
		// Console stops loading.
		if len(apiSrv.TrustedIngressAddrs()) == 0 {
			log.Info("serving console", "dir", consoleDir,
				"reachable", "anyone who can reach this port; sign-in is a bearer token",
				"note", "this tightens to proxy-only once an identity proxy address is observed")
		} else {
			log.Info("serving console", "dir", consoleDir,
				"reachable", "the identity proxy only",
				"proxy", apiSrv.TrustedIngressAddrs(),
				"recovery", "keystone owner, on this host, if the proxy is broken")
		}
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
// It differs from the gateway in one way that matters. The two listen
// separately, and -api-listen's documented default is loopback — so a
// deployment can have routable gateway SANs while the API answers only
// on 127.0.0.1. Advertising a SAN in that case would prefill a
// `keystone_url` nothing is listening on, and the adapter installed
// with it could never check in. A loopback-bound API advertises
// nothing, which leaves the field blank for an operator to answer.
func advertisedAPIURL(hosts, listen string) string {
	if boundToLoopback(listen) {
		return ""
	}
	addr := advertisedAddr(hosts, listen)
	if addr == "" {
		return ""
	}
	return "http://" + addr
}

// boundToLoopback reports whether a listener answers only on loopback.
// A wildcard bind (0.0.0.0, ::, or an empty host) answers everywhere
// and is not loopback-only.
func boundToLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	switch host {
	case "", "0.0.0.0", "::":
		return false
	case "localhost":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// accessGroupsByCapability is the one fact the reconcile loop needs from
// each package: which household groups the service is for (ADR-W033
// design §4, slice 8).
//
// Keyed on the CAPABILITY, because that is what an entitlement is about
// — a household that swaps one photos profile for another has not
// changed who may see the photos. A catalog conformance test asserts
// every profile of a capability declares the same audience, so there is
// one right answer here rather than a choice between profiles.
//
// A nil catalog yields a nil map, and seeding then does nothing: a
// deployment with no catalog has no manifests to read an audience from.
func accessGroupsByCapability(cat *catalog.Catalog) map[string][]string {
	if cat == nil {
		return nil
	}
	out := map[string][]string{}
	for _, pkg := range cat.List() {
		if c := pkg.Metadata.Capability; c != "" && len(pkg.AccessGroups) > 0 {
			out[c] = pkg.AccessGroups
		}
	}
	return out
}
