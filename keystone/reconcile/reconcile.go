// Package reconcile is Keystone's reconcile engine (ADR-W004 A4):
// observe -> diff -> plan -> approve -> apply -> health-gate -> rollback,
// for the three Wardstone-applied kinds (Container, Volume, Route) plus
// the provider-backed DnsRecord kind (ADR-W004 A5, S2c). DnsRecord never
// touches Applier/Wardstone at all — the Reconciler routes it through a
// keystone/provider.Provider looked up from Providers[resource.KindDnsRecord]
// for Observe/Create/Update/Delete, using the same diff -> plan -> apply ->
// health-gate loop as the Wardstone-applied kinds. A *provider.RateLimited
// or *provider.Transient error is retried by the Reconciler's existing
// bounded-retry (like a transient Wardstone op failure) and only becomes
// an Outcome.Incident once those retries are exhausted.
//
// # Observe: pragmatic, per-kind, documented
//
// The Wardstone op surface (proto/wardstone/v1) has no generic "inspect"
// arm, so each kind's observe step uses whatever signal its existing ops
// give it, per ADR-W004 A4/B1:
//
//   - Container: InspectContainer (protocol minor 5, ADR-W008 R2c) reads
//     back the running container's labels; the dev.wildstone.spec-hash
//     label stamped at EnsureContainer is compared against a fresh hash
//     of the desired spec. Equal + running = NoOp; different or stopped =
//     spec drift, planned as an Update (the agent's ensureContainer
//     replaces on spec-hash mismatch, so the Update converges env/port/
//     mount changes, not just image changes). This closes the S2b
//     presence-vs-spec gap. An agent that answers INVALID (pre-minor-5)
//     gets the old presence-only ContainerStats observe — see
//     observeContainerPresence.
//   - Volume: there is no volume-inspect op at all (only EnsureVolume).
//     Observe is therefore "observe-by-apply": the Reconciler treats a
//     volume as existing iff a Reconciler-tracked, in-memory "managed" set
//     says a prior Reconcile call successfully created it. This is process-
//     lifetime state, not persisted — a fresh Reconciler (e.g. after a
//     Keystone restart) rediscovers volumes the first time they appear in
//     a plan again, safely, because EnsureVolume is idempotent.
//   - Route: ReadManagedFile of the rendered fragment path, byte-compared
//     against a fresh render of the desired spec. This is a true live
//     check (present + equal = exists, present + different = Update,
//     NOT_FOUND = absent) — no caching needed.
//
// # Drift classes (ADR-W004 A4)
//
// Reconcilable drift (e.g. a container stopped/removed out-of-band) is
// repaired by the normal diff -> plan -> apply path. Foreign drift — a
// name occupied by an object the agent's ownership guard reports as not
// wildstone-owned (FORBIDDEN on ContainerStats) — is never planned as a
// delete: observe excludes foreign objects from the observed set
// entirely (so diff never even considers them) and instead reports them
// in Outcome.ForeignDrift for a human to see and adopt/rename around.
//
// # Health gate and rollback
//
// Every apply is followed by a post-condition probe (ADR-W004 A4 step 5):
// Container -> ContainerStats reports success (retried with backoff);
// Route -> ProbeHttp against the upstream (ip:port upstreams only — see
// probeRoute); Volume -> a second, idempotent EnsureVolume call succeeding.
// A gate failure runs the step's reverse (where one exists) and halts —
// later steps in the plan are never applied.
package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// resKey identifies a resource for the Reconciler's internal bookkeeping
// (the "managed" cache, desired-by-key lookups): matching is by (kind,
// name), same as plan.Diff.
type resKey struct {
	Kind resource.Kind
	Name string
}

// StepResult records what happened when the Reconciler tried to apply one
// plan.PlanStep.
type StepResult struct {
	Step plan.PlanStep
	// Applied is true once the op(s) for this step were dispatched and
	// returned success (before any health gate runs). False means the op
	// itself failed (transport error, OpError, or — for a Volume Delete —
	// the apply-time safety refusal); Outcome.Incident carries the reason.
	Applied bool
	// HealthOK is true once the step's post-condition probe passed.
	// Meaningless (false) if Applied is false.
	HealthOK bool
	// Reversed is true if a health-gate failure triggered this step's
	// reverse action.
	Reversed bool
	// Error is the human-readable failure reason when Applied is false.
	Error string
}

// Outcome is what one Reconcile call produced.
type Outcome struct {
	// Plan is the diff-produced plan this Reconcile call evaluated —
	// always populated, even when nothing was applied.
	Plan *plan.Plan
	// Applied is true iff every step in Plan.Steps was applied and passed
	// its health gate (or the plan was empty to begin with).
	Applied bool
	// Rolledback is true iff a step's health gate failed, its reverse (if
	// any) ran, and later steps were never attempted.
	Rolledback bool
	// PendingApproval is true iff Plan.OverallRisk was Medium/High and no
	// Approve callback (or one that returned false) authorized it — in
	// that case nothing in Plan was applied at all.
	PendingApproval bool
	// Incident is set when a step's op failed outright (not a health-gate
	// failure — see Rolledback for that) after exhausting retries, or hit
	// the Volume-Delete apply-time refusal. Later steps were not attempted.
	Incident string
	// ForeignDrift lists Container names observe found occupied by a
	// non-wildstone-owned object. Never planned as a delete (ADR-W004 A4).
	ForeignDrift []string
	// Steps records one StepResult per plan step actually attempted (a
	// halt on Incident/Rolledback means later steps have no entry here).
	Steps []StepResult
	// PrereqRepaired lists managed files that were found altered and
	// rewritten this tick (see repairNodePrereqs). These are not plan
	// steps: they are files Wildstone owns outright, whose correct
	// contents are a pure function of the renderer, so restoring one is
	// the low-risk self-repair concepts.md promises rather than a change
	// needing approval.
	PrereqRepaired []string
}

// Reconciler drives the ADR-W004 A4 reconcile loop for the three
// Wardstone-applied kinds (Container, Volume, Route) against one node at a
// time. The zero value is usable with built-in defaults (see the const
// block below) except for Applier and ManagedPrefix, which callers must
// set.
type Reconciler struct {
	// Applier dispatches operations to the target node. Production callers
	// set this to &GatewayApplier{Gateway: gw}.
	Applier Applier

	// Providers routes provider-backed kinds (currently only
	// resource.KindDnsRecord, ADR-W004 A5) to their external-resource
	// controller. A DnsRecord in desired with no entry here is an
	// Outcome.Incident ("no provider configured"), not a silent skip —
	// S2b's silent-skip behavior for DnsRecord ended with S2c.
	Providers map[resource.Kind]provider.Provider

	// ManagedPrefix is the managed-file prefix Route fragments are
	// rendered under: <ManagedPrefix>/routes/<name>.caddy. Must be one of
	// the target agent's configured ManagedPrefixes.
	ManagedPrefix string

	// AutheliaAddr is the forward-auth endpoint rendered into routes with
	// auth "authelia_oidc". Empty uses DefaultAutheliaAddr, which matches
	// the container the identity package deploys.
	AutheliaAddr string

	// Approve decides whether a Medium/High risk plan.Plan may apply. nil
	// (the zero value) means default-deny (ADR-W004 A3: "Medium/High
	// require explicit approval") — every Medium/High plan comes back as
	// Outcome.PendingApproval with nothing applied.
	Approve func(*plan.Plan) bool

	// ReloadFn is the Route-apply reload seam (ADR-W004 A1: "a reload op
	// seam"). nil (the default) is a no-op, and for the shipped ingress
	// profiles that is now correct rather than merely deferred: both run
	// Caddy with --watch, so writing a fragment under <prefix>/routes/ IS
	// the reload — verified on a live Caddy, add and remove alike, with
	// unaffected routes left serving throughout.
	//
	// Before --watch, a Route added after the first apply was written,
	// reconciled green, and inert until someone restarted the container
	// (F24) — the same "converged on paper, no working front door" shape
	// the network fix went after. The seam stays for an ingress that
	// cannot watch its own config.
	ReloadFn func(ctx context.Context) error

	// ReloadAutheliaFn is the OidcClient-apply batched reload seam
	// (ADR-W005 A3/B3): called at most ONCE per Reconcile call, after every
	// step in the plan (of any kind) has been attempted, iff at least one
	// OidcClient step in the plan Applied. This is deliberately a distinct
	// seam from ReloadFn/Route's per-step reload — the whole point of the
	// batching requirement is that N OidcClient changes in one plan must
	// not restart Authelia N times. nil (the default) is a documented
	// no-op, same posture as ReloadFn: the real Authelia reload/restart
	// call lands in a later E-series work package.
	ReloadAutheliaFn func(ctx context.Context) error

	// JobGateAttempts and JobGateBackoff bound the wait for a Job's
	// container to reach a terminal state. Zero uses the defaults in
	// job.go, which are far longer than the container health gate on
	// purpose: a provisioning run legitimately takes seconds.
	JobGateAttempts int
	JobGateBackoff  time.Duration

	// MaxApplyRetries bounds retries of a single op dispatch on a
	// transient failure (transport error, or an OpError with code
	// RUNTIME) before the step is given up on (ADR-W004 A4: "bounded
	// remediation... spike N=3"). <= 0 uses the default of 3.
	MaxApplyRetries int
	// RetryBackoff is the delay between retryDispatch attempts. <= 0 uses
	// a 50ms default.
	RetryBackoff time.Duration

	// HealthGateAttempts bounds how many times a health-gate probe
	// (ContainerStats / ProbeHttp / EnsureVolume) is retried before the
	// gate is declared failed. <= 0 uses a default of 5.
	HealthGateAttempts int
	// HealthGateBackoff is the delay between health-gate probe attempts.
	// <= 0 uses a 100ms default.
	HealthGateBackoff time.Duration

	mu      sync.Mutex
	managed map[string]map[resKey]bool // nodeID -> (kind,name) -> Reconciler believes it exists
	// posture is the node's ingress posture for the tick in flight,
	// derived from desired state at the top of Reconcile.
	//
	// Cached rather than threaded because three renderer call sites sit
	// deep in the apply and observe paths where `desired` is long out of
	// scope, and widening three signatures to carry a value that is
	// constant for the whole tick is the worse trade. Written once per
	// tick under mu, read through currentPosture().
	posture ingressPosture
	// configFilePaths remembers where each applied ConfigFile went.
	//
	// Every other managed file derives its path from the resource NAME,
	// so a delete can find it with the spec gone. A ConfigFile's path
	// comes from the spec's filename and the owner's capability, both of
	// which vanish with the resource — so without this, a file that left
	// desired state could never be located, and would sit on disk being
	// read by an app forever.
	configFilePaths map[string]map[resKey]string
}

