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

	// Household members (ADR-W020 A3, ADR-W023 A7): the household's
	// account list IS the LdapUser resources in the Statefile. Reading
	// the roster is a read; adding a person is a propose, because that
	// is literally what it does — the account exists once a human
	// approves the plan.
	mux.Handle("GET /api/v1/household/members", s.handle(principal.VerbRead, s.listHouseholdMembers))
	mux.Handle("POST /api/v1/household/members", s.handle(principal.VerbPropose, s.addHouseholdMember))
	mux.Handle("GET /api/v1/household/groups", s.handle(principal.VerbRead, s.listHouseholdGroups))

	// Butler (ADR-W017 A2): ask carries the read verb — conversing is not
	// a write; a session's proposals enter through the same submitProposal
	// pipeline as POST /plans, as principal "butler" acting-for the
	// caller, and only a human approve verb moves them further.
	mux.Handle("GET /api/v1/butler", s.handle(principal.VerbRead, s.getButlerStatus))
	// Admin-only: it reaches out to an operator-supplied address from
	// inside the control plane, which is not a read a viewer should be
	// able to make.
	mux.Handle("GET /api/v1/butler/models", s.handle(principal.VerbManage, s.listButlerModels))
	mux.Handle("GET /api/v1/butler/config", s.handle(principal.VerbManage, s.getButlerConfig))
	mux.Handle("PUT /api/v1/butler/config", s.handle(principal.VerbManage, s.setButlerConfig))
	mux.Handle("GET /api/v1/butler/memory", s.handle(principal.VerbRead, s.retrieveButlerMemory))
	mux.Handle("GET /api/v1/butler/memory/export", s.handle(principal.VerbRead, s.exportButlerMemory))
	mux.Handle("POST /api/v1/butler/memory/correct", s.handle(principal.VerbRead, s.correctButlerMemory))
	mux.Handle("POST /api/v1/butler/memory/forget", s.handle(principal.VerbRead, s.forgetButlerMemory))
	mux.Handle("GET /api/v1/butler/memory/digest", s.handle(principal.VerbRead, s.listMemoryDigest))
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
	mux.Handle("GET /api/v1/butler/evaluations", s.handle(principal.VerbRead, s.listButlerEvaluations))
	mux.Handle("GET /api/v1/butler/debug", s.handle(principal.VerbRead, s.listButlerDebugCaptures))
	mux.Handle("GET /api/v1/butler/settings", s.handle(principal.VerbManage, s.getButlerSettings))
	mux.Handle("PUT /api/v1/butler/settings", s.handle(principal.VerbManage, s.setButlerSettings))
	mux.Handle("POST /api/v1/butler/ask", s.handle(principal.VerbRead, s.askButler))
	mux.Handle("GET /api/v1/butler/sessions", s.handle(principal.VerbRead, s.listButlerSessions))
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
	if !strings.HasPrefix(auth, scheme) {
		return principal.Identity{}, false
	}
	id, err := s.Principals.Authenticate(strings.TrimSpace(strings.TrimPrefix(auth, scheme)))
	if err != nil {
		return principal.Identity{}, false
	}
	return id, true
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
	case mintResponse, joinTokenResponse:
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
