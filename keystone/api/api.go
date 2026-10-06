// Package api is Keystone's public plane (ADR-W007 A1/A5): the ONE HTTP+
// JSON API consumed by Console, `wild`, and the Butler — ground rule 3, no
// private UI-only or AI-only endpoints. Every request runs the fixed
// middleware chain authn → RBAC → rate limit → handler → audit, and every
// response body passes through the redacting encoder; handlers cannot opt
// out of either.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/householdtz"
	"github.com/chickenrat17/wildstone-homelab/keystone/mailreview"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/research"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/speech"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
	"github.com/chickenrat17/wildstone-homelab/keystone/upkeep"
)

// Server serves the public plane. All fields are required except Log and
// OnApproved.
type Server struct {
	artifactCursorMu sync.Mutex
	artifactCursors  map[string]artifactCursor
	// Test seam only; production reloads settings and verifies confined bytes.
	artifactReadBytes func(context.Context, runtimedb.Workspace, runtimedb.WorkspaceRevision) ([]byte, error)
	// ResearchTest tests stored, validated configuration using a bounded context.
	// The adapter must resolve service/secret references and check their current
	// revisions. Nil is deliberately unavailable. Production wiring also requires a verified
	// release pin and fresh Wardstone dependency inspection.
	ResearchTest func(context.Context, research.Provider) error
	// Serializes bounded provider probes against infrastructure approval writes.
	researchServiceMu sync.RWMutex

	SecretVault *runtimedb.SecretVault
	// MailReview is the private review of mail event candidates (epic
	// #326 S06). nil leaves the candidate routes reading only.
	MailReview *mailreview.Engine
	// PairingHTTP, when set, is the client pairing calls PantryOS with
	// (tests); its redirect policy is always replaced with "never follow".
	PairingHTTP *http.Client
	// PantryGroceries is PantryOS's grocery list, which the household's
	// old one moves to (#387). nil answers the move with 503.
	PantryGroceries PantryGroceries

	Principals *principal.Store
	DB         *runtimedb.DB
	// Catalog is what this control plane can install (ADR-W021 A3).
	// nil leaves the package and capability endpoints answering 503;
	// every other route is unaffected, the same posture Agents takes.
	Catalog *catalog.Catalog
	State   *statefile.Store
	Log     *slog.Logger

	// WallpaperDir holds the pictures that ship with the product, one
	// per shipped theme, named <slug>.<ext> (see wallpapers.go for why
	// they are a directory rather than embedded bytes). Empty means the
	// shipped themes arrive without pictures and only a household's own
	// uploads are drawn, which is what a deployment that installed no
	// wallpaper artifact gets.
	WallpaperDir string

	// Speech transcribes voice messages and dashboard dictation
	// (ADR-W042). nil, or a Router whose Mode is off, means voice is
	// refused with a "please type" answer.
	Speech *speech.Router
	// NodeID is the Wardstone node Keystone runs on. An installed speech
	// engine is reachable only there (#408). Empty means unknown, and
	// nothing is refused on a guess.
	NodeID string

	// trustedIngress is the exact set of remote addresses the identity
	// proxy speaks from (ADR-W030 A8.1). It gates the household portal
	// and the Console — both of which trust
	// an identity header, and a header anyone can set is not an
	// authentication. Empty means those paths are unavailable rather
	// than open: an unconfigured trust boundary must fail closed.
	//
	// Atomic, and unexported so it cannot be assigned past that, because
	// it is no longer written once at startup. The reconciler refreshes
	// it whenever the proxy's container address changes — Docker is free
	// to hand a recreated container a different address, and a value
	// fixed at install time would silently stop matching. Request
	// goroutines read this on every call, so a plain field written by
	// the reconcile loop would be a data race on the house's trust
	// boundary.
	trustedIngress atomic.Pointer[[]string]

	// ProbeModel asks one model whether it can actually operate a tool,
	// and is how a model earns the authority to do things (ADR-W017 A4).
	//
	// It is a seam rather than an implementation because building a
	// provider means knowing about API keys in the environment, which
	// cmd/keystone already knows and this package deliberately does not.
	//
	// nil leaves a newly assigned model UNPROBED rather than assuming
	// it works: unprobed answers questions and carries no authority to
	// use tools that do things, which is the safe reading of "nobody
	// has checked".
	ProbeModel func(ctx context.Context, provider, model, endpoint string) (readiness, detail string)

	// OnApproved, when set, is called (non-blocking contract: keep it
	// cheap) after a plan approval commits — cmd/keystone wires it to the
	// reconcile service's Poke so convergence starts immediately instead
	// of at the next interval (ADR-W008 R2a).
	OnApproved func()

	// Policy validates every proposed target tree (ADR-W009 A1). New
	// installs a no-exemptions engine; there is deliberately no way to
	// run the API without one — the standing invariants cannot be turned
	// off, only exempted per-resource via the engine's exemption list.
	Policy *policy.Engine

	// Agents dispatches Wardstone ops for the secrets endpoints
	// (ADR-W012 W2) — cmd/keystone wires the gateway applier. nil leaves
	// those endpoints answering 503; every other route is unaffected.
	Agents reconcile.Applier

	// LiveNodes reports the agent sessions connected right now (#30) —
	// cmd/keystone adapts gateway.LiveNodes. nil renders every node as
	// disconnected rather than failing: the durable inventory still
	// answers what nodes exist.
	LiveNodes func() []LiveNode

	// MintJoin mints a single-use node join token (ADR-W014 A3) —
	// cmd/keystone wires gateway.MintJoinToken. nil leaves the pairing
	// endpoint answering 503.
	MintJoin func(ttl time.Duration) (string, error)
	// CAFingerprint is the gateway CA's hex sha256, returned alongside
	// every minted join token so installs pin the join (ADR-W003 W3).
	CAFingerprint string

	// GatewayAddr is the host:port a node should dial to reach the agent
	// gateway, used to fill in --keystone in the emitted pairing command
	// (ADR-W014 A3). Empty leaves a placeholder and says so in the
	// response notes — a wrong address is worse than a visible blank.
	GatewayAddr string

	// APIURL is this API's base URL as something running ON A NODE
	// reaches it — the routable -hosts SAN the pairing command already
	// advertises, with the API's own port. It fills the `keystone_url`
	// param source, so a package that needs to tell a container where
	// the control plane is does not have to ask an operator to work out
	// that 127.0.0.1 would mean the container itself. Empty when no
	// routable SAN exists, which leaves the field blank rather than
	// suggesting an address nothing can reach.
	APIURL string

	// ManagedPrefix is where ConfigFiles are rendered on a node, wired
	// from -managed-prefix. Needed here because a secret can be read
	// from a managed file rather than from a container's environment,
	// and propagating a new value means rewriting that file.
	ManagedPrefix string

	// HouseholdTimezone is the IANA zone this deployment keeps, wired
	// from -household-timezone. It is offered as the answer to any
	// param declaring `source: timezone`, so a household states its
	// clock once instead of on every install form.
	HouseholdTimezone string

	// CalendarCollectionURL retains the original Household collection and seeds
	// the trusted CalDAV origin for declared shared calendars. Existing events
	// and mobile profiles keep their addresses; declarations carry no URLs.

	CalendarCollectionURL string

	// CalendarEvents reads the SHARED household calendar for the home
	// page's card. nil leaves the card offering the hand-off and saying
	// it cannot show what is on — which is honest, and is the state of
	// a deployment that installed the capability and has not pointed
	// Keystone at it.
	//
	// A seam rather than a client, because this package has no calendar
	// credential and should not grow one: cmd/keystone owns it and hands
	// down a function that returns three days of one collection.
	CalendarEvents func(from, to time.Time) ([]CalendarCardEvent, error)

	// CalendarBook backs the household's own calendar page (/calendar/):
	// the same shared collection, read AND written. nil leaves the page
	// saying the calendar has no sign-in yet, the card's state too.
	CalendarBook       CalendarBook
	CalendarBookFor    func(collectionURL string) CalendarBook
	calendarBookingsMu sync.Mutex
	// pantryPairingMu serializes pairing and unpairing PantryOS: each mints,
	// exchanges and revokes publisher tokens, and two at once could revoke
	// each other's new token (ADR-W044 A6).
	pantryPairingMu sync.Mutex

	// Butlers serves conversations as named specialists (ADR-W031) —
	// cmd/keystone wires it over butlerlive, butlerstore and the
	// durable runner. nil leaves every conversation on the single
	// -butler-provider path, which is what a deployment that has
	// created no Butlers keeps running.
	Butlers ButlerConversations

	// AgentGateway is the Agent API's Wildstone side (ADR-W039 §3.4;
	// ADR-W040): the same agentapi.Client a native runner holds
	// in-process (keystone/agentgateway.InProcess), reached here so an
	// external runtime gets the identical behaviour over HTTP —
	// cmd/keystone wires the same instance it hands butlerlive.Butlers,
	// which is what Phase 2c's byte-identical evidence rests on. nil
	// leaves every /api/v1/agent/... route answering 503.
	AgentGateway agentapi.Client

	// Ledger is the durable task ledger (ADR-W030 A4) — cmd/keystone
	// wires the same *butlerstore.Ledger the Agent Gateway and the
	// native runner share. It is used here for exactly one thing:
	// fenceRuntimeTasks withdraws a revoked runtime's live task handles
	// and fences their in-flight attempts (§3.7). nil leaves revocation
	// disabling the credential without a gateway-served task to fence,
	// which is every deployment before Phase 2c's routes are reachable.
	Ledger *butlerstore.Ledger

	// CoordinatorHolder is THIS process's identity for the runtime
	// database's single-coordinator lease (ADR-W030 A11) — cmd/keystone
	// wires it. Empty leaves the coordinator routes answering 503: a
	// process that cannot name itself must not be able to promote
	// itself, because the id it promoted to would be one nothing can
	// re-acquire on the next restart.
	CoordinatorHolder string

	// ButlerHost hosts the Butler ask endpoint (ADR-W017 A2/A4) —
	// cmd/keystone wires it from -butler-provider. nil leaves ask
	// answering 503; session history stays readable.
	ButlerHost *ButlerHost
	// ToolInventory is what a class's tools are, for the purpose of
	// §6.4's default assignment set. nil means ask the gateways, which
	// is what a running deployment does.
	//
	// A seam rather than a direct call because "except opt-in tools" is
	// only testable against an opt-in tool, and no shipped tool sets the
	// flag. The alternative was marking a real one opt-in to make a test
	// pass, which would change what every household's Agents are given.
	ToolInventory func(class string, readOnly bool) []butler.CapabilityTool
	// ApplyButlerConfig rebuilds the provider for a new choice and
	// returns the probe's verdict for THAT provider and model (ADR-W017
	// A9). cmd/keystone wires it; nil leaves the config endpoints
	// answering 503 rather than storing a choice nothing will honour.
	ApplyButlerConfig func(context.Context, runtimedb.ButlerConfig) (runtimedb.ButlerSessionMode, string, error)

	// HouseholdLoc is the IANA zone the household keeps (-household-
	// timezone). "Today" means midnight THERE, not midnight wherever
	// the control plane's process happens to think it is. nil falls
	// back to the process's local zone.
	HouseholdLoc *time.Location
	// HouseholdZone, when set, is the household's live timezone and wins
	// over HouseholdLoc: an Owner can change it (directly, or by
	// approving an Agent's proposal) and the change applies with no
	// restart.
	HouseholdZone *householdtz.Zone
	// MemberScoped reports whether a household event type is about a
	// PERSON rather than about the house (ADR-W026 A7). Used to withhold
	// one member's movements from a room the whole family is in; nil
	// withholds every attributed fact but the asker's, which is the
	// safe direction.
	MemberScoped func(eventType string) bool

	// Automation is the household rule engine (ADR-W024 H3), used here
	// only for side-effect-free replay; cmd/keystone runs its loop. New
	// wires one over the same DB and Statefile.
	Automation *automation.Engine
	// Upkeep, when set, answers where the household's commitments stand.
	// An interface rather than the engine so the api package does not
	// depend on the pass's timer, and so a test can hand it fixed
	// answers.
	Upkeep interface {
		Evaluate() ([]upkeep.Standing, error)
	}
	// OnMeterReading, when set, is called after a reading is recorded, so
	// the commitment pass can run again rather than leaving somebody who
	// has just typed in a mileage to wait until tomorrow to be told what
	// it meant.
	OnMeterReading func()
	// OnHouseholdEvent, when set, is called after a new household event
	// is recorded (not after a duplicate) — cmd/keystone wires it to the
	// engine's Poke so a rule is evaluated when its trigger lands, not
	// at the next interval.
	OnHouseholdEvent func()

	// BackupRun / BackupRestore reach the backup engine (ADR-W015 A3) —
	// cmd/keystone wires Engine.RunNow and Engine.RestoreAlongside. nil
	// leaves the backup endpoints answering 503.
	BackupRun     func(ctx context.Context, plan string) error
	BackupRestore func(ctx context.Context, plan string) error
	// BackupRestoreOffsite restores from a plan's off-site copy
	// (ADR-W045 A3); POST .../restore?from=offsite.
	BackupRestoreOffsite func(ctx context.Context, plan string) error
	// BackupTargetTest performs a read-only bucket stat with the target's
	// node-scoped credentials.
	BackupTargetTest func(ctx context.Context, target string) error
	// BackupEstimate performs read-only source/repository measurements for
	// plan authoring. The string is a human-readable disk-check result.
	BackupEstimate func(ctx context.Context, plan resource.Resource) (sourceBytes uint64, diskCheck string, err error)
	// BackupPreflight checks a proposal's BackupPlans fit their node's
	// disk before a plan is recorded (ADR-W045 A3.1); nil skips it.
	BackupPreflight func(puts []resource.Resource) error

	// ConsoleEntryWindow and ConsoleStepUpWindow are how old a Console
	// authentication may be to stay in the Console, and to perform a
	// consequential operation (ADR-W033 A4.1). Zero means the defaults,
	// 12 hours and 15 minutes.
	ConsoleEntryWindow  time.Duration
	ConsoleStepUpWindow time.Duration
	// lateAnswerPoll is how often a late answer rechecks whether its
	// turn has been recorded (DeliverLateAnswer). Zero is a second.
	lateAnswerPoll time.Duration
	// ConsoleSignInHTTP is the client used to reach the sign-in provider;
	// nil means a default with a timeout. Tests point it at a fake.
	ConsoleSignInHTTP *http.Client
	// Now is the clock the Console's freshness checks read; nil means
	// time.Now. Tests move it.
	Now func() time.Time

	console consoleAuth

	limiter *limiter
	// hostFacts caches the machines card's live readings, so a house
	// full of tablets does not cost one agent round trip each
	// (handlers_portal_machines.go).
	hostFacts machineStatsCache
}