// Reconcile drives one observe -> diff -> plan -> approve -> apply ->
// health-gate pass for nodeID against desired. Wardstone-applied kinds
// (Container, Volume, Route) go through Applier; DnsRecord goes through
// Providers[resource.KindDnsRecord] (ADR-W004 A5, S2c) — nodeID is unused
// for provider-backed kinds, which are not node-scoped. OidcClient
// (ADR-W005 A3) also goes through Applier (a WriteManagedFile-rendered
// Authelia fragment, like Route); LdapGroup (ADR-W005 A3) is declared/
// best-effort and never dispatches an op at all — see ldapgroup.go. Both
// are grouped with the Applier-routed kinds below since neither uses the
// Provider path; nodeID is likewise not meaningfully load-bearing for
// LdapGroup, which is not truly node-scoped, but is threaded through for
// symmetry with the managed-cache bookkeeping the other kinds already use.
// repairNodePrereqs re-asserts the managed files that are Wildstone's
// alone — the base Caddyfile and the routes placeholder — on every tick.
//
// They were written only by ensureNodePrereqs, which runs immediately
// before a Container ensure and nowhere else, and neither file is a
// resource, so drift detection could not see them either. Deleting the
// `:80 { abort }` block from the base Caddyfile brought F26 back and
// nothing repaired it across multiple ticks; emptying the placeholder
// brought F30's one-warning-per-second back permanently. Both recovered
// only as a side effect of deploying some container, which is a strange
// remedy for "my front door stopped denying unknown hosts" (F34).
//
// Repaired silently rather than proposed: unlike a resource, these files
// have no desired-state spec a human could have meant differently — the
// renderer is the only authority on their contents — so this is exactly
// the case concepts.md describes, where editing a managed Caddyfile by
// hand does not stick.
// managedFile is one file Wildstone owns outright: its correct contents
// are a pure function of the renderer and desired state, so nothing but
// repair asserts them.
type managedFile struct {
	path string
	want []byte
	// materialize asks the agent to resolve secret references in the
	// content before writing (protocol minor 9). Only files a third party
	// actually reads need it; Wildstone's own bookkeeping files keep
	// their references, which keeps their drift comparison a plain byte
	// compare and keeps secrets off disk in one more place.
	materialize bool
}

// nodePrereqFiles is the closed list of those files. It is a function
// rather than a literal inside repairNodePrereqs so a test can count and
// name them without restating the list — a second copy is exactly how a
// new prereq gets added and silently left out of the coverage that was
// supposed to bound it.
func nodePrereqFiles(managedPrefix string, desired []resource.Resource) []managedFile {
	clientsFile := renderAutheliaClients(desired)
	return []managedFile{
		{baseCaddyfilePath(managedPrefix), renderBaseCaddyfile(postureOf(desired)), false},
		{placeholderFragmentPath(managedPrefix), renderPlaceholderFragment(), false},
		// The merged Authelia clients file qualifies for the same reason
		// the other two do. Asserting it before observe also means it is
		// already correct by the time an OidcClient step's reload fires
		// later in the same tick.
		// Authelia reads this one, so its references are resolved on the
		// node — including the client secrets, which it stores as pbkdf2
		// digests. DERIVED, never a constant: with zero OidcClients the
		// renderer deliberately emits a comment-only file so Authelia
		// still boots, and the agent just as deliberately refuses a
		// materializing write with nothing to materialize. Both are
		// right; asserting `true` between them made the FIRST install
		// anyone performs fail, since identity is always installed
		// before anything that consumes it:
		//
		//	prereq write /etc/wildstone/authelia/clients.yml:
		//	ERROR_CODE_INVALID: materialize_secrets set but the content
		//	carries no secret reference
		{autheliaClientsPath(managedPrefix), clientsFile, configFileNeedsMaterialization(string(clientsFile))},
		// The merged LLDAP group list. A prereq rather than a per-step
		// render for the same reason: it must be correct on disk before
		// the provisioning job that reads it is planned, and its content
		// is what that job's spec hash is computed from. No secret
		// references, so no materialization.
		{lldapGroupsPath(managedPrefix), renderLdapGroups(desired), false},
		// The merged LLDAP user list, same category as the group list and
		// ordered after it so a membership's group is on disk first.
		{lldapUsersPath(managedPrefix), renderLdapUsers(desired), false},
		// The merged Authelia access-control block. Same category as the
		// clients file and for the same reason: Authelia reads it, and a
		// forward_auth emitted by a Route fragment is decided by a rule
		// that must already be on disk when the fragment goes live. No
		// secret references, so no materialization.
		{autheliaAccessPath(managedPrefix), renderAutheliaAccess(desired), false},
		// Homepage's service list. Same category again: the dashboard
		// container bind-mounts this file, so it must be on disk before
		// the container is created — which is exactly what a prereq
		// guarantees, and why a file mount is safe here (F55).
		{homepageServicesPath(managedPrefix), renderHomepageServices(desired), false},
	}
}

// prereqRepairedClients reports whether the merged Authelia clients file
// was among the prereqs rewritten this tick.
func prereqRepairedClients(repaired []string, managedPrefix string) bool {
	want := autheliaClientsPath(managedPrefix)
	for _, p := range repaired {
		if p == want {
			return true
		}
	}
	return false
}

func (r *Reconciler) repairNodePrereqs(ctx context.Context, nodeID string, desired []resource.Resource) ([]string, string) {
	if nodeID == "" || r.ManagedPrefix == "" {
		return nil, ""
	}
	files := nodePrereqFiles(r.ManagedPrefix, desired)
	var repaired []string
	for _, f := range files {
		res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ReadManagedFile{
			ReadManagedFile: &wsv1.ReadManagedFile{Path: f.path},
		}})
		if err != nil {
			return repaired, fmt.Sprintf("prereq read %s: %v", f.path, err)
		}
		oe := res.GetError()
		if oe != nil && oe.GetCode() != wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
			return repaired, fmt.Sprintf("prereq read %s: %s: %s", f.path, oe.GetCode(), oe.GetMessage())
		}
		// Absent counts as drifted: a missing base Caddyfile is the
		// fresh-install state, and writing it is what brings ingress up.
		if oe == nil && managedFileMatches(res.GetReadManagedFile(), f.want) {
			continue
		}
		wres, err := r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
			WriteManagedFile: &wsv1.WriteManagedFile{Path: f.path, Content: f.want, MaterializeSecrets: f.materialize},
		}})
		if err != nil {
			return repaired, fmt.Sprintf("prereq write %s: %v", f.path, err)
		}
		if we := wres.GetError(); we != nil {
			return repaired, fmt.Sprintf("prereq write %s: %s: %s", f.path, we.GetCode(), we.GetMessage())
		}
		repaired = append(repaired, f.path)
	}
	return repaired, ""
}

