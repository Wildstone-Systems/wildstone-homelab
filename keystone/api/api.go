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
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

// Server serves the public plane. All fields are required except Log and
// OnApproved.
type Server struct {
	Principals *principal.Store
	DB         *runtimedb.DB
	// Catalog is what this control plane can install (ADR-W021 A3).
	// nil leaves the package and capability endpoints answering 503;
	// every other route is unaffected, the same posture Agents takes.
	Catalog *catalog.Catalog
	State   *statefile.Store
	Log     *slog.Logger

	// TrustedIngress is the exact set of remote addresses the identity
	// proxy speaks from (ADR-W030 A8.1). It gates the household review
	// route, which trusts an identity header — and a header anyone can
	// set is not an authentication. Empty means the household review
	// path is unavailable rather than open: an unconfigured trust
	// boundary must fail closed.
	TrustedIngress []string

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

	// Butlers serves conversations as named specialists (ADR-W031) —
	// cmd/keystone wires it over butlerlive, butlerstore and the
	// durable runner. nil leaves every conversation on the single
	// -butler-provider path, which is what a deployment that has
	// created no Butlers keeps running.
	Butlers ButlerConversations

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

	limiter *limiter
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
	mux.Handle("POST /api/v1/plans/{id}/approve", s.handle(principal.VerbApprove, s.approvePlan))
	mux.Handle("POST /api/v1/plans/{id}/reject", s.handle(principal.VerbApprove, s.rejectPlan))

	mux.Handle("GET /api/v1/audit", s.handle(principal.VerbManage, s.listAudit))
	// Admin-only: the pending one-time code is a credential in flight.
	mux.Handle("GET /api/v1/nodes/{node}/identity/code", s.handle(principal.VerbManage, s.identityCode))
	mux.Handle("GET /api/v1/principals", s.handle(principal.VerbManage, s.listPrincipals))
	mux.Handle("POST /api/v1/principals", s.handle(principal.VerbManage, s.createPrincipal))
	mux.Handle("POST /api/v1/principals/{name}/disable", s.handle(principal.VerbManage, s.setPrincipalDisabled(true)))
	mux.Handle("POST /api/v1/principals/{name}/enable", s.handle(principal.VerbManage, s.setPrincipalDisabled(false)))
	mux.Handle("POST /api/v1/principals/{name}/tokens", s.handle(principal.VerbManage, s.mintToken))

	// Node inventory (#30): the one route that does not need a node name
	// up front — everything else under /nodes/{node} does.
	mux.Handle("GET /api/v1/nodes", s.handle(principal.VerbRead, s.listNodes))

	// Pairing (ADR-W014 A3): admin mints a single-use join token + CA
	// fingerprint for a node install.
	mux.Handle("POST /api/v1/join-tokens", s.handle(principal.VerbManage, s.mintJoinToken))

	// Secrets (ADR-W012 W2): admin-only, name-level only — values transit
	// the set endpoint once and are never persisted, logged, or readable
	// back (the protocol has no GetSecret arm).
	mux.Handle("GET /api/v1/nodes/{node}/secrets", s.handle(principal.VerbManage, s.listNodeSecrets))
	// The install form's folder picker (ADR-W021 A5). Read-only, confined
	// to the prefixes a bind mount may name.
	mux.Handle("GET /api/v1/nodes/{node}/directories", s.handle(principal.VerbRead, s.listDirectories))
	mux.Handle("POST /api/v1/nodes/{node}/secrets/{name}", s.handle(principal.VerbManage, s.setSecret))
	mux.Handle("POST /api/v1/nodes/{node}/secrets/{name}/rotate", s.handle(principal.VerbManage, s.rotateSecret))

	// Onboarding validation probes (ADR-W016 A2, E10-U2): dial-or-read
	// only on the agent side, so the whole family carries read.
	mux.Handle("GET /api/v1/nodes/{node}/probes/blockdevices", s.handle(principal.VerbRead, s.probeBlockDevices))
	mux.Handle("GET /api/v1/nodes/{node}/probes/ports", s.handle(principal.VerbRead, s.probePorts))
	mux.Handle("GET /api/v1/nodes/{node}/probes/http", s.handle(principal.VerbRead, s.probeHttp))
	mux.Handle("GET /api/v1/nodes/{node}/probes/nfs", s.handle(principal.VerbRead, s.probeDial("nfs")))
	mux.Handle("GET /api/v1/nodes/{node}/probes/smb", s.handle(principal.VerbRead, s.probeDial("smb")))

	// Container logs (ADR-W015 A2): read verb; the response passes the
	// redacting encoder like everything else.
	mux.Handle("GET /api/v1/nodes/{node}/containers/{name}/logs", s.handle(principal.VerbRead, s.containerLogs))

	// Backups (ADR-W015 A2): ledger reads for everyone with read; run and
	// restore are human-initiated executions — approve verb, so the
	// Butler's propose-only role cannot trigger them.
	mux.Handle("GET /api/v1/backups", s.handle(principal.VerbRead, s.listBackups))
	mux.Handle("POST /api/v1/backups/{plan}/run", s.handle(principal.VerbApprove, s.runBackup))
	mux.Handle("POST /api/v1/backups/{plan}/restore", s.handle(principal.VerbApprove, s.restoreBackup))

	// Morning report (ADR-W017 A5): deterministic digest, read verb.
	mux.Handle("GET /api/v1/reports/morning", s.handle(principal.VerbRead, s.morningReport))

	// Household events (ADR-W024): the intake (emit — the publisher
	// role's only verb), the cursor (read), and publisher credentialing
	// (manage).
	mux.Handle("POST /api/v1/household/events", s.handle(principal.VerbEmit, s.emitHouseholdEvent))
	mux.Handle("GET /api/v1/household/events", s.handle(principal.VerbRead, s.listHouseholdEvents))
	mux.Handle("POST /api/v1/household/publishers/{capability}", s.handle(principal.VerbManage, s.mintPublisher))
	// Replay is a read: it evaluates and returns, writes nothing.
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

	// Butler (ADR-W017 A2): ask carries the read verb — conversing is not
	// a write; a session's proposals enter through the same submitProposal
	// pipeline as POST /plans, as principal "butler" acting-for the
	// caller, and only a human approve verb moves them further.
	mux.Handle("GET /api/v1/butler", s.handle(principal.VerbRead, s.getButlerStatus))
	// Admin-only: it reaches out to an operator-supplied address from
	// inside the control plane, which is not a read a viewer should be
	// able to make.
	mux.Handle("GET /api/v1/butler/models/offered", s.handle(principal.VerbManage, s.listOfferedModels))
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
	// The adapter's side: claim and answer, and nothing else.
	mux.Handle("POST /api/v1/integration-work/claim", s.handle(principal.VerbClaimIntegrationWork, s.claimIntegrationWork))
	mux.Handle("POST /api/v1/integration-work/result", s.handle(principal.VerbReportIntegrationResult, s.reportIntegrationResult))
	mux.Handle("GET /api/v1/butler/index/measurements", s.handle(principal.VerbRead, s.listIndexMeasurements))
	mux.Handle("POST /api/v1/butler/index/compact", s.handle(principal.VerbManage, s.compactButlerIndex))
	mux.Handle("GET /api/v1/butler/memory", s.handle(principal.VerbRead, s.retrieveButlerMemory))
	mux.Handle("GET /api/v1/butler/memory/export", s.handle(principal.VerbRead, s.exportButlerMemory))
	mux.Handle("POST /api/v1/butler/memory/correct", s.handle(principal.VerbRead, s.correctButlerMemory))
	mux.Handle("POST /api/v1/butler/memory/forget", s.handle(principal.VerbRead, s.forgetButlerMemory))
	mux.Handle("GET /api/v1/butler/memory/digest", s.handle(principal.VerbRead, s.listMemoryDigest))
	mux.Handle("GET /api/v1/household/routines", s.handle(principal.VerbRead, s.listHouseholdRoutines))
	mux.Handle("GET /api/v1/household/routines/{name}", s.handle(principal.VerbRead, s.getHouseholdRoutine))
	mux.Handle("POST /api/v1/household/routines", s.handle(principal.VerbPropose, s.createHouseholdRoutine))
	mux.Handle("POST /api/v1/household/routines/{name}/edit", s.handle(principal.VerbPropose, s.editHouseholdRoutine))
	mux.Handle("POST /api/v1/household/routines/{name}/state", s.handle(principal.VerbPropose, s.setHouseholdRoutineState))
	mux.Handle("POST /api/v1/household/review/token", s.handle(principal.VerbApprove, s.issueHouseholdReviewToken))
	// The narrow acting-for service route (ADR-W030 A8.1). It carries no
	// API verb: identity comes from the trusted ingress in front of it,
	// and authority comes entirely from a single-use, proposal-bound
	// token. A request without both gets nothing.
	mux.HandleFunc("POST /api/v1/household/review", s.householdReview)
	// The chat callback path. It needs `converse` — the adapter's only
	// verb — because the adapter relays a tap; the identity comes from
	// the binding a human redeemed, and the authority from the token.
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
	mux.Handle("POST /api/v1/butler/members/{member}/disable", s.handle(principal.VerbManage, s.disableButlerMember))
	mux.Handle("POST /api/v1/butler/members/{member}/offboard", s.handle(principal.VerbManage, s.offboardButlerMember))
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
	mux.Handle("GET /api/v1/butler/evaluations", s.handle(principal.VerbRead, s.listButlerEvaluations))
	mux.Handle("GET /api/v1/butler/debug", s.handle(principal.VerbRead, s.listButlerDebugCaptures))
	mux.Handle("POST /api/v1/butler/members/{member}/offboarding", s.handle(principal.VerbManage, s.offboardMember))
	mux.Handle("GET /api/v1/restore", s.handle(principal.VerbRead, s.getRestoreState))
	mux.Handle("POST /api/v1/restore/quarantine", s.handle(principal.VerbManage, s.enterRestoreQuarantine))
	mux.Handle("POST /api/v1/restore/reconcile", s.handle(principal.VerbManage, s.reconcileRestore))
	mux.Handle("GET /api/v1/notices", s.handle(principal.VerbRead, s.listNotices))
	mux.Handle("POST /api/v1/notices/acknowledge", s.handle(principal.VerbRead, s.acknowledgeNotice))
	mux.Handle("POST /api/v1/notices/answer", s.handle(principal.VerbRead, s.answerNotice))
	mux.Handle("POST /api/v1/notices/expire", s.handle(principal.VerbManage, s.expireNotices))
	mux.Handle("GET /api/v1/notices/preferences", s.handle(principal.VerbRead, s.getNoticePreferences))
	mux.Handle("PUT /api/v1/notices/preferences", s.handle(principal.VerbRead, s.setNoticePreferences))
	mux.Handle("PUT /api/v1/notices/kinds", s.handle(principal.VerbManage, s.setNoticeKind))
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
	mux.Handle("PUT /api/v1/butlers/{name}/address", s.handle(principal.VerbManage, s.setButlerAddress))
	mux.Handle("DELETE /api/v1/butlers/{name}", s.handle(principal.VerbManage, s.retireButler))
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
	mux.Handle("POST /api/v1/chat/adapters/{capability}", s.handle(principal.VerbManage, s.mintChatAdapter))
	mux.Handle("POST /api/v1/chat/messages", s.handle(principal.VerbConverse, s.receiveChatMessage))
	mux.Handle("GET /api/v1/chat/outbound", s.handle(principal.VerbConverse, s.pollChatOutbound))
	mux.Handle("POST /api/v1/chat/outbound/{seq}/ack", s.handle(principal.VerbConverse, s.ackChatOutbound))
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
	mux.Handle("POST /api/v1/filedrop/rename", s.portal(surfaceFiles, s.filedropRename))

	mux.Handle("GET /assistant/{$}", s.portal(surfaceAssistant, s.portalPage("assistant")))
	mux.Handle("GET /assistant/widget", s.portal(surfaceAssistant, s.portalPage("assistant-widget")))
	mux.Handle("GET /api/v1/assistant/agents", s.portal(surfaceAssistant, s.assistantAgents))
	mux.Handle("GET /api/v1/assistant/scope", s.portal(surfaceAssistant, s.assistantScope))
	mux.Handle("GET /api/v1/assistant/conversations", s.portal(surfaceAssistant, s.assistantConversations))
	mux.Handle("POST /api/v1/assistant/ask", s.portal(surfaceAssistant, s.assistantAsk))
	mux.Handle("GET /api/v1/assistant/conversations/{id}", s.portal(surfaceAssistant, s.assistantConversation))

	// The household's own front page (the dashboard capability's
	// wildstone profile). One read answers every card on it, for the
	// reason handlers_portal_home.go opens with: nine reads of a moving
	// database is how two cards end up telling different stories about
	// one event.
	mux.Handle("GET /home/{$}", s.portal(surfaceHome, s.portalPage("home")))
	mux.Handle("GET /api/v1/home/summary", s.portal(surfaceHome, s.homeSummary))
	// The one write the page has: a member answering the question they
	// were asked. Whose question it is, is settled in the database.
	mux.Handle("POST /api/v1/home/notices/answer", s.portal(surfaceHome, s.homeAnswerNotice))
	// Clearing takes no seq: see homeClearNotices. It can only ever
	// reach the caller's own notifications.
	mux.Handle("POST /api/v1/home/notices/clear", s.portal(surfaceHome, s.homeClearNotices))

	// The portal's static design files: the stylesheet, Inter, the
	// Phosphor glyph font and the app icon set. Shared by every
	// household surface, so the door accepts membership of any of them
	// rather than naming one (portalAsset).
	mux.Handle("GET /portal/{dir}/{name}", s.portalAsset(s.portalAssetFile))

	mux.Handle("GET /api/v1/household/butler", s.handle(principal.VerbRead, s.getHouseholdButlerStatus))
	mux.Handle("POST /api/v1/household/butler/ask", s.handle(principal.VerbRead, s.askHouseholdButler))

	return mux
}