// ButlerConversations is the named-Butler seam (ADR-W031). It is an
// interface rather than the concrete type so that a deployment with no
// durable wiring — and a test that wants one turn without a runtime
// database — can be the nil case honestly rather than by constructing
// something half-real.
type ButlerConversations interface {
	// Any reports whether this deployment has created any Butler. It is
	// the only question whose answer may route a conversation onto the
	// legacy provider, and it reads deployment state rather than the
	// message.
	Any() (bool, error)
	// Serve runs one turn as whichever Butler the turn's address or
	// name resolves to.
	Serve(ctx context.Context, t butlerlive.Turn) (butlerlive.Answer, error)
	// Sole names the only Butler a person may talk to on a surface, for
	// the Console path where there is no bot to address. It refuses
	// rather than picks when there is more than one.
	Sole(surface runtimedb.ButlerSurface, member string, asOwner bool) (runtimedb.Butler, error)
}

// New wires a Server. The rate limits are per ADR-W007 A5: a generous
// per-principal bucket, one strict shared bucket for everything that
// fails authn.
func New(p *principal.Store, db *runtimedb.DB, state *statefile.Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		Principals: p,
		DB:         db,
		State:      state,
		Log:        log,
		Policy:     policy.NewEngine(nil),
		Automation: &automation.Engine{DB: db, State: state, Log: log},
		limiter:    newLimiter(),
	}
}