func (r *Reconciler) Reconcile(ctx context.Context, nodeID string, desired []resource.Resource) (*Outcome, error) {
	// The ingress posture is read once and used by every renderer this
	// tick, so a Route fragment and the base Caddyfile cannot disagree
	// about the scheme halfway through.
	r.mu.Lock()
	r.posture = postureOf(desired)
	r.mu.Unlock()

	wsDesired := make([]resource.Resource, 0, len(desired))
	providerDesired := make([]resource.Resource, 0, len(desired))
	for _, res := range desired {
		switch res.Kind {
		case resource.KindContainer, resource.KindVolume, resource.KindRoute, resource.KindOidcClient,
			resource.KindLdapGroup, resource.KindLdapUser, resource.KindIngress, resource.KindBackupPlan, resource.KindConfigFile, resource.KindJob,
			resource.KindSecret:
			wsDesired = append(wsDesired, res)
		case resource.KindDnsRecord:
			providerDesired = append(providerDesired, res)
		default:
			return nil, fmt.Errorf("reconcile: unknown kind %q", res.Kind)
		}
	}

	// A node-scoped resource that reached here with no node was bucketed
	// under "" by the loop, and there is no agent session at "" to observe
	// or apply it. resource.Validate refuses this at proposal time, so
	// reaching it means a resource was constructed in code and never
	// validated; say which one, rather than failing several frames later
	// as an opaque dispatch error naming no resource at all.
	if nodeID == "" {
		for _, res := range wsDesired {
			return nil, fmt.Errorf("reconcile: %s/%s is node-scoped but carries no metadata.node", res.Kind, res.Metadata.Name)
		}
	}

	// Before observing resources, re-assert the managed files that are not
	// resources and so have no observe rule of their own (F34).
	// Before the prereq files, because they can reference secrets: see
	// the ordering note in secret.go. A failure here is reported the same
	// way a failed prereq is, since it has the same cause and the same
	// consequence — nothing further on this node can be written.
	if serr := r.ensureNodeSecrets(ctx, nodeID, wsDesired); serr != "" {
		return nil, fmt.Errorf("reconcile: %s", serr)
	}

	prereqRepaired, prereqErr := r.repairNodePrereqs(ctx, nodeID, wsDesired)
	if prereqErr != "" {
		return nil, fmt.Errorf("reconcile: %s", prereqErr)
	}

	observed, foreign, err := r.observe(ctx, nodeID, wsDesired)
	if err != nil {
		return nil, fmt.Errorf("reconcile: observe: %w", err)
	}
	provObserved, err := r.observeProviders(ctx, providerDesired)
	if err != nil {
		return nil, fmt.Errorf("reconcile: observe (provider): %w", err)
	}

	allDesired := append(append([]resource.Resource{}, wsDesired...), providerDesired...)
	allObserved := append(append([]resource.Resource{}, observed...), provObserved...)

	p := plan.Diff(allDesired, allObserved)
	outcome := &Outcome{Plan: p, ForeignDrift: foreign, PrereqRepaired: prereqRepaired}
	if len(p.Steps) == 0 {
		outcome.Applied = true
		return outcome, nil
	}

	// ADR-W004 A3: Low auto-applies; Medium/High require explicit
	// approval, default deny. This gate is evaluated once for the whole
	// plan (OverallRisk = max over steps), not per step — a plan that
	// mixes a Low container create with a High volume delete is either
	// approved and applied in full or not applied at all.
	if p.OverallRisk > plan.RiskLow {
		if r.Approve == nil || !r.Approve(p) {
			outcome.PendingApproval = true
			return outcome, nil
		}
	}

	desiredByKey := make(map[resKey]resource.Resource, len(allDesired))
	for _, res := range allDesired {
		desiredByKey[resKey{res.Kind, res.Metadata.Name}] = res
	}
	observedByKey := make(map[resKey]resource.Resource, len(allObserved))
	for _, res := range allObserved {
		observedByKey[resKey{res.Kind, res.Metadata.Name}] = res
	}

	// ADR-W005 A3/B3 batching: track whether ANY OidcClient step in this
	// plan Applied, and — regardless of which return path below is taken
	// (full completion, an Incident halt, or a Rolledback halt) — invoke
	// the batched Authelia reload seam at most once, after every step this
	// call attempted. Using defer means every return in the loop below is
	// covered without duplicating the call at each return site; a plan
	// with zero OidcClient steps never invokes the seam at all (the flag
	// stays false), and a plan with N OidcClient Create/Update steps that
	// all applied invokes it exactly once, not N times — the exact trap
	// ADR-W005 flags.
	// A repair of the merged clients file counts as an OIDC change for
	// reload purposes: the file Authelia reads was wrong and is now
	// right, and Authelia will not notice until it reloads. Without this,
	// a hand-edited clients file is silently restored on disk while the
	// running provider keeps serving the edit.
	anyOidcApplied := prereqRepairedClients(prereqRepaired, r.ManagedPrefix)
	defer func() {
		if !anyOidcApplied {
			return
		}
		if err := r.runAutheliaReload(ctx); err != nil && outcome.Incident == "" && !outcome.Rolledback {
			// The fragment writes themselves already succeeded and passed
			// their own health gates (fragment present+equal) — only the
			// batched reload failed. Surface it as an Incident rather than
			// silently dropping it, but don't clobber a more specific
			// Incident/Rolledback reason an earlier step already set.
			outcome.Incident = fmt.Sprintf("authelia batched reload: %s", err)
			outcome.Applied = false
		}
	}()

	// Hoisted out of the loop: one plan can carry many Container steps,
	// and the answer is the same for all of them.
	cfgContents := managedConfigContents(r.ManagedPrefix, desiredByKey)

	for _, step := range p.Steps {
		var sr StepResult
		switch step.Kind {
		case resource.KindContainer:
			sr = r.applyContainerStep(ctx, nodeID, step, desiredByKey, cfgContents)
		case resource.KindJob:
			sr = r.applyJobStep(ctx, nodeID, step, desiredByKey, cfgContents)
		case resource.KindSecret:
			sr = r.applySecretStep(ctx, nodeID, step, desiredByKey)
		case resource.KindVolume:
			sr = r.applyVolumeStep(ctx, nodeID, step)
		case resource.KindRoute:
			sr = r.applyRouteStep(ctx, nodeID, step, desiredByKey)
		case resource.KindDnsRecord:
			sr = r.applyDnsRecordStep(ctx, step, desiredByKey, observedByKey)
		case resource.KindOidcClient:
			sr = r.applyOidcClientStep(ctx, nodeID, step, desiredByKey)
			if sr.Applied {
				anyOidcApplied = true
			}
		case resource.KindLdapGroup:
			sr = r.applyLdapGroupStep(ctx, nodeID, step, desiredByKey)
		case resource.KindLdapUser:
			sr = r.applyLdapUserStep(ctx, nodeID, step, desiredByKey)
		case resource.KindIngress:
			sr = r.applyIngressStep(ctx, nodeID, step, desiredByKey)
		case resource.KindBackupPlan:
			sr = r.applyBackupPlanStep(ctx, nodeID, step, desiredByKey)
		case resource.KindConfigFile:
			sr = r.applyConfigFileStep(ctx, nodeID, step, desiredByKey)
		default:
			sr = StepResult{Step: step, Error: fmt.Sprintf("reconcile: kind %s not handled by the Applier/Provider path", step.Kind)}
		}
		outcome.Steps = append(outcome.Steps, sr)

		if !sr.Applied {
			outcome.Incident = fmt.Sprintf("%s %s/%s: %s", step.Action, step.Kind, step.Name, sr.Error)
			return outcome, nil
		}
		if !sr.HealthOK {
			outcome.Rolledback = true
			return outcome, nil
		}
	}

	outcome.Applied = true
	return outcome, nil
}

// ---------------------------------------------------------------- observe