// handlerFunc is an authenticated handler: it receives the caller identity
// and returns (body, HTTP status). A nil body with a 2xx status writes
// `{"ok":true}`; an error return becomes a JSON error body with the
// returned status.
type handlerFunc func(r *http.Request, id principal.Identity) (any, int, error)

// handle builds the fixed middleware chain for one route (ADR-W007 A5).
func (s *Server) handle(verb principal.Verb, h handlerFunc) http.Handler {
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
			s.writeError(w, http.StatusUnauthorized, "unauthenticated")
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
		body, st, err := h(r, id)
		status = st
		if err != nil {
			s.writeError(w, st, err.Error())
			return
		}
		if body == nil {
			body = map[string]bool{"ok": true}
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
		return id, true
	}
	return s.consoleForwardAuth(r)
}

// writeJSON is the ONLY response encoder on the public plane: it redacts
// the serialized bytes, so no handler can leak a secret-shaped value even
// by mistake (ADR-W007 A5 — redaction is a serialization property). The
// one deliberate hole is token minting, which marks its response with
// mintResponse to skip redaction for the single legitimate
// secret-bearing reply.
func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		s.Log.Error("response marshal failed", "err", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	switch body.(type) {
	case mintResponse, joinTokenResponse, reviewTokenResponse:
		// The two deliberate carve-outs (ADR-W007 A3, ADR-W014 A3): a
		// newly minted API token and a newly minted join token, each in
		// the only moment it ever exists in a response.
	default:
		raw = []byte(redact.String(string(raw)))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(raw)
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

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