// Handler returns the routed public-plane handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerArtifactRoutes(mux)

	// healthz is the single unauthenticated route: static, no state read,
	// nothing to redact, still rate-limited via the anonymous bucket.
	mux.HandleFunc("GET /api/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(anonBucket) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"ok":true}`)
	})

	mux.Handle("GET /api/v1/whoami", s.handle(principal.VerbRead, s.whoami))
	// Console sign-in (ADR-W033 A4.1). Not behind s.handle: these are
	// how a forwarded person GETS a Console session, so they cannot
	// require one. Each checks the trusted-ingress peer itself.
	mux.HandleFunc("GET "+consoleSigninPath, s.consoleSignin)
	mux.HandleFunc("GET "+consoleCallbackPath, s.consoleCallback)
	mux.HandleFunc("POST /api/v1/console/signout", s.consoleSignout)
	mux.Handle("GET /api/v1/resources", s.handle(principal.VerbRead, s.listResources))
	mux.Handle("GET /api/v1/resources/{kind}/{name}", s.handle(principal.VerbRead, s.getResource))
	mux.Handle("GET /api/v1/observed", s.handle(principal.VerbRead, s.listObserved))
	mux.Handle("GET /api/v1/events", s.handle(principal.VerbRead, s.listEvents))

	mux.Handle("GET /api/v1/plans", s.handle(principal.VerbRead, s.listPlans))
	mux.Handle("GET /api/v1/plans/{id}", s.handle(principal.VerbRead, s.getPlan))
	mux.Handle("POST /api/v1/plans", s.handle(principal.VerbPropose, s.proposePlan))

	// Capabilities (ADR-W021 A1): expansion lives here, not in the CLI,
	// so the Console and wild reach it through the same one API.
	mux.Handle("GET /api/v1/packages", s.handle(principal.VerbRead, s.listPackages))
	mux.Handle("POST /api/v1/capabilities", s.handle(principal.VerbPropose, s.enableCapability))
	mux.Handle("POST /api/v1/capabilities/disable", s.handle(principal.VerbPropose, s.disableCapability))
	mux.Handle("POST /api/v1/plans/{id}/approve", s.handle(principal.VerbApprove, s.consequential(s.approvePlan)))
	mux.Handle("POST /api/v1/plans/{id}/reject", s.handle(principal.VerbApprove, s.consequential(s.rejectPlan)))

	mux.Handle("GET /api/v1/audit", s.handle(principal.VerbManage, s.listAudit))
	// Admin-only: the pending one-time code is a credential in flight.
	mux.Handle("GET /api/v1/nodes/{node}/identity/code", s.handle(principal.VerbManage, s.identityCode))
	mux.Handle("GET /api/v1/principals", s.handle(principal.VerbManage, s.listPrincipals))
	mux.Handle("POST /api/v1/principals", s.handle(principal.VerbManage, s.consequential(s.createPrincipal)))
	mux.Handle("POST /api/v1/principals/{name}/disable", s.handle(principal.VerbManage, s.consequential(s.setPrincipalDisabled(true))))
	mux.Handle("POST /api/v1/principals/{name}/enable", s.handle(principal.VerbManage, s.setPrincipalDisabled(false)))
	mux.Handle("POST /api/v1/principals/{name}/tokens", s.handle(principal.VerbManage, s.consequential(s.mintToken)))

	// Node inventory (#30): the one route that does not need a node name
	// up front — everything else under /nodes/{node} does.
	mux.Handle("GET /api/v1/nodes", s.handle(principal.VerbRead, s.listNodes))

	// Pairing (ADR-W014 A3): admin mints a single-use join token + CA
	// fingerprint for a node install.
	mux.Handle("POST /api/v1/join-tokens", s.handle(principal.VerbManage, s.consequential(s.mintJoinToken)))

	// Household/Personal secrets (ADR-W033 D1–D5) are separate from the
	// write-only node credential endpoints below (ADR-W012 W2).
	mux.Handle("GET /api/v1/household/secrets", s.handle(principal.VerbManage, s.householdSecretConsole))
	mux.Handle("POST /api/v1/household/secrets", s.handle(principal.VerbManage, s.consequential(s.householdSecretConsole)))
	mux.Handle("POST /api/v1/household/secrets/{id}/{operation}", s.handle(principal.VerbManage, s.consequential(s.householdSecretConsole)))
	mux.Handle("GET /api/v1/home/secrets", s.portal(surfaceHome, s.householdSecretHome))
	mux.Handle("POST /api/v1/home/secrets", s.portal(surfaceHome, s.householdSecretHome))
	mux.Handle("POST /api/v1/home/secrets/{id}/{operation}", s.portal(surfaceHome, s.householdSecretHome))
	mux.Handle("GET /api/v1/nodes/{node}/secrets", s.handle(principal.VerbManage, s.listNodeSecrets))
	// The install form's folder picker (ADR-W021 A5). Read-only, confined
	// to the prefixes a bind mount may name.
	mux.Handle("GET /api/v1/nodes/{node}/directories", s.handle(principal.VerbRead, s.listDirectories))
	mux.Handle("POST /api/v1/nodes/{node}/secrets/{name}", s.handle(principal.VerbManage, s.setSecret))
	mux.Handle("POST /api/v1/nodes/{node}/secrets/{name}/rotate", s.handle(principal.VerbManage, s.consequential(s.rotateSecret)))
	mux.Handle("POST /api/v1/nodes/{node}/recovery/secrets", s.handle(principal.VerbManage, s.consequential(s.importNodeRecovery)))

	// Onboarding validation probes (ADR-W016 A2, E10-U2): dial-or-read
	// only on the agent side, so the whole family carries read.
	mux.Handle("GET /api/v1/nodes/{node}/probes/blockdevices", s.handle(principal.VerbRead, s.probeBlockDevices))
	mux.Handle("GET /api/v1/nodes/{node}/probes/ports", s.handle(principal.VerbRead, s.probePorts))
	mux.Handle("GET /api/v1/nodes/{node}/probes/http", s.handle(principal.VerbRead, s.probeHttp))
	mux.Handle("GET /api/v1/nodes/{node}/probes/nfs", s.handle(principal.VerbRead, s.probeDial("nfs")))
	mux.Handle("GET /api/v1/nodes/{node}/probes/smb", s.handle(principal.VerbRead, s.probeDial("smb")))
	mux.Handle("GET /api/v1/nodes/{node}/probes/radios", s.handle(principal.VerbRead, s.probeRadios))
	mux.Handle("GET /api/v1/nodes/{node}/probes/home-automation", s.handle(principal.VerbRead, s.probeHomeAutomation))
	// The home-automation aggregate (#446): a read joining the probes
	// above with catalog, desired/observed state, plans and backups.
	mux.Handle("GET /api/v1/home-automation", s.handle(principal.VerbRead, s.homeAutomationStatus))
	// Its orchestration (#447): the next safe package step for a
	// household's goals, previewed or filed as ONE ordinary plan through
	// the capability pipeline. Never approves, never waits.
	mux.Handle("POST /api/v1/home-automation/preview", s.handle(principal.VerbPropose, s.homeAutomationPreview))
	mux.Handle("POST /api/v1/home-automation/propose", s.handle(principal.VerbPropose, s.homeAutomationPropose))
	// What each installed part keeps where (#451): its data and backup
	// class, what a radio swap preserves, what removal stops and keeps.
	mux.Handle("GET /api/v1/home-automation/continuity", s.handle(principal.VerbRead, s.homeAutomationContinuity))

	// Container logs (ADR-W015 A2): read verb; the response passes the
	// redacting encoder like everything else.
	mux.Handle("GET /api/v1/nodes/{node}/containers/{name}/logs", s.handle(principal.VerbRead, s.containerLogs))

	// Backups (ADR-W015 A2): ledger reads for everyone with read; run and
	// restore are human-initiated executions — approve verb, so the
	// Butler's propose-only role cannot trigger them.
	mux.Handle("GET /api/v1/backups", s.handle(principal.VerbRead, s.listBackups))
	mux.Handle("GET /api/v1/backups/targets", s.handle(principal.VerbRead, s.listBackupTargets))
	mux.Handle("POST /api/v1/backups/targets", s.handle(principal.VerbPropose, s.proposeBackupTarget))
	mux.Handle("POST /api/v1/backups/targets/{target}/test", s.handle(principal.VerbRead, s.testBackupTarget))
	mux.Handle("PUT /api/v1/backups/plans/{plan}", s.handle(principal.VerbPropose, s.proposeBackupPlan))
	mux.Handle("POST /api/v1/backups/plans/{plan}/estimate", s.handle(principal.VerbRead, s.estimateBackupPlan))
	mux.Handle("POST /api/v1/backups/{plan}/run", s.handle(principal.VerbApprove, s.runBackup))
	mux.Handle("POST /api/v1/backups/{plan}/restore", s.handle(principal.VerbApprove, s.consequential(s.restoreBackup)))

	// Morning report (ADR-W017 A5): deterministic digest, read verb.
	mux.Handle("GET /api/v1/reports/morning", s.handle(principal.VerbRead, s.morningReport))

	// Household events (ADR-W024): the intake (emit — the publisher
	// role's only verb), the cursor (read), and publisher credentialing
	// (manage).
	mux.Handle("POST /api/v1/household/events", s.handle(principal.VerbEmit, s.emitHouseholdEvent))
	mux.Handle("GET /api/v1/household/events", s.handle(principal.VerbRead, s.listHouseholdEvents))
	mux.Handle("POST /api/v1/household/publishers/{capability}", s.handle(principal.VerbManage, s.consequential(s.mintPublisher)))
	// Pairing with PantryOS (ADR-W044 A6): the only way a PantryOS on
	// another machine receives its publisher token. No route returns
	// either secret.
	mux.Handle("GET /api/v1/pantry/pairing", s.handle(principal.VerbRead, s.pantryPairingStatus))
	mux.Handle("POST /api/v1/pantry/pairing", s.handle(principal.VerbManage, s.consequential(s.pairPantry)))
	mux.Handle("DELETE /api/v1/pantry/pairing", s.handle(principal.VerbManage, s.consequential(s.unpairPantry)))
	// Moving the household's old grocery list into PantryOS (#387):
	// offered, never automatic, and consequential because it removes the
	// old list once everything on it has arrived.
	mux.Handle("GET /api/v1/pantry/grocery-move", s.handle(principal.VerbRead, s.groceryMovePreview))
	mux.Handle("POST /api/v1/pantry/grocery-move", s.handle(principal.VerbManage, s.consequential(s.moveGroceries)))
	// Replay is a read: it evaluates and returns, writes nothing.
	// Meters and their readings (ADR-W035 K1). The reading is an
	// ordinary authenticated write rather than a Plan: it is evidence,
	// not authority, and an approval queue between a person and a
	// mileage is how a household stops recording mileages.
	//
	// VerbEmit because a reading is a household fact, the same verb and
	// the same reason as the event intake beside it. Known consequence:
	// RoleOperator does not hold emit, so an operator cannot record a
	// reading today — the same gap they already have on household
	// events. Widening the role is an RBAC decision of its own and is
	// deliberately not made here.
	mux.Handle("GET /api/v1/household/meters", s.handle(principal.VerbRead, s.listMeters))
	mux.Handle("GET /api/v1/household/meters/{name}/readings", s.handle(principal.VerbRead, s.listMeterReadings))
	mux.Handle("POST /api/v1/household/meters/{name}/readings", s.handle(principal.VerbEmit, s.recordMeterReading))
	mux.Handle("GET /api/v1/household/commitments", s.handle(principal.VerbRead, s.listCommitments))
	mux.Handle("GET /api/v1/household/commitments/{name}", s.handle(principal.VerbRead, s.commitmentHistory))
	// And managing them, which the Console could not do at all: every
	// write lived behind a portal session and homepage_users, so an
	// Owner could see a duplicate commitment and not remove it. The
	// same handlers the dashboard calls, through an adapter that
	// resolves the acting person and the desired state the portal
	// chain is handed for free (handlers_household_commitments.go).
	mux.Handle("POST /api/v1/household/commitments",
		s.handle(principal.VerbManage, s.consoleCommitmentCall(s.homeAddCommitment)))
	mux.Handle("PUT /api/v1/household/commitments/{name}",
		s.handle(principal.VerbManage, s.consoleCommitmentCall(s.homeEditCommitment)))
	mux.Handle("DELETE /api/v1/household/commitments/{name}",
		s.handle(principal.VerbManage, s.consoleCommitmentCall(s.homeRemoveCommitment)))
	mux.Handle("POST /api/v1/household/commitments/{name}/done",
		s.handle(principal.VerbManage, s.consoleCommitmentCall(s.answerCommitment("done"))))
	mux.Handle("POST /api/v1/household/commitments/{name}/waived",
		s.handle(principal.VerbManage, s.consoleCommitmentCall(s.answerCommitment("waived"))))
	mux.Handle("POST /api/v1/household/commitments/{name}/booked",
		s.handle(principal.VerbManage, s.consoleCommitmentCall(s.answerCommitment("booked"))))

	mux.Handle("POST /api/v1/household/automations/replay", s.handle(principal.VerbRead, s.replayAutomation))
	mux.Handle("GET /api/v1/household/automations/grammar", s.handle(principal.VerbRead, s.automationGrammar))

	// Household members (ADR-W020 A3, ADR-W023 A7): the household's
	// account list IS the LdapUser resources in the Statefile. Reading
	// the roster is a read; adding a person is a propose, because that
	// is literally what it does — the account exists once a human
	// approves the plan.
	mux.Handle("GET /api/v1/household/members", s.handle(principal.VerbRead, s.listHouseholdMembers))
	mux.Handle("POST /api/v1/household/members", s.handle(principal.VerbPropose, s.addHouseholdMember))
	// Editing somebody is a proposal like adding one, and for the same
	// reason: the account list belongs in state, in plans, in approval
	// and in audit.
	mux.Handle("PUT /api/v1/household/members/{username}", s.handle(principal.VerbPropose, s.updateHouseholdMember))
	mux.Handle("GET /api/v1/household/groups", s.handle(principal.VerbRead, s.listHouseholdGroups))
	// The household's OWN groups and what they reach (ADR-W033 design
	// §4, slice 8). Distinct from /household/groups above, which lists
	// the DIRECTORY groups packages declare: one is where a membership
	// is projected TO, the other is what the household decided.
	//
	// Manage rather than Propose: these change no node, so there is
	// nothing for a plan to apply. The reconcile loop projects them on
	// its next tick.
	mux.Handle("GET /api/v1/household/person-groups", s.handle(principal.VerbRead, s.listPersonGroups))
	mux.Handle("POST /api/v1/household/person-groups/{group}/members", s.handle(principal.VerbManage, s.setPersonGroup))
	mux.Handle("GET /api/v1/services/{capability}/access", s.handle(principal.VerbRead, s.listServiceAccess))
	mux.Handle("POST /api/v1/services/{capability}/access", s.handle(principal.VerbManage, s.setServiceAccess))

	// Butler (ADR-W017 A2): ask carries the read verb — conversing is not
	// a write; a session's proposals enter through the same submitProposal
	// pipeline as POST /plans, as principal "butler" acting-for the
	// caller, and only a human approve verb moves them further.
	mux.Handle("GET /api/v1/butler", s.handle(principal.VerbRead, s.getButlerStatus))
	// Admin-only: it reaches out to an operator-supplied address from
	// inside the control plane, which is not a read a viewer should be
	// able to make.
	mux.Handle("GET /api/v1/butler/models/offered", s.handle(principal.VerbManage, s.listOfferedModels))
	mux.Handle("GET /api/v1/butler/providers", s.handle(principal.VerbManage, s.listProviderKinds))
	mux.Handle("PUT /api/v1/butler/providers/{name}/key", s.handle(principal.VerbManage, s.setProviderKey))
	mux.Handle("DELETE /api/v1/butler/providers/{name}/key", s.handle(principal.VerbManage, s.removeProviderKey))
	mux.Handle("GET /api/v1/butler/config", s.handle(principal.VerbManage, s.getButlerConfig))
	mux.Handle("PUT /api/v1/butler/config", s.handle(principal.VerbManage, s.setButlerConfig))
	// The administrator's side of integrations. Every one needs manage.
	mux.Handle("GET /api/v1/integrations", s.handle(principal.VerbRead, s.listIntegrationConnections))
	mux.Handle("POST /api/v1/integrations", s.handle(principal.VerbManage, s.createIntegrationConnection))
	mux.Handle("POST /api/v1/integrations/{name}/widen", s.handle(principal.VerbManage, s.widenIntegrationConnection))
	mux.Handle("POST /api/v1/integrations/{name}/adapter", s.handle(principal.VerbManage, s.bindIntegrationAdapter))
	mux.Handle("POST /api/v1/integrations/{name}/capabilities", s.handle(principal.VerbManage, s.recordIntegrationCapability))
	mux.Handle("POST /api/v1/integrations/{name}/work", s.handle(principal.VerbManage, s.queueIntegrationWork))
	// Work items live at their own path rather than under a connection:
	// they are addressed by id, and nesting them would collide with the
	// connection sub-resources below.
	mux.Handle("GET /api/v1/integration-work/{seq}", s.handle(principal.VerbRead, s.getIntegrationWork))
	mux.Handle("GET /api/v1/integrations/{name}/placement", s.handle(principal.VerbRead, s.listIntegrationPlacements))
	mux.Handle("POST /api/v1/integrations/{name}/placement", s.handle(principal.VerbManage, s.addIntegrationPlacement))
	mux.Handle("POST /api/v1/integrations/{name}/placement/ready", s.handle(principal.VerbManage, s.markIntegrationPlacementReady))
	mux.Handle("POST /api/v1/integrations/{name}/placement/place", s.handle(principal.VerbManage, s.placeIntegrationConnection))
	mux.Handle("POST /api/v1/integrations/{name}/placement/remove", s.handle(principal.VerbManage, s.removeIntegrationPlacement))
	mux.Handle("POST /api/v1/integrations/{name}/placement/proof", s.handle(principal.VerbManage, s.proveIntegrationPlacementRemoval))
	mux.Handle("POST /api/v1/integrations/{name}/secrets", s.handle(principal.VerbManage, s.onboardIntegrationSecret))
	// Personal credentials and unattended-use authorization (epic #326
	// S02). The Console/CLI routes and the Homepage routes call the same
	// handlers; the store decides who may, and for onboarding, replacing,
	// inspecting and authorizing that is the connection's owner alone.
	// An Owner on the Console may revoke and erase, never more.
	mux.Handle("GET /api/v1/integrations/{name}/credential", s.handle(principal.VerbManage, s.consoleCredential))
	mux.Handle("POST /api/v1/integrations/{name}/credential", s.handle(principal.VerbManage, s.consoleSubmitCredential))
	mux.Handle("POST /api/v1/integrations/{name}/credential/revoke", s.handle(principal.VerbManage, s.consoleRevokeCredential))
	mux.Handle("POST /api/v1/integrations/{name}/credential/erase", s.handle(principal.VerbManage, s.consoleEraseCredential))
	mux.Handle("POST /api/v1/integrations/{name}/authorization", s.handle(principal.VerbManage, s.consoleAuthorizeBackground))
	mux.Handle("POST /api/v1/integrations/{name}/authorization/revoke", s.handle(principal.VerbManage, s.consoleRevokeBackground))
	mux.Handle("POST /api/v1/integrations/{name}/executable", s.handle(principal.VerbManage, s.setIntegrationExecutable))
	// Render the adapter placement as a Plan (epic #326): records the
	// executable, places node-bound adapter tokens, never approves.
	mux.Handle("POST /api/v1/integrations/{name}/adapter/render", s.handle(principal.VerbManage, s.renderIntegrationAdapter))
	// Watch this mailbox (epic #326 S07): the setup read model a screen
	// resumes from, and filing the subscription a Person's authorization
	// names. The Plan it files is an Owner's to approve.
	mux.Handle("GET /api/v1/mail/setup", s.handle(principal.VerbRead, s.consoleMailSetup))
	mux.Handle("POST /api/v1/integrations/{name}/subscription", s.handle(principal.VerbManage, s.consoleFileMailSubscription))
	mux.Handle("GET /api/v1/home/mail/setup", s.portal(surfaceHome, s.homeMailSetup))
	mux.Handle("POST /api/v1/home/integrations/{name}/subscription", s.portal(surfaceHome, s.homeFileMailSubscription))
	mux.Handle("GET /api/v1/home/integrations/{name}/credential", s.portal(surfaceHome, s.homeCredential))
	mux.Handle("POST /api/v1/home/integrations/{name}/credential", s.portal(surfaceHome, s.homeSubmitCredential))
	mux.Handle("POST /api/v1/home/integrations/{name}/credential/revoke", s.portal(surfaceHome, s.homeRevokeCredential))
	mux.Handle("POST /api/v1/home/integrations/{name}/authorization", s.portal(surfaceHome, s.homeAuthorizeBackground))
	mux.Handle("POST /api/v1/home/integrations/{name}/authorization/revoke", s.portal(surfaceHome, s.homeRevokeBackground))
	// Mail event candidates and their private review (epic #326 S06).
	// Person-scoped in the store; acting needs the Person's own identity
	// (never an Agent or a delegate) and manage, which no Agent holds.
	mux.Handle("GET /api/v1/mail/candidates", s.handle(principal.VerbRead, s.consoleMailCandidates))
	mux.Handle("GET /api/v1/mail/candidates/{id}", s.handle(principal.VerbRead, s.consoleMailCandidate))
	mux.Handle("POST /api/v1/mail/candidates/{id}/act", s.handle(principal.VerbManage, s.consoleMailAct))
	mux.Handle("POST /api/v1/mail/act", s.handle(principal.VerbManage, s.consoleMailAct))
	mux.Handle("GET /api/v1/home/mail/candidates", s.portal(surfaceHome, s.homeMailCandidates))
	mux.Handle("GET /api/v1/home/mail/candidates/{id}", s.portal(surfaceHome, s.homeMailCandidate))
	mux.Handle("POST /api/v1/home/mail/candidates/{id}/act", s.portal(surfaceHome, s.homeMailAct))
	mux.Handle("POST /api/v1/home/mail/act", s.portal(surfaceHome, s.homeMailAct))
	// The adapter's side: claim and answer, and nothing else.
	mux.Handle("POST /api/v1/integration-work/claim", s.handle(principal.VerbClaimIntegrationWork, s.claimIntegrationWork))
	mux.Handle("POST /api/v1/integration-work/result", s.handle(principal.VerbReportIntegrationResult, s.reportIntegrationResult))
	mux.Handle("GET /api/v1/butler/index/measurements", s.handle(principal.VerbRead, s.listIndexMeasurements))
	mux.Handle("POST /api/v1/butler/index/compact", s.handle(principal.VerbManage, s.compactButlerIndex))
	mux.Handle("PUT /api/v1/butler/memory/privacy", s.handle(principal.VerbRead, s.consequential(s.setMemoryPrivacy)))
	mux.Handle("GET /api/v1/butler/memory", s.handle(principal.VerbRead, s.retrieveButlerMemory))
	mux.Handle("GET /api/v1/butler/memory/export", s.handle(principal.VerbRead, s.exportButlerMemory))
	mux.Handle("POST /api/v1/butler/memory/correct", s.handle(principal.VerbRead, s.correctButlerMemory))
	mux.Handle("POST /api/v1/butler/memory/forget", s.handle(principal.VerbRead, s.forgetButlerMemory))
	mux.Handle("GET /api/v1/butler/memory/digest", s.handle(principal.VerbRead, s.listMemoryDigest))
	mux.Handle("POST /api/v1/household/routines/{name}/submit", s.handle(principal.VerbPropose, s.submitRoutinePlan))
	mux.Handle("GET /api/v1/owner-review/preference", s.handle(principal.VerbRead, s.ownerReviewPreference))
	mux.Handle("PUT /api/v1/owner-review/preference", s.handle(principal.VerbApprove, s.consequential(s.ownerReviewPreference)))
	mux.Handle("GET /api/v1/household/routine-audiences", s.handle(principal.VerbRead, s.routineAudiences))
	mux.Handle("GET /api/v1/owner-review", s.handle(principal.VerbRead, s.ownerReviewQueue))
	mux.Handle("GET /api/v1/household/routines", s.handle(principal.VerbRead, s.listHouseholdRoutines))
	mux.Handle("GET /api/v1/household/routines/{name}", s.handle(principal.VerbRead, s.getHouseholdRoutine))
	mux.Handle("POST /api/v1/household/routines", s.handle(principal.VerbPropose, s.createHouseholdRoutine))
	mux.Handle("POST /api/v1/household/routines/{name}/edit", s.handle(principal.VerbPropose, s.editHouseholdRoutine))
	mux.Handle("POST /api/v1/household/routines/{name}/state", s.handle(principal.VerbPropose, s.setHouseholdRoutineState))
	mux.Handle("POST /api/v1/household/routines/{name}/control", s.handle(principal.VerbApprove, s.controlReviewedRoutine))
	mux.Handle("POST /api/v1/household/routines/{name}/purge", s.handle(principal.VerbManage, s.consequential(s.purgeDeletedRoutine)))
	mux.Handle("GET /api/v1/household/execution-stop", s.handle(principal.VerbRead, s.getHouseholdExecutionStop))
	mux.Handle("POST /api/v1/household/execution-stop", s.handle(principal.VerbApprove, s.changeHouseholdExecutionStop))
	// Agent subscriptions, event and scheduled (epic #326 S03): one
	// surface for Console, CLI and Butler. Approval stays the Plan path.
	mux.Handle("GET /api/v1/agent-subscriptions", s.handle(principal.VerbRead, s.listAgentSubscriptions))
	mux.Handle("GET /api/v1/agent-subscriptions/{id}", s.handle(principal.VerbRead, s.getAgentSubscription))
	mux.Handle("PUT /api/v1/agent-subscriptions/{id}", s.handle(principal.VerbPropose, s.reviseAgentSubscription))
	mux.Handle("POST /api/v1/agent-subscriptions/{id}/control", s.handle(principal.VerbPropose, s.controlAgentSubscription))
	mux.Handle("POST /api/v1/home/agent-subscriptions/{id}/control", s.portal(surfaceHome, s.homeControlAgentSubscription))
	mux.Handle("GET /api/v1/butler/tools", s.handle(principal.VerbRead, s.listButlerToolControls))
	mux.Handle("PUT /api/v1/butler/tools", s.handle(principal.VerbManage, s.setButlerToolControl))
	mux.Handle("GET /api/v1/butler/grants", s.handle(principal.VerbRead, s.listButlerGrants))
	mux.Handle("POST /api/v1/butler/grants", s.handle(principal.VerbManage, s.grantButlerTool))
	mux.Handle("POST /api/v1/butler/grants/revoke", s.handle(principal.VerbManage, s.revokeButlerGrant))
	mux.Handle("GET /api/v1/butler/requests", s.handle(principal.VerbRead, s.listCapabilityRequests))
	// Asking needs only read: a request grants nothing, and requiring
	// manage to ask would mean only the people who can already enable a
	// tool could request one (ADR-W030 A7).
	mux.Handle("POST /api/v1/butler/requests", s.handle(principal.VerbRead, s.fileCapabilityRequest))
	mux.Handle("POST /api/v1/butler/requests/decide", s.handle(principal.VerbManage, s.decideCapabilityRequest))
	mux.Handle("GET /api/v1/butler/members", s.handle(principal.VerbRead, s.listButlerMembers))
	mux.Handle("POST /api/v1/butler/members", s.handle(principal.VerbManage, s.recordButlerMember))
	mux.Handle("POST /api/v1/butler/members/{member}/admit", s.handle(principal.VerbManage, s.admitButlerMember))
	mux.Handle("POST /api/v1/butler/members/{member}/demote", s.handle(principal.VerbManage, s.consequential(s.demoteButlerMember)))
	mux.Handle("POST /api/v1/butler/members/{member}/disable", s.handle(principal.VerbManage, s.consequential(s.disableButlerMember)))
	mux.Handle("POST /api/v1/butler/members/{member}/offboard", s.handle(principal.VerbManage, s.consequential(s.offboardButlerMember)))
	mux.Handle("POST /api/v1/butler/members/{member}/links", s.handle(principal.VerbManage, s.linkButlerIdentity))
	mux.Handle("POST /api/v1/butler/links/revoke", s.handle(principal.VerbManage, s.unlinkButlerIdentity))
	mux.Handle("POST /api/v1/butler/conversations", s.handle(principal.VerbRead, s.startButlerConversation))
	mux.Handle("POST /api/v1/butler/conversations/{id}/handoff", s.handle(principal.VerbRead, s.handoffButlerConversation))
	mux.Handle("GET /api/v1/butler/tasks", s.handle(principal.VerbRead, s.listButlerTasks))
	mux.Handle("GET /api/v1/butler/tasks/{id}", s.handle(principal.VerbRead, s.getButlerTask))
	mux.Handle("POST /api/v1/butler/tasks/{id}/feedback", s.handle(principal.VerbRead, s.addButlerTaskFeedback))
	mux.Handle("POST /api/v1/butler/tasks/{id}/evaluation", s.handle(principal.VerbManage, s.exportButlerEvaluation))
	mux.Handle("POST /api/v1/butler/tasks/{id}/debug", s.handle(principal.VerbManage, s.enableButlerDebugCapture))
	mux.Handle("POST /api/v1/butler/tasks/{id}/cancel", s.handle(principal.VerbManage, s.cancelButlerTask))
	mux.Handle("POST /api/v1/butler/tasks/{id}/work-review", s.handle(principal.VerbApprove, s.reviewConversationWork))
	mux.Handle("GET /api/v1/butler/evaluations", s.handle(principal.VerbRead, s.listButlerEvaluations))
	mux.Handle("GET /api/v1/butler/debug", s.handle(principal.VerbRead, s.listButlerDebugCaptures))
	mux.Handle("POST /api/v1/butler/members/{member}/offboarding", s.handle(principal.VerbManage, s.consequential(s.offboardMember)))
	mux.Handle("GET /api/v1/butler/members/{member}/offboarding", s.handle(principal.VerbRead, s.getOffboarding))
	mux.Handle("GET /api/v1/restore", s.handle(principal.VerbRead, s.getRestoreState))
	mux.Handle("POST /api/v1/restore/quarantine", s.handle(principal.VerbManage, s.consequential(s.enterRestoreQuarantine)))
	mux.Handle("POST /api/v1/restore/reconcile", s.handle(principal.VerbManage, s.consequential(s.reconcileRestore)))
	mux.Handle("GET /api/v1/notices", s.handle(principal.VerbRead, s.listNotices))
	mux.Handle("POST /api/v1/notices/acknowledge", s.handle(principal.VerbRead, s.acknowledgeNotice))
	mux.Handle("POST /api/v1/notices/answer", s.handle(principal.VerbRead, s.answerNotice))
	mux.Handle("POST /api/v1/notices/expire", s.handle(principal.VerbManage, s.expireNotices))
	mux.Handle("GET /api/v1/notices/preferences", s.handle(principal.VerbRead, s.getNoticePreferences))
	mux.Handle("PUT /api/v1/notices/preferences", s.handle(principal.VerbRead, s.setNoticePreferences))
	mux.Handle("PUT /api/v1/notices/kinds", s.handle(principal.VerbManage, s.setNoticeKind))
	// Appearance. Reading and setting are a READ verb because the theme
	// a person reads on is their own setting and changes nothing about
	// the deployment — the same reasoning as their notice preferences
	// above. Adding or removing a theme is VerbManage: it changes what
	// the whole household can choose from, and it is the one path by
	// which text somebody pasted reaches the control plane's database.
	mux.Handle("GET /api/v1/appearance", s.handle(principal.VerbRead, s.getAppearance))
	mux.Handle("PUT /api/v1/appearance", s.handle(principal.VerbRead, s.setAppearance))
	mux.Handle("POST /api/v1/appearance/themes", s.handle(principal.VerbManage, s.importTheme))
	// A whole theme at once, as the directory its authors publish: the
	// palette and its picture in one upload instead of a paste and a
	// second trip. Same verb as the paste, because it is the same act.
	mux.Handle("POST /api/v1/appearance/themes/package", s.handle(principal.VerbManage, s.importThemePackage))
	mux.Handle("DELETE /api/v1/appearance/themes/{name}", s.handle(principal.VerbManage, s.removeTheme))
	// The picture a theme brings. VerbManage for the same reason the
	// palette is: it changes what the whole household sees, and it is
	// the one path by which an image file somebody downloaded reaches
	// the control plane. The Console never DRAWS one — it has no read
	// route for the bytes at all — it only manages them.
	mux.Handle("PUT /api/v1/appearance/themes/{name}/background", s.handle(principal.VerbManage, s.themeBackgroundSet))
	mux.Handle("DELETE /api/v1/appearance/themes/{name}/background", s.handle(principal.VerbManage, s.themeBackgroundClear))
	// Research administration is never accessible to model/runtime principals.
	mux.Handle("GET /api/v1/research/providers", s.handle(principal.VerbManage, s.listResearchProviders))
	mux.Handle("POST /api/v1/research/providers", s.handle(principal.VerbManage, s.proposeResearchProvider))
	mux.Handle("GET /api/v1/research/providers/{provider}", s.handle(principal.VerbManage, s.getResearchProvider))
	mux.Handle("PATCH /api/v1/research/providers/{provider}", s.handle(principal.VerbManage, s.proposeResearchProvider))
	mux.Handle("DELETE /api/v1/research/providers/{provider}", s.handle(principal.VerbManage, s.proposeResearchProvider))
	mux.Handle("POST /api/v1/research/providers/{provider}/test", s.handle(principal.VerbManage, s.testResearchProvider))
	mux.Handle("PUT /api/v1/research/default-provider", s.handle(principal.VerbManage, s.selectResearchProvider))
	mux.Handle("GET /api/v1/agents/{agent}/research-provider", s.handle(principal.VerbManage, s.getResearchSelection))
	mux.Handle("PUT /api/v1/agents/{agent}/research-provider", s.handle(principal.VerbManage, s.selectResearchProvider))
	mux.Handle("GET /api/v1/butler/models", s.handle(principal.VerbRead, s.listModels))
	mux.Handle("PUT /api/v1/butler/models", s.handle(principal.VerbManage, s.upsertModel))

	// Butlers (ADR-W031). Reading the roster is a read; everything that
	// shapes a Butler — its model, its people, its connections, its
	// tools, where it answers — needs manage, because each is standing
	// configuration the Console holder owns and a Butler must never
	// reach (A8).
	mux.Handle("GET /api/v1/butlers", s.handle(principal.VerbRead, s.listButlers))
	mux.Handle("POST /api/v1/butlers", s.handle(principal.VerbManage, s.createButler))
	mux.Handle("GET /api/v1/butlers/{name}/scope", s.handle(principal.VerbRead, s.butlerScope))
	// Reading what a Butler may do is a read, and it is deliberately
	// its own route rather than a field on the scope: the answer comes
	// from the gateway and the authority resolver, not from the scope
	// tables, and folding it in would make one endpoint's cost depend
	// on wiring the other knows nothing about.
	mux.Handle("GET /api/v1/butlers/{name}/tools", s.handle(principal.VerbRead, s.butlerTools))
	mux.Handle("PUT /api/v1/butlers/{name}/model", s.handle(principal.VerbManage, s.setButlerModel))
	mux.Handle("PUT /api/v1/butlers/{name}/directive", s.handle(principal.VerbManage, s.setButlerDirective))
	mux.Handle("PUT /api/v1/butlers/{name}/access", s.handle(principal.VerbManage, s.setButlerAccess))
	mux.Handle("PUT /api/v1/butlers/{name}/connection", s.handle(principal.VerbManage, s.setButlerConnection))
	mux.Handle("PUT /api/v1/butlers/{name}/tool", s.handle(principal.VerbManage, s.setButlerTool))
	// Assigning adds to a governed Agent's reach where the switch above
	// only subtracts (#389). Opt-in tools need a fresh sign-in, checked
	// in the handler because only it knows which tool it was given.
	mux.Handle("PUT /api/v1/butlers/{name}/assignment", s.handle(principal.VerbManage, s.setButlerAssignment))
	mux.Handle("PUT /api/v1/butlers/{name}/address", s.handle(principal.VerbManage, s.setButlerAddress))
	mux.Handle("PUT /api/v1/butlers/{name}/class", s.handle(principal.VerbManage, s.consequential(s.setButlerAgentClass)))
	// The Agent's runtime record (ADR-W039 §3.3): which runtime, as
	// whom, how it is doing. Reading it is a read: the credential is a
	// reference, never a secret. Replacing the runtime, rotating its
	// credential and revoking it are manage — standing configuration
	// the Console holder owns — and consequential (ADR-W033 A4.1):
	// rotation mints a bearer credential, and the other two withdraw
	// one.
	mux.Handle("GET /api/v1/butlers/{name}/runtime", s.handle(principal.VerbRead, s.butlerRuntime))
	mux.Handle("PUT /api/v1/butlers/{name}/runtime", s.handle(principal.VerbManage, s.consequential(s.setButlerRuntime)))
	mux.Handle("POST /api/v1/butlers/{name}/runtime/credential", s.handle(principal.VerbManage, s.consequential(s.rotateRuntimeCredential)))
	mux.Handle("POST /api/v1/butlers/{name}/runtime/revoke", s.handle(principal.VerbManage, s.consequential(s.revokeButlerRuntime)))
	// Upgrading files a Plan and applies nothing, so it needs manage
	// and no fresh sign-in; approving the Plan is the consequential act.
	mux.Handle("POST /api/v1/butlers/{name}/runtime/upgrade", s.handle(principal.VerbManage, s.upgradeButlerRuntime))
	// The runtime inventory across every live Agent (design doc §9 H9,
	// #355): one read, same verb as the roster it augments.
	mux.Handle("GET /api/v1/runtimes", s.handle(principal.VerbRead, s.listAgentRuntimes))
	mux.Handle("GET /api/v1/butlers/{name}/memory-restrictions", s.handle(principal.VerbRead, s.agentMemoryRestrictions))
	mux.Handle("PUT /api/v1/butlers/{name}/memory-restrictions", s.handle(principal.VerbManage, s.consequential(s.setAgentMemoryRestriction)))
	mux.Handle("DELETE /api/v1/butlers/{name}", s.handle(principal.VerbManage, s.consequential(s.retireButler)))
	// Restore and duplicate are manage for the same reason retiring is:
	// each decides standing configuration. Impact is a read, and it is
	// the one a retire dialog needs BEFORE anybody has decided.
	mux.Handle("POST /api/v1/butlers/{name}/restore", s.handle(principal.VerbManage, s.restoreButler))
	mux.Handle("POST /api/v1/butlers/{name}/duplicate", s.handle(principal.VerbManage, s.duplicateButler))
	mux.Handle("GET /api/v1/butlers/{name}/impact", s.handle(principal.VerbRead, s.butlerImpact))
	// Talking is a read, like the other ask routes. What differs is the
	// ceiling: this one takes it from the named Butler rather than from
	// the door, which is the only correct answer for a caller who holds
	// both surfaces.
	mux.Handle("POST /api/v1/butlers/{name}/ask", s.handle(principal.VerbRead, s.askOneButler))
	// Dictation in the Console: sound to text for the text box, the same
	// verb and the same Butler resolution as asking (ADR-W042).
	mux.Handle("POST /api/v1/butlers/{name}/transcribe", s.handle(principal.VerbRead, s.transcribeForButler))
	mux.Handle("POST /api/v1/butlers/{name}/speak", s.handle(principal.VerbRead, s.speakForButler))
	mux.Handle("GET /api/v1/butlers/{name}/status", s.handle(principal.VerbRead, s.butlerStatus))
	mux.Handle("POST /api/v1/butler/models/{name}/readiness", s.handle(principal.VerbManage, s.recordModelReadiness))
	// Probing is manage because it writes the verdict that decides
	// whether a model may operate tools at all.
	mux.Handle("POST /api/v1/butler/models/{name}/probe", s.handle(principal.VerbManage, s.probeModel))
	mux.Handle("DELETE /api/v1/butler/models/{name}", s.handle(principal.VerbManage, s.deleteModel))
	mux.Handle("POST /api/v1/butler/models/{name}/enabled", s.handle(principal.VerbManage, s.setModelEnabled))
	mux.Handle("GET /api/v1/butler/settings", s.handle(principal.VerbManage, s.getButlerSettings))
	mux.Handle("PUT /api/v1/butler/settings", s.handle(principal.VerbManage, s.setButlerSettings))
	mux.Handle("POST /api/v1/butler/ask", s.handle(principal.VerbRead, s.askButler))
	mux.Handle("GET /api/v1/butler/sessions", s.handle(principal.VerbRead, s.listButlerSessions))
	// The coordinator lease (ADR-W030 A11). Reading it is `read`
	// because an operator diagnosing a Butler that has gone quiet needs
	// to know which process owns the database; promoting is `manage`
	// because it decides that ownership, and it fences whatever the
	// previous holder still believed it was doing.
	mux.Handle("GET /api/v1/coordinator", s.handle(principal.VerbRead, s.getCoordinator))
	mux.Handle("POST /api/v1/coordinator/promote", s.handle(principal.VerbManage, s.promoteCoordinator))

	mux.Handle("GET /api/v1/butler/sessions/{id}", s.handle(principal.VerbRead, s.getButlerSession))
	// A follow-up carries the read verb like the first question does:
	// conversing is not a write. The surface it runs on comes from the
	// stored session, never from this request (ADR-W027).
	mux.Handle("POST /api/v1/butler/sessions/{id}/ask", s.handle(principal.VerbRead, s.continueButlerSession))

	// The family surface (ADR-W026 A1): a sibling route, not a mode of
	// the operator's. Same read verb — talking is not a write — with a
	// different gateway behind it, one that holds no Statefile access of
	// any kind. Sessions from both surfaces are listed by the routes
	// above, which take ?surface= to tell them apart.
	// Chat bindings (ADR-W026 A4): issuing a code and revoking a binding
	// are `manage` — deciding who may talk to a Butler is principal
	// administration. Redeeming is `converse`, because the caller is an
	// adapter, and a concierge holds that verb and nothing else.
	mux.Handle("POST /api/v1/chat/claims", s.handle(principal.VerbManage, s.issueChatClaim))
	// Chat Connections in one step, and the invite keys a Person fetches
	// for themselves (ADR-W033 B7.1). Creating one mints the adapter's
	// credential, so it is consequential (A4.1).
	mux.Handle("GET /api/v1/connections/platforms", s.handle(principal.VerbRead, s.listChatPlatforms))
	// Making a new bot mints a credential and asks for a fresh sign-in
	// inside the handler; inviting somebody to an existing bot does not.
	mux.Handle("POST /api/v1/connections", s.handle(principal.VerbManage, s.createConnection))
	mux.Handle("GET /api/v1/connections", s.handle(principal.VerbRead, s.listConnections))
	mux.Handle("POST /api/v1/connections/{capability}/remove", s.handle(principal.VerbManage, s.consequential(s.removeConnection)))
	// Consequential for the same reason removal is, in both directions:
	// narrowing withdraws a way the household reaches an Agent, and
	// widening hands out access nobody had (design §8.1; #201 asks each
	// slice to say so rather than take the default).
	mux.Handle("POST /api/v1/connections/{capability}/admission", s.handle(principal.VerbManage, s.consequential(s.setConnectionAdmission)))
	mux.Handle("GET /api/v1/connections/invites", s.handle(principal.VerbRead, s.consoleInvites))
	mux.Handle("POST /api/v1/connections/invites/{seq}/key", s.handle(principal.VerbRead, s.consoleInviteKey))
	mux.Handle("POST /api/v1/chat/bindings/redeem", s.handle(principal.VerbConverse, s.redeemChatClaim))
	mux.Handle("GET /api/v1/chat/bindings", s.handle(principal.VerbManage, s.listChatBindings))
	// What an invite may name: the chat adapters this deployment has,
	// rather than whatever the operator types (ADR-W026 A3).
	mux.Handle("GET /api/v1/chat/transports", s.handle(principal.VerbManage, s.listChatTransports))
	// An adapter says which bot it is, on startup and on a heartbeat.
	// Registered before the {capability} pattern above only in reading
	// order — the mux prefers the literal segment either way, so a
	// capability may not be called "self".
	// Converse: it is the adapter speaking about itself, and which
	// capability it speaks for comes from its own principal.
	mux.Handle("POST /api/v1/chat/adapters/self", s.handle(principal.VerbConverse, s.registerChatAdapter))
	// The adapter contract (ADR-W026 A8): post what you heard, poll for
	// what to say, ack what you delivered. All three carry `converse`,
	// and the surface each one acts on comes from the caller's role.
	// A chat capability's own credential (ADR-W026 A3): the role it gets
	// is decided by the surface, so a family adapter can never hold more
	// than converse.
	mux.Handle("POST /api/v1/chat/adapters/{capability}", s.handle(principal.VerbManage, s.consequential(s.mintChatAdapter)))
	mux.Handle("POST /api/v1/chat/messages", s.handle(principal.VerbConverse, s.receiveChatMessage))
	// A voice message: the same door, with a recording instead of text
	// (ADR-W042). The adapter's credential decides the Connection here
	// exactly as it does for text.
	mux.Handle("POST /api/v1/chat/voice", s.handle(principal.VerbConverse, s.receiveChatVoice))
	mux.Handle("GET /api/v1/chat/outbound", s.handle(principal.VerbConverse, s.pollChatOutbound))
	mux.Handle("GET /api/v1/chat/outbound/{seq}/ready", s.handle(principal.VerbConverse, s.readyChatOutbound))
	mux.Handle("POST /api/v1/chat/outbound/{seq}/ack", s.handle(principal.VerbConverse, s.ackChatOutbound))
	mux.Handle("POST /api/v1/chat/outbound/{seq}/speech", s.handle(principal.VerbConverse, s.speakChatOutbound))
	mux.Handle("POST /api/v1/chat/bindings/{id}/revoke", s.handle(principal.VerbManage, s.revokeChatBinding))

	// The household portal (#197, #198): the two surfaces a family
	// member uses directly, rather than an application they are sent to.
	//
	// Every one of these goes through s.portal, not s.handle. They are
	// on the same listener as the Console's API and they are the only
	// routes on it a household member can reach: the identity s.portal
	// produces holds no RBAC verb at all, so a member whose browser
	// wanders to /api/v1/resources is refused there exactly as an
	// anonymous caller would be.
	//
	// Paths are named for the CAPABILITY that declares each surface,
	// because that capability's install is what makes them answer: with
	// it absent, s.portal refuses with 404 and nothing here is live.
	mux.Handle("GET /filedrop/{$}", s.portal(surfaceFiles, s.portalPage("files")))
	mux.Handle("GET /filedrop/widget", s.portal(surfaceFiles, s.portalPage("files-widget")))
	mux.Handle("GET /api/v1/filedrop/capabilities", s.portal(surfaceFiles, s.filedropCapabilities))
	mux.Handle("GET /api/v1/filedrop/recent", s.portal(surfaceFiles, s.filedropRecent))
	mux.Handle("GET /api/v1/filedrop/list", s.portal(surfaceFiles, s.filedropList))
	mux.Handle("GET /api/v1/filedrop/file", s.portal(surfaceFiles, s.filedropFile))
	mux.Handle("POST /api/v1/filedrop/upload", s.portal(surfaceFiles, s.filedropUpload))
	mux.Handle("POST /api/v1/filedrop/delete", s.portal(surfaceFiles, s.filedropDelete))
	mux.Handle("POST /api/v1/filedrop/mkdir", s.portal(surfaceFiles, s.filedropMkdir))
	mux.Handle("POST /api/v1/filedrop/move", s.portal(surfaceFiles, s.filedropMove))
	mux.Handle("POST /api/v1/filedrop/rename", s.portal(surfaceFiles, s.filedropRename))

	mux.Handle("GET /assistant/{$}", s.portal(surfaceAssistant, s.portalPage("assistant")))
	mux.Handle("GET /assistant/widget", s.portal(surfaceAssistant, s.portalPage("assistant-widget")))
	// The Home dashboard's Lists card (packages/lists-wildstone).
	mux.Handle("GET /api/v1/lists", s.portal(surfaceLists, s.listsRead))
	mux.Handle("POST /api/v1/lists/items", s.portal(surfaceLists, s.listsAdd))
	mux.Handle("POST /api/v1/lists/check", s.portal(surfaceLists, s.listsCheck))
	mux.Handle("POST /api/v1/lists/remove", s.portal(surfaceLists, s.listsRemove))
	mux.Handle("POST /api/v1/lists/clear", s.portal(surfaceLists, s.listsClearChecked))
	mux.Handle("POST /api/v1/lists/delete", s.portal(surfaceLists, s.listsDelete))
	mux.Handle("GET /lists/{$}", s.portal(surfaceLists, s.portalPage("lists")))
	mux.Handle("GET /api/v1/assistant/agents", s.portal(surfaceAssistant, s.assistantAgents))
	mux.Handle("GET /api/v1/assistant/scope", s.portal(surfaceAssistant, s.assistantScope))
	mux.Handle("GET /api/v1/assistant/conversations", s.portal(surfaceAssistant, s.assistantConversations))
	mux.Handle("PUT /api/v1/assistant/memory/privacy", s.portal(surfaceAssistant, s.assistantMemoryPrivacy))
	mux.Handle("GET /api/v1/assistant/memory", s.portal(surfaceAssistant, s.assistantMemory))
	// Forgetting is gated in the handler, not the store: ForgetMemory
	// takes a sequence number and trusts it.
	mux.Handle("POST /api/v1/assistant/memory/promote", s.portal(surfaceAssistant, s.assistantPromoteMemory))
	mux.Handle("POST /api/v1/assistant/memory/forget", s.portal(surfaceAssistant, s.assistantForget))
	mux.Handle("POST /api/v1/assistant/ask", s.portal(surfaceAssistant, s.assistantAsk))
	// Dictation: sound to text for the text box, never a turn (ADR-W042).
	mux.Handle("POST /api/v1/assistant/transcribe", s.portal(surfaceAssistant, s.assistantTranscribe))
	mux.Handle("POST /api/v1/assistant/speak", s.portal(surfaceAssistant, s.assistantSpeak))
	mux.Handle("GET /api/v1/assistant/conversations/{id}", s.portal(surfaceAssistant, s.assistantConversation))

	// The household's own front page (the dashboard capability's
	// wildstone profile). One read answers every card on it, for the
	// reason handlers_portal_home.go opens with: nine reads of a moving
	// database is how two cards end up telling different stories about
	// one event.
	mux.Handle("GET /home/secrets/{$}", s.portal(surfaceHome, s.portalPage("secrets")))
	// Watch this mailbox for a Person who is not an Owner (epic #326 S07):
	// the page over the /api/v1/home/mail and integrations routes below.
	mux.Handle("GET /home/mail/{$}", s.portal(surfaceHome, s.portalPage("mail")))
	mux.Handle("GET /home/{$}", s.portal(surfaceHome, s.portalPage("home")))
	mux.Handle("GET /api/v1/home/summary", s.portal(surfaceHome, s.homeSummary))
	// The one write the page has: a member answering the question they
	// were asked. Whose question it is, is settled in the database.
	mux.Handle("POST /api/v1/home/notices/answer", s.portal(surfaceHome, s.homeAnswerNotice))
	mux.Handle("GET /api/v1/home/connections", s.portal(surfaceHome, s.homeInvites))
	mux.Handle("POST /api/v1/home/connections/{seq}/key", s.portal(surfaceHome, s.homeInviteKey))
	// Clearing takes no seq: see homeClearNotices. It can only ever
	// reach the caller's own notifications.
	mux.Handle("POST /api/v1/home/notices/clear", s.portal(surfaceHome, s.homeClearNotices))
	// The theme a member reads on, shared with the Console: one row,
	// one resolver, so the tablet in the kitchen and the Console on a
	// laptop cannot disagree about what somebody chose. Choosing only —
	// ADDING a theme is an Owner's act on the Console.
	mux.Handle("GET /api/v1/home/appearance", s.portal(surfaceHome, s.homeAppearance))
	mux.Handle("POST /api/v1/home/appearance", s.portal(surfaceHome, s.homeSetAppearance))
	// The wallpaper itself, which is drawn HERE and nowhere else. Any
	// theme's picture is readable by any member on this surface: every
	// theme in the catalogue is one they may choose, so a picker that
	// previews them shows nothing picking them would not.
	mux.Handle("GET /api/v1/home/appearance/themes/{name}/background", s.portal(surfaceHome, s.homeThemeBackground))

	// The household's banner photo. Setting it and framing it are
	// separate writes: the framing is chosen once the picture is on
	// screen, where a person can see what they are choosing.
	// Getting the calendar onto a phone (ADR-W034). The profile is the
	// only path to the shared calendar on iOS; the details endpoint is
	// what every other client needs.
	// The household calendar page: the month, and adding, changing and
	// removing events on the SHARED calendar. Gated on calendar_users,
	// the calendar package's own group — see handlers_portal_calendarpage.go.
	mux.Handle("GET /calendar/{$}", s.portal(surfaceCalendar, s.portalPage("calendar")))
	mux.Handle("POST /api/v1/calendar/visibility", s.portal(surfaceCalendar, s.calendarPreference))
	mux.Handle("POST /api/v1/calendar/personal", s.portal(surfaceCalendar, s.calendarPersonalCreate))
	mux.Handle("GET /api/v1/calendar/events", s.portal(surfaceCalendar, s.calendarPageEvents))
	mux.Handle("POST /api/v1/calendar/events", s.portal(surfaceCalendar, s.calendarPageCreate))
	mux.Handle("POST /api/v1/calendar/events/update", s.portal(surfaceCalendar, s.calendarPageUpdate))
	mux.Handle("POST /api/v1/calendar/events/delete", s.portal(surfaceCalendar, s.calendarPageDelete))
	// The device hand-off, on BOTH surfaces, because both surfaces
	// offer it and they are not the same group of people. The dashboard
	// card carries it under homepage_users; the calendar page carries
	// it permanently and is gated on the calendar capability instead. A
	// household that installed the calendar and not the dashboard, or a
	// member in calendar_users and not homepage_users, could open the
	// page, see a hand-off that quietly fetched a 403, and be offered
	// nothing at all — on the one page that is supposed to be where the
	// hand-off always lives.
	//
	// Same two handlers behind both: what a member may be told about
	// adding the calendar does not depend on which door they came
	// through, only on their being through one.
	mux.Handle("GET /api/v1/home/calendar/profile.mobileconfig", s.portal(surfaceHome, s.calendarProfile))
	mux.Handle("GET /api/v1/home/calendar/setup", s.portal(surfaceHome, s.calendarSetupDetails))
	mux.Handle("GET /api/v1/calendar/profile.mobileconfig", s.portal(surfaceCalendar, s.calendarProfile))
	mux.Handle("GET /api/v1/calendar/setup", s.portal(surfaceCalendar, s.calendarSetupDetails))
	mux.Handle("GET /api/v1/home/banner", s.portal(surfaceHome, s.homeBannerImage))
	mux.Handle("POST /api/v1/home/banner", s.portal(surfaceHome, s.homeBannerSet))
	mux.Handle("POST /api/v1/home/banner/frame", s.portal(surfaceHome, s.homeBannerFrame))
	mux.Handle("POST /api/v1/home/banner/clear", s.portal(surfaceHome, s.homeBannerClear))

	// Activity: the household record, on the same surface as the front
	// page because it is the same projection over a longer window.
	// Answering a commitment from the household's own page (ADR-W035
	// K5). Not a Plan: the same reasoning as a meter reading, and the
	// same three things a person can say — I did it, I skipped it, I
	// booked it — plus the number a meter wants.
	// The tracker page and one commitment's history. Reads, so they sit
	// with the other portal GETs; the answer paths below are unchanged.
	mux.Handle("GET /upkeep/{$}", s.portal(surfaceHome, s.portalPage("upkeep")))
	// One person's day (ADR-W033 A8-A10). On the household surface with
	// a subject selector, NOT a per-person authenticated one: on a
	// kitchen tablet the agenda is protected by the door the household
	// came through, and A10 says so out loud rather than leaving a
	// household to infer privacy the shared screen does not give.
	mux.Handle("GET /agenda/{$}", s.portal(surfaceHome, s.portalPage("agenda")))
	mux.Handle("GET /api/v1/home/agenda", s.portal(surfaceHome, s.homeAgenda))
	mux.Handle("GET /api/v1/home/upkeep", s.portal(surfaceHome, s.homeUpkeep))
	mux.Handle("GET /api/v1/home/commitments/{name}", s.portal(surfaceHome, s.homeCommitmentHistory))
	// The household authors its own tracker (ADR-W035 A20, extended).
	// These write the Statefile without waiting for an Owner, because
	// applying a Meter or a Commitment is a no-op — see
	// handlers_portal_authoring.go for why that is the whole argument.
	mux.Handle("POST /api/v1/home/tracked", s.portal(surfaceHome, s.homeTrack))
	mux.Handle("POST /api/v1/home/commitments", s.portal(surfaceHome, s.homeAddCommitment))
	mux.Handle("PUT /api/v1/home/commitments/{name}", s.portal(surfaceHome, s.homeEditCommitment))
	mux.Handle("DELETE /api/v1/home/commitments/{name}", s.portal(surfaceHome, s.homeRemoveCommitment))
	mux.Handle("DELETE /api/v1/home/meters/{name}", s.portal(surfaceHome, s.homeRemoveMeter))
	mux.Handle("POST /api/v1/home/commitments/{name}/done", s.portal(surfaceHome, s.answerCommitment("done")))
	mux.Handle("POST /api/v1/home/commitments/{name}/waived", s.portal(surfaceHome, s.answerCommitment("waived")))
	mux.Handle("POST /api/v1/home/commitments/{name}/booked", s.portal(surfaceHome, s.answerCommitment("booked")))
	mux.Handle("POST /api/v1/home/meters/{name}/readings", s.portal(surfaceHome, s.homeMeterReading))

	mux.Handle("GET /activity/{$}", s.portal(surfaceHome, s.portalPage("activity")))
	mux.Handle("GET /api/v1/activity", s.portal(surfaceHome, s.activityHistory))

	// The Catalog: request, don't install. Sending files a household
	// event for the owner to act on and proposes no plan — the one
	// field a plan needs is the machine, and that is the owner's call.
	mux.Handle("GET /catalog/{$}", s.portal(surfaceHome, s.portalPage("catalog")))
	mux.Handle("GET /api/v1/catalog", s.portal(surfaceHome, s.catalogList))
	mux.Handle("GET /api/v1/catalog/requests", s.portal(surfaceHome, s.catalogRequests))
	mux.Handle("POST /api/v1/catalog/requests", s.portal(surfaceHome, s.catalogRequest))

	// The portal's static design files: the stylesheet, Inter, the
	// Phosphor glyph font and the app icon set. Shared by every
	// household surface, so the door accepts membership of any of them
	// rather than naming one (portalAsset).
	mux.Handle("GET /portal/{dir}/{name}", s.portalAsset(s.portalAssetFile))

	mux.Handle("GET /api/v1/household/butler", s.handle(principal.VerbRead, s.getHouseholdButlerStatus))
	mux.Handle("POST /api/v1/household/butler/ask", s.handle(principal.VerbRead, s.askHouseholdButler))

	// The Agent API over HTTP (ADR-W039 Phase 2c, #337; ADR-W040 §2–§3):
	// the identical keystone/agentgateway.Gateway methods a native
	// runner calls in-process, reached here by an external runtime with
	// its own bearer. Every route authenticates and RBAC-checks like
	// any other (s.handle), and additionally requires the caller to be
	// a runtime principal (agentContext) — a wider credential that
	// happens to hold read or propose, such as an admin token, is
	// refused here exactly as a non-runtime one is. The handle inside
	// each body, never the URL, carries the task; ADR-W040 §7's "no
	// secret in URLs" is why none of these routes name one.
	//
	// Verbs follow the endpoint map in ADR-W040 §3: `read` where the
	// call only observes or reports (manifest, recall, checkpoint, the
	// four work calls, heartbeat), `propose` where it can change
	// something outside the runtime (invoke, remember, propose,
	// notify) — the decision that this family adds no new RBAC verb,
	// since RoleButler already holds both.
	mux.Handle("POST /api/v1/agent/manifest", s.handle(principal.VerbRead, s.agentManifest))
	mux.Handle("GET /api/v1/agent/tools", s.handle(principal.VerbRead, s.agentTools))
	mux.Handle("POST /api/v1/agent/invoke", s.handle(principal.VerbPropose, s.agentInvoke))
	mux.Handle("POST /api/v1/agent/recall", s.handle(principal.VerbRead, s.agentRecall))
	mux.Handle("POST /api/v1/agent/remember", s.handle(principal.VerbPropose, s.agentRemember))
	mux.Handle("POST /api/v1/agent/checkpoint", s.handle(principal.VerbRead, s.agentCheckpoint))
	mux.Handle("POST /api/v1/agent/start", s.handle(principal.VerbRead, s.agentStart))
	mux.Handle("POST /api/v1/agent/wait", s.handle(principal.VerbRead, s.agentWait))
	mux.Handle("POST /api/v1/agent/complete", s.handle(principal.VerbRead, s.agentComplete))
	mux.Handle("POST /api/v1/agent/fail", s.handle(principal.VerbRead, s.agentFail))
	mux.Handle("POST /api/v1/agent/propose", s.handle(principal.VerbPropose, s.agentPropose))
	mux.Handle("POST /api/v1/agent/notify", s.handle(principal.VerbPropose, s.agentNotify))
	mux.Handle("POST /api/v1/agent/heartbeat", s.handle(principal.VerbRead, s.agentHeartbeat))
	mux.Handle("GET /api/v1/agent/deliveries", s.handle(principal.VerbRead, s.agentDeliveries))
	mux.Handle("POST /api/v1/agent/deliveries/{id}/ack", s.handle(principal.VerbRead, s.agentAckDelivery))

	return mux
}