func (r *Reconciler) observe(ctx context.Context, nodeID string, desired []resource.Resource) (observed []resource.Resource, foreign []string, err error) {
	desiredByKey := make(map[resKey]resource.Resource, len(desired))
	for _, res := range desired {
		desiredByKey[resKey{res.Kind, res.Metadata.Name}] = res
	}
	// Computed here rather than passed in, from the same pure function
	// the apply path uses on the same desired set: observe and apply must
	// agree on what a container's configuration is, or every tick sees
	// drift it just finished fixing.
	cfgContents := managedConfigContents(r.ManagedPrefix, desiredByKey)

	// Candidates = every desired (kind,name) (to detect Create/Update/
	// NoOp) union every key the Reconciler currently believes it manages
	// (to detect a legitimate Delete: something we made that's no longer
	// desired). A key present in neither set was never ours and is never
	// considered.
	candidates := make(map[resKey]bool, len(desiredByKey))
	for k := range desiredByKey {
		candidates[k] = true
	}
	for k := range r.managedKeys(nodeID) {
		candidates[k] = true
	}

	for k := range candidates {
		want, isDesired := desiredByKey[k]
		switch k.Kind {
		case resource.KindSecret:
			if obs, present := r.observeSecret(nodeID, k, want, isDesired); present {
				observed = append(observed, obs)
			}
		case resource.KindJob:
			obs, present, oerr := r.observeJob(ctx, nodeID, k, want, isDesired, cfgContents)
			if oerr != nil {
				return nil, nil, oerr
			}
			if present {
				observed = append(observed, obs)
			}
		case resource.KindContainer:
			res, obs, oerr := r.observeContainer(ctx, nodeID, k, want, isDesired, cfgContents)
			if oerr != nil {
				return nil, nil, oerr
			}
			if res == nil {
				continue
			}
			if res.foreign {
				foreign = append(foreign, k.Name)
				continue
			}
			observed = append(observed, obs)
		case resource.KindVolume:
			if !r.isManaged(nodeID, k) {
				continue // never observed/applied; absent
			}
			if isDesired {
				observed = append(observed, want)
			} else {
				observed = append(observed, placeholderResource(resource.KindVolume, k.Name))
			}
		case resource.KindRoute:
			obs, present, oerr := r.observeRoute(ctx, nodeID, k, want, isDesired)
			if oerr != nil {
				return nil, nil, oerr
			}
			if present {
				observed = append(observed, obs)
			}
		case resource.KindOidcClient:
			obs, present, oerr := r.observeOidcClient(ctx, nodeID, k, want, isDesired)
			if oerr != nil {
				return nil, nil, oerr
			}
			if present {
				observed = append(observed, obs)
			}
		case resource.KindConfigFile:
			obs, present, oerr := r.observeConfigFile(ctx, nodeID, k, want, isDesired)
			if oerr != nil {
				return nil, nil, oerr
			}
			if present {
				observed = append(observed, obs)
			}
		case resource.KindLdapGroup, resource.KindLdapUser, resource.KindIngress, resource.KindBackupPlan:
			// Declared-only kinds (ADR-W005 A3 / ADR-W011 A4):
			// "observe-by-apply", same mechanism as Volume — present iff a
			// prior successful apply recorded it in the Reconciler's
			// managed cache. No external call is ever made; for BackupPlan
			// the backup engine (keystone/backup) is what acts on the
			// declaration, not the reconciler.
			if !r.isManaged(nodeID, k) {
				continue // never applied; absent
			}
			if isDesired {
				observed = append(observed, want)
			} else {
				observed = append(observed, placeholderResource(k.Kind, k.Name))
			}
		default:
			return nil, nil, fmt.Errorf("observe: kind %s not handled by S2b/S3a (DnsRecord is S2c's Provider path)", k.Kind)
		}
	}
	return observed, foreign, nil
}

type containerObserveResult struct {
	foreign bool
}

// observeContainer implements the Container observe rule (ADR-W008 R2c
// closes the S2b presence-vs-spec gap): InspectContainer returns the
// container's labels, and the spec-hash label stamped at EnsureContainer
// is compared against a fresh hash of the desired spec — equal means the
// running container matches desired (NoOp), different means SPEC drift
// (an Update is planned by returning a deliberately-unequal placeholder,
// the observeRoute pattern). NOT_FOUND = absent; FORBIDDEN = foreign
// (never planned as a delete, and never matched against a desired create
// either — a name collision with a foreign object is left for a human).
// An INVALID reply means the agent predates the op (protocol minor 5):
// fall back to the pre-R2c presence-only ContainerStats observe so an
// N−1 agent keeps its exact previous behavior.
func (r *Reconciler) observeContainer(ctx context.Context, nodeID string, k resKey, want resource.Resource, isDesired bool, cfgContents map[string]string) (*containerObserveResult, resource.Resource, error) {
	res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{
		InspectContainer: &wsv1.InspectContainer{Name: k.Name},
	}})
	if err != nil {
		return nil, resource.Resource{}, fmt.Errorf("observe container %s: %w", k.Name, err)
	}
	if oe := res.GetError(); oe != nil {
		switch oe.GetCode() {
		case wsv1.ErrorCode_ERROR_CODE_FORBIDDEN:
			return &containerObserveResult{foreign: true}, resource.Resource{}, nil
		case wsv1.ErrorCode_ERROR_CODE_NOT_FOUND:
			return nil, resource.Resource{}, nil // absent
		case wsv1.ErrorCode_ERROR_CODE_INVALID:
			return r.observeContainerPresence(ctx, nodeID, k, want, isDesired)
		default:
			return nil, resource.Resource{}, fmt.Errorf("observe container %s: %s: %s", k.Name, oe.GetCode(), oe.GetMessage())
		}
	}
	if !isDesired {
		return &containerObserveResult{}, placeholderResource(resource.KindContainer, k.Name), nil
	}
	inspect := res.GetInspectContainer()
	if inspect.GetLabels()[specHashLabel] == specHash(want.Container, ContainerNetwork, configDigest(r.ManagedPrefix, want.Container, cfgContents)) && inspect.GetRunning() {
		return &containerObserveResult{}, want, nil
	}
	// Present but stale (spec drifted, or a pre-spec-hash container) or
	// stopped: plan an Update.
	return &containerObserveResult{}, placeholderResource(resource.KindContainer, k.Name), nil
}

// observeContainerPresence is the pre-R2c presence-only observe, kept as
// the N−1 fallback: ContainerStats success = present (conflating running
// with spec-matching — the documented S2b limitation this path preserves
// for agents that don't speak InspectContainer yet).
func (r *Reconciler) observeContainerPresence(ctx context.Context, nodeID string, k resKey, want resource.Resource, isDesired bool) (*containerObserveResult, resource.Resource, error) {
	res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ContainerStats{
		ContainerStats: &wsv1.ContainerStats{Name: k.Name},
	}})
	if err != nil {
		return nil, resource.Resource{}, fmt.Errorf("observe container %s: %w", k.Name, err)
	}
	if oe := res.GetError(); oe != nil {
		switch oe.GetCode() {
		case wsv1.ErrorCode_ERROR_CODE_FORBIDDEN:
			return &containerObserveResult{foreign: true}, resource.Resource{}, nil
		case wsv1.ErrorCode_ERROR_CODE_NOT_FOUND:
			return nil, resource.Resource{}, nil // absent
		default:
			return nil, resource.Resource{}, fmt.Errorf("observe container %s: %s: %s", k.Name, oe.GetCode(), oe.GetMessage())
		}
	}
	if isDesired {
		return &containerObserveResult{}, want, nil
	}
	return &containerObserveResult{}, placeholderResource(resource.KindContainer, k.Name), nil
}

// observeRoute implements the Route observe rule: ReadManagedFile the
// rendered fragment path; NOT_FOUND = absent; present+byte-equal to a
// fresh render of the desired spec = NoOp; present+different = Update
// (signaled by returning a deliberately-unequal placeholder spec, since
// the actual live RouteSpec fields can't be recovered from rendered
// Caddy text — only the fact that it's stale can be, which is all
// plan.Diff needs to emit an Update step).
func (r *Reconciler) observeRoute(ctx context.Context, nodeID string, k resKey, want resource.Resource, isDesired bool) (resource.Resource, bool, error) {
	path := routeFragmentPath(r.ManagedPrefix, k.Name)
	res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ReadManagedFile{
		ReadManagedFile: &wsv1.ReadManagedFile{Path: path},
	}})
	if err != nil {
		return resource.Resource{}, false, fmt.Errorf("observe route %s: %w", k.Name, err)
	}
	if oe := res.GetError(); oe != nil {
		if oe.GetCode() == wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
			return resource.Resource{}, false, nil
		}
		return resource.Resource{}, false, fmt.Errorf("observe route %s: %s: %s", k.Name, oe.GetCode(), oe.GetMessage())
	}
	if !isDesired {
		return placeholderResource(resource.KindRoute, k.Name), true, nil
	}
	if managedFileMatches(res.GetReadManagedFile(), renderRouteFragment(want.Route, r.autheliaAddr(), r.currentPosture())) {
		return want, true, nil
	}
	return placeholderResource(resource.KindRoute, k.Name), true, nil
}

// observeOidcClient implements the OidcClient observe rule (ADR-W005 A3):
// ReadManagedFile the rendered Authelia fragment; NOT_FOUND = absent;
// present+byte-equal to a fresh render of the desired spec = NoOp;
// present+different = Update (signaled the same way observeRoute signals
// it: a deliberately-unequal placeholder spec, since the real fragment
// text can't be reversed back into an OidcClientSpec — only "stale" can
// be, which is all plan.Diff needs).
func (r *Reconciler) observeOidcClient(ctx context.Context, nodeID string, k resKey, want resource.Resource, isDesired bool) (resource.Resource, bool, error) {
	fragPath := oidcClientFragmentPath(r.ManagedPrefix, k.Name)
	res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ReadManagedFile{
		ReadManagedFile: &wsv1.ReadManagedFile{Path: fragPath},
	}})
	if err != nil {
		return resource.Resource{}, false, fmt.Errorf("observe oidc client %s: %w", k.Name, err)
	}
	if oe := res.GetError(); oe != nil {
		if oe.GetCode() == wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
			return resource.Resource{}, false, nil
		}
		return resource.Resource{}, false, fmt.Errorf("observe oidc client %s: %s: %s", k.Name, oe.GetCode(), oe.GetMessage())
	}
	if !isDesired {
		return placeholderResource(resource.KindOidcClient, k.Name), true, nil
	}
	if managedFileMatches(res.GetReadManagedFile(), renderOidcClientFragment(want.OidcClient)) {
		return want, true, nil
	}
	return placeholderResource(resource.KindOidcClient, k.Name), true, nil
}

// managedFileMatches decides whether a ReadManagedFile result represents
// the content we intended to write.
//
// Two cases, and the second is the whole reason this is a function. A
// plain managed file comes back as bytes and is compared byte for byte.
// A MATERIALIZED file does not come back at all: its bytes on disk hold
// resolved secret values, which the agent deliberately withholds, and
// sends the sha256 of the pre-materialization content instead. That hash
// is the only thing Keystone can compare against, because Keystone holds
// no secrets and so cannot compute what the file should look like.
//
// Getting this wrong is not subtle in effect but is silent in cause:
// comparing rendered bytes against a materialized file never matches, so
// the reconciler would rewrite it on every single tick, forever, and
// report drift it had just repaired.
func managedFileMatches(res *wsv1.ReadManagedFileResult, want []byte) bool {
	if src := res.GetSourceSha256Hex(); src != "" {
		sum := sha256.Sum256(want)
		return src == hex.EncodeToString(sum[:])
	}
	return routeFragmentEqual(res.GetContent(), want)
}

func routeFragmentEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// placeholderResource builds a minimal, validly-typed resource used purely
// as an observed-side stand-in when the real live spec can't be recovered
// (Container/Route "something exists here" signals) or isn't needed
// (Volume, whose spec is just its name). Its zero-value inner spec is
// guaranteed to compare unequal to any validly-populated desired spec of
// the same kind (see resource.Validate: Container.Image and Route.Host/
// Classification can never be empty), so plan.Diff's specEqual always
// treats a placeholder as "changed" when compared against a desired
// resource — exactly the "force an Update, or allow a Delete" behavior
// observe needs from it.
func placeholderResource(kind resource.Kind, name string) resource.Resource {
	r := resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       kind,
		Metadata:   resource.Metadata{Name: name, Owner: "system"},
	}
	switch kind {
	case resource.KindContainer:
		r.Container = &resource.ContainerSpec{}
	case resource.KindSecret:
		r.Secret = &resource.SecretSpec{}
	case resource.KindJob:
		// Deliberately unequal to any real desired Job: an empty Run has
		// no image, and containerEqual compares images first.
		r.Job = &resource.JobSpec{}
	case resource.KindVolume:
		r.Volume = &resource.VolumeSpec{Name: name}
	case resource.KindRoute:
		r.Route = &resource.RouteSpec{}
	case resource.KindOidcClient:
		r.OidcClient = &resource.OidcClientSpec{}
	case resource.KindLdapGroup:
		r.LdapGroup = &resource.LdapGroupSpec{Name: name}
	case resource.KindLdapUser:
		r.LdapUser = &resource.LdapUserSpec{Username: name}
	case resource.KindIngress:
		r.Ingress = &resource.IngressSpec{}
	case resource.KindBackupPlan:
		r.BackupPlan = &resource.BackupPlanSpec{}
	case resource.KindConfigFile:
		r.ConfigFile = &resource.ConfigFileSpec{}
	}
	return r
}

// ---------------------------------------------------------------- observe: providers

// observeProviders observes every provider-backed desired resource
// (currently just DnsRecord, ADR-W004 A5) via Providers[kind].Observe,
// returning only those confirmed to exist (diff treats an absent one as a
// Create candidate the normal way). A *provider.RateLimited or
// *provider.Transient error is retried by retryProvider like any other
// transient failure; if retries are exhausted the error is returned to
// the caller, which surfaces it as an Outcome via the normal error path
// (Reconcile itself errors out here, same as a Wardstone observe
// transport failure would).
func (r *Reconciler) observeProviders(ctx context.Context, desired []resource.Resource) ([]resource.Resource, error) {
	var observed []resource.Resource
	for _, want := range desired {
		p, ok := r.Providers[want.Kind]
		if !ok || p == nil {
			return nil, fmt.Errorf("observe: no provider configured for kind %s", want.Kind)
		}
		var obs *resource.Resource
		var exists bool
		err := r.retryProvider(ctx, func() error {
			var innerErr error
			obs, exists, innerErr = p.Observe(ctx, want)
			return innerErr
		})
		if err != nil {
			return nil, fmt.Errorf("observe %s/%s: %w", want.Kind, want.Metadata.Name, err)
		}
		if exists && obs != nil {
			observed = append(observed, *obs)
		}
	}
	return observed, nil
}

// ---------------------------------------------------------------- apply: DnsRecord

// applyDnsRecordStep routes a DnsRecord PlanStep through
// Providers[resource.KindDnsRecord] (ADR-W004 A5). Create/Update use the
// desired spec; Delete uses the observed spec (the resource is no longer
// in desired by construction, so its identity — zone+name+type — must come
// from what was last observed).
func (r *Reconciler) applyDnsRecordStep(ctx context.Context, step plan.PlanStep, desiredByKey, observedByKey map[resKey]resource.Resource) StepResult {
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}

	p, ok := r.Providers[resource.KindDnsRecord]
	if !ok || p == nil {
		sr.Error = "no provider configured for DnsRecord"
		return sr
	}

	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		want, ok := desiredByKey[key]
		if !ok || want.DnsRecord == nil {
			sr.Error = "internal: no desired DnsRecord spec for " + step.Name
			return sr
		}
		var callErr error
		if step.Action == plan.ActionCreate {
			callErr = r.retryProvider(ctx, func() error { return p.Create(ctx, want) })
		} else {
			callErr = r.retryProvider(ctx, func() error { return p.Update(ctx, want) })
		}
		if callErr != nil {
			sr.Error = callErr.Error()
			return sr
		}
		sr.Applied = true

		// Health gate (ADR-W004 A4 step 5 / A5): a provider read-back
		// confirming the live record now matches the desired spec.
		if r.waitDnsRecordConverged(ctx, p, want) {
			sr.HealthOK = true
			return sr
		}
		// No reverse is defined for DnsRecord (ADR-W004 doesn't specify
		// one, and unlike Route there's no "prior fragment bytes" to
		// restore — the provider's own external state IS the prior
		// state). Surface the gate failure via Outcome.Rolledback without
		// attempting a reverse, same posture as Volume's Create gate
		// failure.
		return sr

	case plan.ActionDelete:
		have, ok := observedByKey[key]
		if !ok || have.DnsRecord == nil {
			sr.Error = "internal: no observed DnsRecord spec for " + step.Name
			return sr
		}
		callErr := r.retryProvider(ctx, func() error { return p.Delete(ctx, have) })
		if callErr != nil {
			sr.Error = callErr.Error()
			return sr
		}
		sr.Applied = true
		sr.HealthOK = r.waitDnsRecordAbsent(ctx, p, have)
		return sr

	default:
		sr.Error = "unexpected action " + step.Action.String()
		return sr
	}
}