// handlerFunc is an authenticated handler: it receives the caller identity
// and returns (body, HTTP status). A nil body with a 2xx status writes
// `{"ok":true}`; an error return becomes a JSON error body with the
// returned status.
type handlerFunc func(r *http.Request, id principal.Identity) (any, int, error)

type responseHandlerFunc func(http.ResponseWriter, *http.Request, principal.Identity) (any, int, error)

// handle builds the fixed middleware chain for one route (ADR-W007 A5).
func (s *Server) handle(verb principal.Verb, h handlerFunc) http.Handler {
	return s.handleResponse(verb, func(_ http.ResponseWriter, r *http.Request, id principal.Identity) (any, int, error) {
		body, status, err := h(r, id)
		if body == nil && err == nil {
			body = map[string]bool{"ok": true}
		}
		return body, status, err
	})
}

// handleResponse retains the same authentication/RBAC/rate/audit chain for
// inert attachments. A successful nil body means the handler wrote the bytes.
func (s *Server) handleResponse(verb principal.Verb, h responseHandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// --- authn
		id, ok := s.authenticate(r)
		if !ok {
			// Everything unauthenticated shares one strict bucket so token
			// guessing is throttled globally, not per-source.
			if !s.limiter.allow(anonBucket) {
				s.writeError(w, http.StatusTooManyRequests, "rate limited")
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="wildstone"`)
			// A person the proxy signed in who has no fresh Console
			// session is told where to get one, so the Console can send
			// them there instead of showing a refusal.
			s.writeErrorBody(w, http.StatusUnauthorized, "unauthenticated", s.consoleSigninHint(r))
			return
		}

		// --- RBAC. Denials are audited too (ADR-W007 B5).
		status := 0
		defer func() {
			if err := s.DB.AppendAudit(runtimedb.AuditRecord{
				Principal: id.Name,
				ActingFor: id.ActingFor,
				Method:    r.Method,
				Path:      r.URL.Path,
				Status:    status,
				Remote:    remoteHost(r),
			}); err != nil {
				s.Log.Error("audit append failed", "err", err)
			}
		}()
		if !principal.Allowed(id.Role, verb) {
			status = http.StatusForbidden
			s.writeError(w, status, fmt.Sprintf("role %s may not %s", id.Role, verb))
			return
		}

		// --- rate limit (per principal).
		if !s.limiter.allow(id.Name) {
			status = http.StatusTooManyRequests
			s.writeError(w, status, "rate limited")
			return
		}

		// --- handler.
		body, st, err := h(w, r, id)
		status = st
		if err != nil {
			s.writeErrorBody(w, st, err.Error(), body)
			return
		}
		if body == nil {
			return
		}
		s.writeJSON(w, st, body)
	})
}

func (s *Server) authenticate(r *http.Request) (principal.Identity, bool) {
	auth := r.Header.Get("Authorization")
	const scheme = "Bearer "
	if strings.HasPrefix(auth, scheme) {
		// A token always decides when one is offered, even a bad one.
		// Falling through to the proxy path on a failed token would let
		// a stale or revoked credential quietly become whatever the
		// headers claim, which is an escalation dressed as a fallback.
		id, err := s.Principals.Authenticate(strings.TrimSpace(strings.TrimPrefix(auth, scheme)))
		if err != nil {
			return principal.Identity{}, false
		}
		if !s.actingPersonMayStillHold(id) {
			return principal.Identity{}, false
		}
		return id, true
	}
	return s.consoleForwardAuth(r)
}

// actingPersonMayStillHold re-derives the standing of the PERSON a token
// acts for, on every request (design §3.2, §10.1; §18.1 "Demoting an
// Owner immediately invalidates Console access/session").
//
// A token is minted once and then presented. Authenticate asks whether
// it was revoked, whether it expired, and whether its principal is
// disabled — three questions about the credential, none about the human
// behind it. So an Owner could mint themselves an admin token, be
// demoted, and keep the whole control plane until it expired. The
// forward-auth door already refuses them on their next request; without
// this, demotion locked the door and left the window open.
//
// It sits HERE rather than in butler.authorize() for the reason slice 3
// gives: authorize() returns {Allowed: structural} when no authorizer is
// configured, because it models administrative state. A Person's role
// and lifecycle are not administrative state, so the check goes in front
// of that seam, unconditionally, on the one path every authenticated
// request passes through.
//
// Two deliberate limits:
//
//   - A token's name with no Person behind it passes. Tokens are the
//     installer's and the host's path, and Person records are created
//     through an API that lives behind this check, so refusing here would
//     leave the installer no way to create the first Owner (§2.4). The
//     Console's own door has no such exception: it refuses a forwarded
//     name with no Person record (forwardedUserMayHoldConsole).
//   - Only the CONSOLE's own role requires Owner. RoleAdmin is what
//     consoleForwardAuth mints, so a RoleAdmin token is the Console's
//     authority in another wrapper and is held to the Console's rule.
//     The verb table is deliberately not consulted: RoleOperator also
//     holds approve, and it is the legacy MACHINE axis that decision 6
//     of #201 keeps explicitly separate from Owner/Member — keying an
//     Owner requirement off its verbs would merge the two axes this
//     epic is taking apart. An operator holds the verb for machine
//     operations only; it never decides a Plan (planDecider).
//
// Lifecycle applies to EVERY acting credential, elevated or not.
// Offboarding somebody stops their chat reach as surely as their
// Console, and a concierge acting for a Member must keep working while
// they are active — that is what a family Butler is.
func (s *Server) actingPersonMayStillHold(id principal.Identity) bool {
	// ActingFor, not actingMember(id), and the difference is a real gap
	// that belongs to another slice.
	//
	// actingMember falls back to the principal's own NAME when a token
	// acts for nobody, so a machine principal named after a person's
	// handle writes their memory and their work under them. Checking
	// that name here would revoke such a credential — but it would do it
	// by treating a `principals` row and a `butler_members` row as the
	// same person because they share a string, which merges the two
	// registries decision 6 of #201 keeps apart. `root-admin` the
	// credential and `root-admin` the household member are not the same
	// subject, and a name collision must not make them one.
	//
	// The real defect is the fallback itself: a machine principal's name
	// is not a person handle, and every actingMember call site inherits
	// that. Narrowing the legacy machine axis is #210, which is where it
	// can be fixed once rather than papered over here.
	if id.ActingFor == "" {
		return true
	}
	// Case-insensitively, and for the same reason the Console door is:
	// handles are compared exactly everywhere else, so an exact lookup
	// would miss the Person whenever a different capitalisation was
	// recorded and fall into the permissive "no record" branch — turning
	// a data-hygiene defect into a way to keep a revoked credential.
	people, err := s.DB.ButlerMembersByHandleFold(id.ActingFor)
	if err != nil {
		// A lookup that failed is not a lookup that passed.
		return false
	}
	if len(people) == 0 {
		// No Person of that name, so there is no lifecycle to check.
		// This is the ordinary case for a machine credential —
		// root-admin, an adapter, the node agent — and it must stay
		// permissive: resolving "names nobody" to "refused" would fail
		// the whole control plane closed. It is NOT the Console door's
		// rule, which refuses an unknown name once the household has an
		// Owner; a forwarded human identity and a machine credential are
		// different questions.
		return true
	}
	console := id.Role == runtimedb.RoleAdmin
	// Every match must qualify. With two records differing only by case
	// there is no way to tell which one was meant, and an authorization
	// check that guesses is not a check.
	for _, m := range people {
		if !m.Active() {
			return false
		}
		if console && m.Role != runtimedb.RoleOwner {
			return false
		}
	}
	return true
}

// writeJSON is the ONLY response encoder on the public plane: it redacts
// the serialized bytes, so no handler can leak a secret-shaped value even
// by mistake (ADR-W007 A5 — redaction is a serialization property). The
// typed exceptions are minted tokens, authorized protected Household
// reveal/submission review, artifact selectors/cursors and canonical links,
// preview text/HTML already redacted in full before pagination,
// and the admin-only password-reset link. Ordinary chat and tool replies cannot use those
// response types.
func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	if a, ok := body.(audioResponse); ok {
		a.write(w, status)
		return
	}
	raw, err := json.Marshal(body)
	if err != nil {
		s.Log.Error("response marshal failed", "err", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	switch body.(type) {
	case artifactListResponse:
		if raw, err = body.(artifactListResponse).redacted(); err != nil {
			s.writeError(w, http.StatusInternalServerError, "internal")
			return
		}
	case artifactPreviewResponse:
		if raw, err = body.(artifactPreviewResponse).redacted(); err != nil {
			s.writeError(w, http.StatusInternalServerError, "internal")
			return
		}
	case secretRevealResponse:
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
	case mintResponse, joinTokenResponse, runtimeCredentialResponse:
		// The deliberate carve-outs (ADR-W007 A3, ADR-W014 A3, ADR-W039
		// §4.1): a newly minted API token, join token or runtime
		// credential, each in the only moment it ever exists in a
		// response.
	case identityNotification:
		// Redacted everywhere but the password-reset link an admin hands
		// over (identityNotification.redacted), and never cached.
		if raw, err = body.(identityNotification).redacted(); err != nil {
			s.Log.Error("response redaction failed", "err", err)
			http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
	case mailCandidateResponse:
		// Redacted everywhere but the Person's own review tokens, which
		// they present back to act (mailCandidateResponse.redacted), and
		// never cached.
		if raw, err = body.(mailCandidateResponse).redacted(); err != nil {
			s.Log.Error("response redaction failed", "err", err)
			http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
	case deliveriesResponse:
		// Redacted everywhere but each delivery's task handle, which the
		// runtime must present back (deliveriesResponse.redacted).
		d := body.(deliveriesResponse)
		if raw, err = d.redacted(); err != nil {
			s.Log.Error("response redaction failed", "err", err)
			http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			return
		}
	default:
		raw = []byte(redact.String(string(raw)))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(raw)
}

// audioResponse is a reply read aloud (ADR-W042 part two), written as
// the audio itself rather than as JSON. It is the one binary body: the
// words it was made from were redacted before they were spoken, and a
// JSON-and-redact pass over base64 audio would only corrupt it.
type audioResponse struct {
	MIME  string
	Audio []byte
	// Engine is where it was made, "local" or "cloud", for a caller
	// that shows it.
	Engine string
}

func (a audioResponse) write(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", a.MIME)
	w.Header().Set("Content-Length", strconv.Itoa(len(a.Audio)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if a.Engine != "" {
		w.Header().Set("X-Wildstone-Speech-Engine", a.Engine)
	}
	w.WriteHeader(status)
	w.Write(a.Audio)
}

// mintResponse is the one body shape allowed to carry a secret: the newly
// minted token, in the only moment it ever exists in a response (ADR-W007
// A3/B3).
type mintResponse struct {
	Token string `json:"token"`
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

// writeErrorBody is writeError for a failure that still has something to
// hand back.
//
// Almost every failed request has nothing to say beyond why it failed,
// and passes body nil to get exactly the old shape. One case is not like
// that: a Butler turn can fail AFTER tools have already run, and the
// session holding that durable work is the only way the caller reaches
// it again. A surface with no server-side thread pointer — the Console
// and the API, unlike chat — would otherwise retry, open a fresh task,
// and repeat calls that already changed the house.
//
// The body's own fields are merged and "error" is added last, so it wins
// over any field of that name rather than being silently dropped. A body
// that cannot be rendered as an object degrades to the plain error
// rather than failing the response: the caller is already being told
// something went wrong, and that message is the part it cannot lose.
func (s *Server) writeErrorBody(w http.ResponseWriter, status int, msg string, body any) {
	if body == nil {
		s.writeError(w, status, msg)
		return
	}
	raw, err := json.Marshal(body)
	if err != nil {
		s.writeError(w, status, msg)
		return
	}
	fields := map[string]any{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		s.writeError(w, status, msg)
		return
	}
	fields["error"] = msg
	s.writeJSON(w, status, fields)
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// SetTrustedIngress replaces the addresses the identity proxy is
// believed to speak from.
//
// Called at startup from the -household-ingress flag, and again by the
// reconciler each time it learns the proxy container's address. The two
// are the same act: this is the only statement about where the proxy
// is, and the later one wins.
func (s *Server) SetTrustedIngress(addrs []string) {
	s.trustedIngress.Store(&addrs)
}

// TrustedIngressAddrs reports the current set, for logging and for the
// refusal messages that name it. Callers must not mutate the result.
func (s *Server) TrustedIngressAddrs() []string {
	if v := s.trustedIngress.Load(); v != nil {
		return *v
	}
	return nil
}