// waitDnsRecordConverged health-gates a DnsRecord Create/Update: polls
// Observe until the live record matches want's spec, bounded by the same
// health-gate attempts/backoff as the other kinds' gates.
func (r *Reconciler) waitDnsRecordConverged(ctx context.Context, p provider.Provider, want resource.Resource) bool {
	attempts, backoff := r.healthGateParams()
	for i := 0; i < attempts; i++ {
		obs, exists, err := p.Observe(ctx, want)
		if err == nil && exists && obs.DnsRecord != nil && *obs.DnsRecord == *want.DnsRecord {
			return true
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(backoff):
			}
		}
	}
	return false
}

// waitDnsRecordAbsent health-gates a DnsRecord Delete: polls Observe until
// the record is confirmed gone.
func (r *Reconciler) waitDnsRecordAbsent(ctx context.Context, p provider.Provider, have resource.Resource) bool {
	attempts, backoff := r.healthGateParams()
	for i := 0; i < attempts; i++ {
		_, exists, err := p.Observe(ctx, have)
		if err == nil && !exists {
			return true
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(backoff):
			}
		}
	}
	return false
}

// retryProvider retries fn up to MaxApplyRetries times (same bound/backoff
// as retryDispatch) as long as fn's error is a *provider.RateLimited
// (ADR-W004 A5: "the reconciler treats [it] as retryable-later, not
// fatal") or a *provider.Transient (a retryable server-side blip, same
// treatment). Any other error returns immediately without retry — a
// provider's non-rate-limit, non-transient failure (e.g. a genuinely
// invalid record) is not assumed retryable.
func (r *Reconciler) retryProvider(ctx context.Context, fn func() error) error {
	maxRetries := r.MaxApplyRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	backoff := r.RetryBackoff
	if backoff <= 0 {
		backoff = 50 * time.Millisecond
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if !provider.IsRateLimited(err) && !provider.IsTransient(err) {
			return err
		}
		lastErr = err
		if attempt < maxRetries-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
	}
	return lastErr
}

// ---------------------------------------------------------------- managed cache

func (r *Reconciler) markManaged(nodeID string, k resKey, present bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.managed == nil {
		r.managed = map[string]map[resKey]bool{}
	}
	m := r.managed[nodeID]
	if m == nil {
		m = map[resKey]bool{}
		r.managed[nodeID] = m
	}
	if present {
		m[k] = true
	} else {
		delete(m, k)
	}
}

func (r *Reconciler) rememberConfigFilePath(nodeID string, k resKey, filePath string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.configFilePaths == nil {
		r.configFilePaths = map[string]map[resKey]string{}
	}
	m := r.configFilePaths[nodeID]
	if m == nil {
		m = map[resKey]string{}
		r.configFilePaths[nodeID] = m
	}
	m[k] = filePath
}

func (r *Reconciler) forgetConfigFilePath(nodeID string, k resKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.configFilePaths[nodeID], k)
}

func (r *Reconciler) configFilePathFor(nodeID string, k resKey) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.configFilePaths[nodeID][k]
	return p, ok
}

func (r *Reconciler) isManaged(nodeID string, k resKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.managed[nodeID][k]
}

func (r *Reconciler) managedKeys(nodeID string) map[resKey]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[resKey]bool, len(r.managed[nodeID]))
	for k := range r.managed[nodeID] {
		out[k] = true
	}
	return out
}

// ManagedNodes returns every node ID the Reconciler currently tracks at
// least one managed resource on. The E3 service loop (ADR-W008 A3) uses
// it so a node whose desired set became EMPTY still gets a reconcile
// pass — otherwise pending deletions on that node would never be planned,
// because the loop groups passes by the nodes present in desired.
func (r *Reconciler) ManagedNodes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.managed))
	for node, keys := range r.managed {
		if len(keys) > 0 {
			out = append(out, node)
		}
	}
	return out
}

// ---------------------------------------------------------------- apply: Container

func (r *Reconciler) applyContainerStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource, cfgContents map[string]string) StepResult {
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}

	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		want, ok := desiredByKey[key]
		if !ok || want.Container == nil {
			sr.Error = "internal: no desired Container spec for " + step.Name
			return sr
		}
		// Pull before ensure: the agent's ensureContainer deliberately
		// never pulls (closed op surface — creation and image fetching are
		// separate audited operations), and digest-pinned refs are never
		// resolved implicitly by Docker, so without this a container can
		// only ever start on a node that already has the exact digest
		// cached. Found by the first real VM install (friction F-pull).
		// Network before pull before ensure: the agent refuses to attach
		// to a network that does not exist, and refuses to create a
		// container whose image is absent.
		if ne := r.ensureNodePrereqs(ctx, nodeID); ne != "" {
			sr.Error = ne
			return sr
		}
		if pr := r.pullImage(ctx, nodeID, want.Container.Image); pr != "" {
			sr.Error = pr
			return sr
		}
		res, err := r.retryDispatch(ctx, nodeID, containerEnsureOp(step.Name, want.Container, ContainerNetwork, configDigest(r.ManagedPrefix, want.Container, cfgContents)))
		if err != nil {
			sr.Error = err.Error()
			return sr
		}
		if oe := res.GetError(); oe != nil {
			sr.Error = fmt.Sprintf("%s: %s", oe.GetCode(), oe.GetMessage())
			return sr
		}
		sr.Applied = true

		if r.waitContainerRunning(ctx, nodeID, step.Name) {
			sr.HealthOK = true
			r.markManaged(nodeID, key, true)
			return sr
		}
		r.reverseContainer(ctx, nodeID, step)
		sr.Reversed = true
		return sr

	case plan.ActionDelete:
		// StopContainer then RemoveContainer (ADR-W004 A4 step 4). Best
		// effort on the stop: a container that's already stopped or
		// foreign-refused on stop still gets a RemoveContainer attempt,
		// whose own ownership check is authoritative.
		r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_StopContainer{ //nolint:errcheck
			StopContainer: &wsv1.StopContainer{Name: step.Name},
		}})
		res, err := r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_RemoveContainer{
			RemoveContainer: &wsv1.RemoveContainer{Name: step.Name, Force: true},
		}})
		if err != nil {
			sr.Error = err.Error()
			return sr
		}
		if oe := res.GetError(); oe != nil {
			sr.Error = fmt.Sprintf("%s: %s", oe.GetCode(), oe.GetMessage())
			return sr
		}
		sr.Applied = true
		sr.HealthOK = !r.containerRunning(ctx, nodeID, step.Name) // gate: confirms it's actually gone
		r.markManaged(nodeID, key, false)
		return sr

	default:
		sr.Error = "unexpected action " + step.Action.String()
		return sr
	}
}

// reverseContainer implements the Container reverse rules from ADR-W004
// A4 step 5. Create's reverse is unambiguous (Stop+Remove what was just
// made). Update's documented reverse is "re-Ensure prior image if known
// else Stop" — this Reconciler's Container observe is presence-only (see
// the package doc), so the prior image is never known here, and Update
// always takes the "else Stop" branch. Flagged as a limitation in the S2b
// report: true image-aware rollback needs an inspect-style op the current
// Wardstone surface doesn't expose.
func (r *Reconciler) reverseContainer(ctx context.Context, nodeID string, step plan.PlanStep) {
	if step.Action == plan.ActionCreate {
		r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_StopContainer{ //nolint:errcheck
			StopContainer: &wsv1.StopContainer{Name: step.Name},
		}})
		r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_RemoveContainer{ //nolint:errcheck
			RemoveContainer: &wsv1.RemoveContainer{Name: step.Name, Force: true},
		}})
		return
	}
	r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_StopContainer{ //nolint:errcheck
		StopContainer: &wsv1.StopContainer{Name: step.Name},
	}})
}

func (r *Reconciler) containerRunning(ctx context.Context, nodeID, name string) bool {
	res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ContainerStats{
		ContainerStats: &wsv1.ContainerStats{Name: name},
	}})
	return err == nil && res.GetError() == nil
}

func (r *Reconciler) waitContainerRunning(ctx context.Context, nodeID, name string) bool {
	attempts, backoff := r.healthGateParams()
	for i := 0; i < attempts; i++ {
		if r.containerRunning(ctx, nodeID, name) {
			return true
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(backoff):
			}
		}
	}
	return false
}

// specHashLabel mirrors the agent's SpecHashLabel constant (a small
// duplicated constant on each side rather than a shared module dep, the
// opSignDomainPrefix pattern — ADR-W003 §A4.5 keeps wardstone's
// dependency surface minimal, and keystone cannot import
// wardstone/internal anyway). Keep in sync with
// wardstone/internal/agent/ops.go.
const specHashLabel = "dev.wildstone.spec-hash"

// specHash computes the deterministic hash of a full container spec that
// rides the specHashLabel (ADR-W008 R2c): the agent compares it to decide
// replacement, and observe compares it to detect spec drift. JSON of the
// spec struct is deterministic here because ContainerSpec's only map
// (Env) is marshaled with sorted keys by encoding/json. Truncated to 16
// hex chars: enough to make collision odds irrelevant for one homelab's
// container count, and deliberately short of the 32-hex shape the
// redaction layer treats as secret-like.
// ContainerNetwork is the user-defined Docker network every managed
// container is attached to (protocol minor 8). It exists so containers
// can reach each other by name: Docker's default bridge has no embedded
// DNS, which is why the "container/<name>:<port>" upstreams packages
// declare had nothing behind them (#40). The agent refuses to attach to
// a network it did not create, so this is always one EnsureDockerNetwork
// made.
const ContainerNetwork = "wildstone"

// DefaultAutheliaAddr is where forward-auth is sent when Reconciler
// .AutheliaAddr is unset: the identity package's own Authelia container,
// resolved by name on ContainerNetwork.
const DefaultAutheliaAddr = "authelia:9091"

func (r *Reconciler) autheliaAddr() string {
	if r.AutheliaAddr != "" {
		return r.AutheliaAddr
	}
	return DefaultAutheliaAddr
}

// specHash covers the network as well as the spec so that attaching an
// existing fleet to ContainerNetwork for the first time actually replaces
// those containers. The network is not part of ContainerSpec, so hashing
// the spec alone would leave every pre-minor-8 container running
// unattached with a matching hash — converged on paper, unreachable by
// name in fact.
//
// cfgDigest extends that reasoning to the managed configuration files a
// container mounts, which are not part of ContainerSpec either: without
// it, editing an app's config leaves the container running the bytes it
// read at start, with a matching hash. See configdigest.go for what is
// included and what is deliberately not.
//
// An empty cfgDigest contributes NOTHING to the hash, rather than
// hashing the empty string: that keeps every container which reads no
// managed config on the hash it already has, so this dimension replaces
// only the containers it actually applies to.
func specHash(spec *resource.ContainerSpec, network, cfgDigest string) string {
	data, err := json.Marshal(spec)
	if err != nil {
		// A ContainerSpec is plain data; Marshal cannot fail on it. Guard
		// anyway: an empty hash never matches, which degrades to "always
		// replace" — converging, never wedging.
		return ""
	}
	data = append(data, []byte("\x00net="+network)...)
	if cfgDigest != "" {
		data = append(data, []byte("\x00cfg="+cfgDigest)...)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// ensureNodePrereqs asserts the two things every managed container needs
// to exist before it does, returning "" on success or the error text.
//
//   - ContainerNetwork, so the container can be attached and reached by
//     name. The agent refuses to attach to a network it did not create,
//     so this must run first.
//   - The base Caddyfile, so an ingress container has a configuration to
//     start with. It is written here rather than alongside the route
//     fragments because Caddy needs it even when zero Routes exist yet —
//     a fresh install brings up ingress before any app.
//
// Both are idempotent, so like pullImage this runs before every container
// ensure rather than tracking per-node state. It is NOT drift repair —
// this comment used to claim it was, which was wrong in the way that
// matters: it runs only on the container-create path, so a hand-edited
// base Caddyfile stayed edited until someone happened to deploy a
// container (F34). repairNodePrereqs handles the tick.
func (r *Reconciler) ensureNodePrereqs(ctx context.Context, nodeID string) string {
	res, err := r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_EnsureDockerNetwork{
		EnsureDockerNetwork: &wsv1.EnsureDockerNetwork{Name: ContainerNetwork},
	}})
	if err != nil {
		return "network " + ContainerNetwork + ": " + err.Error()
	}
	if oe := res.GetError(); oe != nil {
		return fmt.Sprintf("network %s: %s: %s", ContainerNetwork, oe.GetCode(), oe.GetMessage())
	}
	if r.ManagedPrefix == "" {
		return ""
	}
	res, err = r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
		WriteManagedFile: &wsv1.WriteManagedFile{
			Path: baseCaddyfilePath(r.ManagedPrefix), Content: renderBaseCaddyfile(r.currentPosture()),
		},
	}})
	if err != nil {
		return "base Caddyfile: " + err.Error()
	}
	if oe := res.GetError(); oe != nil {
		return fmt.Sprintf("base Caddyfile: %s: %s", oe.GetCode(), oe.GetMessage())
	}
	// A fresh install has no Routes yet, and an import glob matching
	// nothing warns once a second under --watch (F30). Seed one
	// non-empty fragment so the glob always matches.
	res, err = r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
		WriteManagedFile: &wsv1.WriteManagedFile{
			Path: placeholderFragmentPath(r.ManagedPrefix), Content: renderPlaceholderFragment(),
		},
	}})
	if err != nil {
		return "routes placeholder: " + err.Error()
	}
	if oe := res.GetError(); oe != nil {
		return fmt.Sprintf("base Caddyfile: %s: %s", oe.GetCode(), oe.GetMessage())
	}
	return ""
}

// pullImage dispatches a PullImage for image on nodeID, returning "" on
// success or the error text. Pulling an already-present digest is a cheap
// no-op on the Docker side, so this runs unconditionally before every
// container ensure rather than trying to guess cache state.
func (r *Reconciler) pullImage(ctx context.Context, nodeID, image string) string {
	res, err := r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_PullImage{
		PullImage: &wsv1.PullImage{Image: image},
	}})
	if err != nil {
		return "pull " + image + ": " + err.Error()
	}
	if oe := res.GetError(); oe != nil {
		return fmt.Sprintf("pull %s: %s: %s", image, oe.GetCode(), oe.GetMessage())
	}
	return ""
}

func containerEnsureOp(name string, spec *resource.ContainerSpec, network, cfgDigest string) *wsv1.Operation {
	var ports []*wsv1.PortBinding
	for _, p := range spec.Ports {
		hostIP, hostPort, containerPort, ok := resource.SplitPortMapping(p)
		if !ok {
			continue // malformed mapping; not this package's job to re-validate
		}
		ports = append(ports, &wsv1.PortBinding{HostIp: hostIP, HostPort: hostPort, ContainerPort: containerPort})
	}
	var mounts []*wsv1.Mount
	for _, m := range spec.Mounts {
		mounts = append(mounts, &wsv1.Mount{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	return &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{EnsureContainer: &wsv1.EnsureContainer{
		Name: name, Image: spec.Image, Env: spec.Env, Ports: ports, Mounts: mounts, RestartPolicy: spec.Restart,
		Command: spec.Command,
		Network: network,
		Labels:  map[string]string{specHashLabel: specHash(spec, network, cfgDigest)},
	}}}
}

// ---------------------------------------------------------------- apply: Volume

func (r *Reconciler) applyVolumeStep(ctx context.Context, nodeID string, step plan.PlanStep) StepResult {
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}

	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		res, err := r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_EnsureVolume{
			EnsureVolume: &wsv1.EnsureVolume{Name: step.Name},
		}})
		if err != nil {
			sr.Error = err.Error()
			return sr
		}
		if oe := res.GetError(); oe != nil {
			sr.Error = fmt.Sprintf("%s: %s", oe.GetCode(), oe.GetMessage())
			return sr
		}
		sr.Applied = true

		// Health gate (ADR-W004 A4 step 5): "Volume: EnsureVolume
		// idempotent success" — EnsureVolume is idempotent by
		// construction, so the gate is simply calling it again and
		// confirming it still succeeds.
		gateRes, gateErr := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_EnsureVolume{
			EnsureVolume: &wsv1.EnsureVolume{Name: step.Name},
		}})
		if gateErr == nil && gateRes.GetError() == nil {
			sr.HealthOK = true
			r.markManaged(nodeID, key, true)
			return sr
		}
		// Gate failure: there is no volume-delete op on the Wardstone
		// surface to reverse a Create with (only EnsureVolume exists),
		// and a delete would be exactly the data-affecting auto-action
		// this whole path exists to avoid. Surface the failure (HealthOK
		// stays false -> Outcome.Rolledback) without attempting a reverse.
		return sr

	case plan.ActionDelete:
		// NEVER auto-applied (ADR-W004 A4): the Wardstone op surface has
		// no volume-delete arm at all. A Volume-Delete step reaching
		// apply is refused unconditionally — either the plan/approval gate
		// has a bug (this should have stopped at PendingApproval), or,
		// even once approved, there is genuinely no operation to apply it
		// with. This is the "guard so a volume-delete reaching apply...
		// is refused as a bug" S2b calls for.
		sr.Error = "refusing to apply a Volume Delete: no Wardstone op exists to delete a volume, and Keystone never auto-deletes data (ADR-W004 A4)"
		return sr

	default:
		sr.Error = "unexpected action " + step.Action.String()
		return sr
	}
}

// ---------------------------------------------------------------- apply: Route

func (r *Reconciler) applyRouteStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource) StepResult {
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}
	path := routeFragmentPath(r.ManagedPrefix, step.Name)

	// Capture the fragment's current bytes before writing, so a
	// health-gate (or reload) failure can restore them (ADR-W004 A4 step
	// 5: "Route -> restore prior fragment bytes"). NOT_FOUND is a
	// legitimate "prior state" too — restoring it means writing back
	// empty, the same approach the Delete action itself uses below.
	var priorContent []byte
	if priorRes, priorErr := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ReadManagedFile{
		ReadManagedFile: &wsv1.ReadManagedFile{Path: path},
	}}); priorErr == nil && priorRes.GetError() == nil {
		priorContent = priorRes.GetReadManagedFile().GetContent()
	}

	var content []byte
	var wantSpec *resource.RouteSpec
	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		want, ok := desiredByKey[key]
		if !ok || want.Route == nil {
			sr.Error = "internal: no desired Route spec for " + step.Name
			return sr
		}
		wantSpec = want.Route
		content = renderRouteFragment(wantSpec, r.autheliaAddr(), r.currentPosture())
	case plan.ActionDelete:
		// No delete-file op exists (only WriteManagedFile/ReadManagedFile)
		// — a Route delete overwrites the fragment instead (ADR-W004 A4
		// step 4 documents this). The replacement is a comment, not empty
		// bytes: an empty import makes Caddy warn once a second forever
		// under --watch (F30).
		content = renderDeletedFragment(step.Name)
	default:
		sr.Error = "unexpected action " + step.Action.String()
		return sr
	}

	res, err := r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
		WriteManagedFile: &wsv1.WriteManagedFile{Path: path, Content: content},
	}})
	if err != nil {
		sr.Error = err.Error()
		return sr
	}
	if oe := res.GetError(); oe != nil {
		sr.Error = fmt.Sprintf("%s: %s", oe.GetCode(), oe.GetMessage())
		return sr
	}
	sr.Applied = true

	if err := r.runReload(ctx); err != nil {
		// The write itself succeeded (Applied stays true); only the reload
		// gate failed, so this is a rollback, not an Incident.
		r.restoreRouteFragment(ctx, nodeID, path, priorContent)
		sr.Reversed = true
		return sr
	}

	if step.Action == plan.ActionDelete {
		checkRes, checkErr := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ReadManagedFile{
			ReadManagedFile: &wsv1.ReadManagedFile{Path: path},
		}})
		sr.HealthOK = checkErr == nil && checkRes.GetError() == nil && len(checkRes.GetReadManagedFile().GetContent()) == 0
		if sr.HealthOK {
			r.markManaged(nodeID, key, false)
		}
		return sr
	}

	if r.probeRoute(ctx, nodeID, wantSpec) {
		sr.HealthOK = true
		r.markManaged(nodeID, key, true)
		return sr
	}

	r.restoreRouteFragment(ctx, nodeID, path, priorContent)
	sr.Reversed = true
	return sr
}

func (r *Reconciler) restoreRouteFragment(ctx context.Context, nodeID, path string, content []byte) {
	r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{ //nolint:errcheck
		WriteManagedFile: &wsv1.WriteManagedFile{Path: path, Content: content},
	}})
}

// runReload invokes the ReloadFn seam (ADR-W004 A1). nil is a documented
// no-op — the real Caddy reload lands in a later E-series work package.
func (r *Reconciler) runReload(ctx context.Context) error {
	if r.ReloadFn == nil {
		return nil
	}
	return r.ReloadFn(ctx)
}

// probeRoute health-gates a Route Create/Update (ADR-W004 A4 step 5:
// "Route -> ProbeHttp/ProbeTcp"). Only "ip:port"-shaped upstreams are
// actually dialed — a "container/<name>:<port>" upstream needs the
// container's Docker-network address, which Keystone has no way to
// resolve from the op surface available to S2b, so such routes are
// gate-pass-by-construction (the WriteManagedFile success is the whole
// gate for them). Flagged as a known limitation in the S2b report.

// routeFrontDoor is the host:port a Route is served at, when that is a
// thing the node can actually connect to.
//
// Only under the tailnet posture: there the address is the node's own
// MagicDNS name, which resolves on the node itself. Under a hostname
// posture the declared host may resolve nowhere near here, which is why
// the upstream probe skipped these entirely.
func (r *Reconciler) routeFrontDoor(spec *resource.RouteSpec) (string, uint32, bool) {
	p := r.currentPosture()
	if p.tls != "tailscale" || p.tailnetName == "" {
		return "", 0, false
	}
	host, portStr, ok := strings.Cut(routeSiteAddress(p, spec.Host), ":")
	if !ok {
		return "", 0, false // no port assigned: nothing is serving it
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, false
	}
	return host, uint32(port), true
}

func (r *Reconciler) probeRoute(ctx context.Context, nodeID string, spec *resource.RouteSpec) bool {
	// Under the tailnet posture, probe the FRONT DOOR rather than the
	// upstream (F83). This is the only check that can tell whether Caddy
	// accepted the config it was just handed: it rejects a whole file on
	// any ambiguity and keeps serving the previous one, so the fragment
	// on disk is correct, every resource reports healthy, and the port
	// this Route was assigned is not bound by anything.
	//
	// That happened: a port reassignment briefly gave two sites the same
	// address, Caddy refused the file, and the front door served stale
	// routing — one service's link reaching another, a second reaching
	// nothing — for as long as nobody looked.
	if host, port, ok := r.routeFrontDoor(spec); ok {
		attempts, backoff := r.healthGateParams()
		for i := 0; i < attempts; i++ {
			res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ProbeTcp{
				ProbeTcp: &wsv1.ProbeTcp{Host: host, Port: port},
			}})
			if err == nil && res.GetError() == nil && res.GetProbeTcp().GetReachable() {
				return true
			}
			if i < attempts-1 {
				select {
				case <-ctx.Done():
					return false
				case <-time.After(backoff):
				}
			}
		}
		return false
	}
	if strings.HasPrefix(spec.Upstream, "container/") {
		// No front door to probe and a container upstream the agent
		// cannot dial by name: nothing to check, as before.
		return true
	}
	url := "http://" + spec.Upstream
	attempts, backoff := r.healthGateParams()
	for i := 0; i < attempts; i++ {
		res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ProbeHttp{
			ProbeHttp: &wsv1.ProbeHttp{Url: url},
		}})
		if err == nil && res.GetError() == nil && res.GetProbeHttp().GetReachable() {
			return true
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(backoff):
			}
		}
	}
	return false
}

// ---------------------------------------------------------------- retry/backoff

func (r *Reconciler) healthGateParams() (attempts int, backoff time.Duration) {
	attempts = r.HealthGateAttempts
	if attempts <= 0 {
		attempts = 5
	}
	backoff = r.HealthGateBackoff
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
	}
	return attempts, backoff
}

// retryDispatch dispatches op, retrying up to MaxApplyRetries times
// (default 3, ADR-W004 A4) on a transient failure: a transport-level
// error from Applier.Apply, or an OpError with code RUNTIME (the agent's
// docker/filesystem-failure code — plausibly transient). Any other
// OpError (FORBIDDEN, INVALID, NOT_FOUND, VERIFICATION) is not transient
// and is returned immediately without retry.
func (r *Reconciler) retryDispatch(ctx context.Context, nodeID string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	maxRetries := r.MaxApplyRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	backoff := r.RetryBackoff
	if backoff <= 0 {
		backoff = 50 * time.Millisecond
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		res, err := r.Applier.Apply(ctx, nodeID, op)
		if err == nil {
			if oe := res.GetError(); oe != nil && oe.GetCode() == wsv1.ErrorCode_ERROR_CODE_RUNTIME {
				lastErr = fmt.Errorf("%s: %s", oe.GetCode(), oe.GetMessage())
			} else {
				return res, nil
			}
		} else {
			lastErr = err
		}
		if attempt < maxRetries-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
	}
	return nil, fmt.Errorf("op failed after %d attempts: %w", maxRetries, lastErr)
}

// currentPosture is the posture for the tick in flight. A zero value —
// possible only if a renderer runs outside Reconcile, which today means
// a test — reads as the pre-Ingress default rather than as "no scheme".
func (r *Reconciler) currentPosture() ingressPosture {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.posture.tls == "" {
		return terminatedUpstream
	}
	return r.posture
}
