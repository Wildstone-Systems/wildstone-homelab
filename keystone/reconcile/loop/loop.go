// Package loop is the E3 reconcile service (ADR-W008): the level-
// triggered driver that turns approved plan records into engine passes,
// repairs low-risk drift, surfaces Medium/High drift for approval, and
// keeps runtimedb's observed status and event stream current. The S2
// engine (keystone/reconcile) stays the authority on how one pass works;
// this package decides when passes run and what they mean for plan
// records.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/reconcile"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

// driftPrincipal marks plan records the drift path proposes (ADR-W008
// A2): surfaced drift awaiting a human decision through the normal API.
const driftPrincipal = "system/drift"

// selfDrivenPrincipals mark plan records another Keystone component
// drives through the state machine itself — backup-engine runs
// (keystone/backup) and secret rotations (keystone/api) — with
// provenance but no Statefile intent, so the reconcile loop must never
// pick them up as approved work. Kept as small duplicated constants
// (the specHashLabel pattern) rather than imports — the loop needs the
// strings, not the engines. system/oob and system/drift plans, by
// contrast, ARE the loop's to converge.
var selfDrivenPrincipals = map[string]bool{
	"system/backup":  true,
	"system/secrets": true,
}

// Loop wires the service's dependencies. Rec must have Applier (and any
// Providers) configured; Loop owns Rec.Authorize — callers must leave it
// and Rec.Approve nil (see Tick).
type Loop struct {
	State *statefile.Store
	DB    *runtimedb.DB
	Rec   *reconcile.Reconciler
	// Connected reports whether a node has a live agent session
	// (gateway.Gateway.Connected in production). nil assumes connected —
	// the fake-applier test posture.
	Connected func(node string) bool
	Log       *slog.Logger

	// calendarDrift is the last set of calendar-grant findings reported,
	// so a standing finding is recorded once rather than every tick.
	calendarDrift string

	// AccessGroups maps a capability to the household groups its
	// manifest declares it is for (ADR-W033 design §4, slice 8).
	//
	// Plain data rather than a catalog handle: the loop needs one fact
	// about each package and should not grow a dependency on the thing
	// that parses them. nil disables seeding entirely, which is the
	// posture every existing test is in.
	AccessGroups map[string][]string

	// failStreak counts consecutive Ticks that failed (a non-empty
	// TickOutcome.Failed or a Tick error) — the input to Run's backoff
	// (ADR-W008 R2b). Reset to zero by any non-failing tick.
	failStreak atomic.Int32

	pokeOnce sync.Once
	poke     chan struct{}
}

// Poke wakes Run for an immediate tick — plan approval and out-of-band
// ingestion call it so convergence starts now instead of at the next
// interval (ADR-W008 R2a). A poke deliberately bypasses failure backoff:
// it signals fresh human intent, which is exactly when a retry is
// warranted. Non-blocking; pokes coalesce.
func (l *Loop) Poke() {
	select {
	case l.pokeCh() <- struct{}{}:
	default:
	}
}

func (l *Loop) pokeCh() chan struct{} {
	l.pokeOnce.Do(func() { l.poke = make(chan struct{}, 1) })
	return l.poke
}

// maxBackoffShift caps the R2b backoff at base<<5 (32× the interval).
const maxBackoffShift = 5

// delay returns how long Run waits before the next tick: the base
// interval, doubled per consecutive failed tick up to the cap — repeated
// failures (a wedged provider, a node that keeps failing its gate) stop
// hammering the same error while the level-triggered loop still retries.
func (l *Loop) delay(base time.Duration) time.Duration {
	shift := l.failStreak.Load()
	if shift > maxBackoffShift {
		shift = maxBackoffShift
	}
	return base << shift
}

// TickOutcome summarizes one Tick for callers and tests.
type TickOutcome struct {
	// Revision is the accepted Statefile revision the pass converged
	// toward ("" for an empty repository or a tick that did not run).
	Revision string
	// PlanIDs are the approved plans this tick executed (empty = drift tick).
	PlanIDs []string
	// Failed is the failure detail when the tick's pass did not fully
	// converge ("" = success or clean skip).
	Failed string
	// DriftProposed is the plan ID of a newly proposed drift plan, if any.
	DriftProposed string
	// Repaired is true when a drift tick applied at least one low-risk fix.
	Repaired bool
	// Blocked lists resources set aside this tick after failing the same
	// way repeatedly (F60), so the rest of the plan could proceed.
	Blocked []reconcile.BlockedResource
	// Waiting are the queued plans left applying: a node they need was
	// offline, or held by Medium/High work nobody approved.
	Waiting []string
}

// Tick runs one service pass (ADR-W008 A1–A5).
func (l *Loop) Tick(ctx context.Context) (TickOutcome, error) {
	log := l.Log
	if log == nil {
		log = slog.Default()
	}
	quarantined := false
	if err := l.DB.CheckQuarantine(); err != nil {
		if !errors.Is(err, runtimedb.ErrQuarantined) {
			return TickOutcome{}, fmt.Errorf("loop: checking restore quarantine: %w", err)
		}
		quarantined = true
	}

	approved, err := l.DB.PlansInState(runtimedb.PlanApproved)
	if err != nil {
		return TickOutcome{}, err
	}
	applying, err := l.DB.PlansInState(runtimedb.PlanApplying)
	if err != nil {
		return TickOutcome{}, err
	}
	// Self-driven records pass through 'approved' microseconds before
	// their owning component moves them to 'applying'; grabbing one here
	// would double-drive its state machine (ADR-W011 A4, ADR-W012 A5).
	filterSelfDriven := func(records []runtimedb.PlanRecord, applyingState bool) []runtimedb.PlanRecord {
		filtered := records[:0]
		for _, p := range records {
			if selfDrivenPrincipals[p.Principal] {
				continue
			}
			// Household tracker edits are deliberately inserted directly as
			// applying audit records while their request commits the Statefile.
			// They never authorize infrastructure work. Resuming arbitrary
			// applying plans without recognizing this envelope would let the
			// loop steal that short-lived record from its owner.
			if applyingState {
				var envelope struct {
					AuditOnly bool `json:"auditOnly"`
				}
				if json.Unmarshal([]byte(p.PlanJSON), &envelope) == nil && envelope.AuditOnly {
					continue
				}
			}
			filtered = append(filtered, p)
		}
		return filtered
	}
	approved = filterSelfDriven(approved, false)
	applying = filterSelfDriven(applying, true)
	// Desired state is the accepted revision and nothing else (ADR-W008
	// A1 as amended): an uncommitted edit, or a commit the ingestion loop
	// has not accepted, never reaches a node.
	revision, desired, err := l.State.Snapshot()
	if errors.Is(err, statefile.ErrNoAcceptedRevision) {
		// History with nothing accepted yet: a restored or upgraded
		// control plane before its first ingestion tick. Converging to an
		// empty tree would remove everything; wait instead.
		log.Info("reconcile paused: no accepted statefile revision yet")
		return TickOutcome{}, nil
	}
	if err != nil {
		return TickOutcome{}, fmt.Errorf("loop: loading accepted statefile revision: %w", err)
	}
	// A restored control plane stays inert until every replacement node has
	// successfully imported its sealed secret bundle. Once that durable marker
	// exists, reconciliation may propose and apply the infrastructure needed to
	// restore applications; the broader restore quarantine remains in force
	// until the journal replay and explicit release.
	if quarantined {
		nodes := map[string]bool{}
		for _, r := range desired {
			if r.Metadata.Node != "" {
				nodes[r.Metadata.Node] = true
			}
		}
		for node := range nodes {
			imported, err := l.DB.RecoveryNodeSecretsImported(node)
			if err != nil {
				return TickOutcome{}, fmt.Errorf("loop: checking recovery import for node %s: %w", node, err)
			}
			if !imported {
				log.Info("reconcile paused pending recovery secret import", "node", node)
				return TickOutcome{}, nil
			}
		}
	}
	// What the accepted revision says each resource is, before any
	// projection below edits the copy. An approval is bound to these
	// digests (ADR-W008 A2a), so household facts projected into a
	// resource never make a reviewed step look different, and never make
	// an unreviewed one look reviewed.
	accepted := acceptedDigests(desired)

	// Entitlements become directory memberships here, on a COPY of
	// desired state, before anything is grouped or planned (§4, slice
	// 8). Every tick, recomputed from scratch, so a hand-removed
	// membership comes back and a withdrawn entitlement goes away
	// without either waiting for an event nobody sends.
	// Entitlements first exist here: a capability installed gets the
	// rows its manifest declares, and one uninstalled has its rows made
	// dormant rather than deleted (§17.3).
	if err := seedCapabilityEntitlements(l.DB, l.AccessGroups, desired); err != nil {
		return TickOutcome{}, fmt.Errorf("loop: seeding entitlements: %w", err)
	}
	desired, membershipDrift, err := projectEntitlements(l.DB, desired)
	if err != nil {
		return TickOutcome{}, fmt.Errorf("loop: projecting entitlements: %w", err)
	}
	for _, d := range membershipDrift {
		// Reported, never acted on. Each of these is a household-visible
		// fact that projection deliberately declined to resolve by
		// guessing.
		l.event("projection.drift", "", d.Account, map[string]string{"reason": d.Reason})
	}

	// The calendar registry's grants become usernames here, on the same
	// copy, for the same reason (ADR-W038, #372).
	desired, calendarDrift, err := projectCalendarRights(l.DB, desired)
	if err != nil {
		return TickOutcome{}, fmt.Errorf("loop: projecting calendar grants: %w", err)
	}
	if summary := fmt.Sprint(calendarDrift); summary != l.calendarDrift {
		l.calendarDrift = summary
		for _, d := range calendarDrift {
			l.event("projection.calendar", "", d.Account, map[string]string{"reason": d.Reason})
		}
	}

	out := TickOutcome{Revision: revision}
	queuedPlans := append(append([]runtimedb.PlanRecord{}, applying...), approved...)
	planTick := len(queuedPlans) > 0
	for _, p := range queuedPlans {
		out.PlanIDs = append(out.PlanIDs, p.ID)
	}
	for _, p := range approved {
		if err := l.DB.TransitionPlan(p.ID, runtimedb.PlanApplying, ""); err != nil {
			return out, err
		}
	}
	// Plans are intents, not scripts (ADR-W008 A1): approval already
	// committed the change, so executing them means converging to the
	// accepted revision now. What an approval authorizes is exactly the
	// steps it reviewed, at the digests it reviewed (ADR-W008 A2a): a
	// Medium/High step nobody approved holds its node, however many other
	// Plans are approved this tick.
	auth, err := newAuthority(queuedPlans, accepted)
	if err != nil {
		return out, err
	}
	l.Rec.Authorize = auth.covers
	l.Rec.ClusterDelete = auth.clusterDelete
	l.Rec.ClusterRevision = revision
	l.Rec.ClusterRoutes = l.Rec.ClusterRoutes[:0]
	for _, r := range desired {
		if r.Kind == resource.KindRoute {
			l.Rec.ClusterRoutes = append(l.Rec.ClusterRoutes, r)
		}
	}
	l.Rec.ClusterAuthority = func(st plan.PlanStep) (reconcile.Authority, error) {
		planID, digest, ok := auth.grant(st)
		if !ok {
			return reconcile.Authority{}, fmt.Errorf("cluster: %s %s/%s has no exact approved Plan step", st.Action, st.Kind, st.Name)
		}
		term, issued, err := l.DB.CurrentLeaderEpoch()
		if err != nil {
			return reconcile.Authority{}, err
		}
		if !issued || term.Epoch == 0 {
			return reconcile.Authority{}, fmt.Errorf("cluster: no active leader epoch has been issued")
		}
		return reconcile.Authority{AcceptedRevision: revision, PlanID: planID, StepDigest: digest, LeaderEpoch: term.Epoch}, nil
	}
	// A failed Delete is recorded unhealthy because the resource is still
	// present but not in desired state. Startup deliberately does not seed
	// unhealthy observations into the Reconciler, so an applying plan that
	// survives a Keystone restart would otherwise lose both the resource
	// and its node: desired has no declaration, ManagedNodes is empty, and
	// the retry would incorrectly resolve the plan as applied (#323).
	// Reconstruct only the delete targets authorized by the queued plans,
	// joined to their durable observed rows for node placement.
	if err := l.seedPendingDeletes(queuedPlans); err != nil {
		return out, err
	}

	// Where each resource the runtime DB knows of lives, read before any
	// pass edits the rows. A resource a Plan deletes is no longer desired,
	// and on a node that is offline or whose pass errors no step names it
	// either; this is what still places it, so the Plan waits for that
	// node instead of reading as converged.
	placements, err := l.observedPlacements()
	if err != nil {
		return out, err
	}

	groups := groupByNode(desired)
	// A node whose desired set became empty still needs a pass, or its
	// deletions are never planned (the engine tracks what it manages per
	// node; see Reconciler.ManagedNodes).
	present := map[string]bool{}
	for _, g := range groups {
		present[g.node] = true
	}
	managed := l.Rec.ManagedNodes()
	sort.Strings(managed)
	for _, node := range managed {
		if !present[node] {
			groups = append(groups, nodeGroup{node: node})
		}
	}

	var failures []string
	var unauthorized []plan.PlanStep
	results := map[string]*nodeResult{}
	acting := map[string]bool{}
	// Nodes this tick did not actually get to. Their status rows are the
	// only durable record of what the reconciler manages there — the
	// managed set is seeded from them on restart (F76) — so a node that
	// was skipped must keep every row it has.
	unvisited := map[string]bool{}
	for _, g := range groups {
		nr := &nodeResult{}
		results[g.node] = nr
		if g.node != "" && g.node != "@cluster" && l.Connected != nil && !l.Connected(g.node) && hasAgentKinds(g.resources) {
			// An offline node says nothing about whether its state can
			// converge. A Plan with work there stays applying, so a
			// reconnect retries it without another approval or an
			// unrelated Statefile commit (#323).
			nr.deferred = true
			if planTick {
				l.event("reconcile.deferred-offline", g.node, "plan remains retryable", nil)
			} else {
				l.event("reconcile.skipped-offline", g.node, "", nil)
			}
			unvisited[g.node] = true
			continue
		}
		outcome, err := l.Rec.Reconcile(ctx, g.node, g.resources)
		if err != nil {
			var clusterUnavailable *reconcile.ClusterUnavailable
			if errors.As(err, &clusterUnavailable) {
				nr.deferred = true
				unvisited[g.node] = true
				if planTick {
					l.event("reconcile.deferred-leader", g.node, "leader unavailable; retry pending", nil)
				}
				continue
			}
			if g.node != "" && g.node != "@cluster" && l.Connected != nil && !l.Connected(g.node) {
				nr.deferred = true
				unvisited[g.node] = true
				if planTick {
					l.event("reconcile.deferred-offline", g.node, "plan interrupted; retry pending", nil)
				}
				continue
			}
			nr.failed = err.Error()
			failures = append(failures, fmt.Sprintf("node %s: %v", g.node, err))
			unvisited[g.node] = true
			continue
		}
		nr.record(outcome)
		l.recordObserved(g, outcome)
		for _, st := range outcome.Plan.Steps {
			acting[string(st.Kind)+"/"+st.Name] = true
		}
		for _, b := range outcome.Blocked {
			// A blocked resource keeps its status row even when nothing
			// desires it any more. A DELETE can be quarantined too — an
			// agent that refuses to remove a container, say — and that
			// resource is desired by nobody, so without this the prune
			// below would take away the only record that it is still
			// there. An orphan nothing reports is worse than an orphan.
			acting[string(b.Kind)+"/"+b.Name] = true
			out.Blocked = append(out.Blocked, b)
			l.event("reconcile.blocked", g.node, b.Summary(),
				map[string]string{"kind": string(b.Kind), "name": b.Name})
		}
		switch {
		case g.node != "" && g.node != "@cluster" && l.Connected != nil && !l.Connected(g.node):
			// The operation may have landed before the session disappeared.
			// Preserve the applying plan and let a fresh observation decide;
			// never turn transport uncertainty into rollback or a terminal
			// plan verdict.
			nr.deferred = true
			if planTick {
				l.event("reconcile.deferred-offline", g.node, "plan interrupted; retry pending", nil)
			}
		case planTick && outcome.Retryable:
			nr.deferred = true
			l.event("reconcile.deferred", g.node, outcome.Incident, nil)
		case outcome.Incident != "":
			nr.failed = outcome.Incident
			failures = append(failures, fmt.Sprintf("node %s: %s", g.node, outcome.Incident))
		case outcome.Rolledback:
			nr.failed = "health gate failed, rolled back"
			failures = append(failures, fmt.Sprintf("node %s: health gate failed, rolled back", g.node))
		case outcome.PendingApproval:
			nr.pending = true
			unauthorized = append(unauthorized, outcome.Unauthorized...)
		case len(outcome.Plan.Steps) > 0 && outcome.Applied:
			// Attributed to what authorized it: a step a Plan reviewed is
			// that Plan's work; anything else is drift repair under its
			// own Low-risk policy, whether or not a Plan was queued.
			if auth.reviewedAny(outcome.Plan.Steps) {
				l.event("reconcile.applied", g.node, outcome.Plan.Summary,
					map[string]string{"steps": fmt.Sprint(len(outcome.Plan.Steps))})
			} else {
				out.Repaired = true
				l.event("drift.repaired", g.node, outcome.Plan.Summary,
					map[string]string{"steps": fmt.Sprint(len(outcome.Plan.Steps))})
			}
		}
		if len(outcome.PrereqRepaired) > 0 {
			// Silent repair is right (see repairNodePrereqs) but invisible
			// repair is not: the operator should be able to see that a
			// managed file was edited and put back, especially the base
			// Caddyfile, whose default-deny block is a security posture.
			out.Repaired = true
			l.event("prereq.repaired", g.node, strings.Join(outcome.PrereqRepaired, ", "), nil)
		}
		if len(outcome.ForeignDrift) > 0 {
			l.event("drift.foreign", g.node, strings.Join(outcome.ForeignDrift, ", "), nil)
		}
	}

	l.pruneObserved(desired, acting, unvisited)

	out.Failed = strings.Join(failures, "; ")
	// Each Plan gets its own verdict (ADR-W008 A2a): what happened to the
	// resources IT reviewed, not to the whole tree. The Statefile is never
	// reverted on failure: desired stays desired and the next tick retries.
	homes := resourceNodes(placements, desired, results)
	deferred := false
	var failedPlans []string
	for _, p := range auth.plans {
		switch p.verdict(homes, results) {
		case verdictWait:
			deferred = true
			out.Waiting = append(out.Waiting, p.id)
		case verdictFailed:
			failedPlans = append(failedPlans, p.id)
			if err := l.DB.TransitionPlan(p.id, runtimedb.PlanFailed, ""); err != nil {
				return out, err
			}
		default:
			if err := l.DB.TransitionPlan(p.id, runtimedb.PlanApplied, ""); err != nil {
				return out, err
			}
		}
	}
	if out.Failed != "" {
		attrs := map[string]string(nil)
		if len(failedPlans) > 0 {
			attrs = map[string]string{"plans": strings.Join(failedPlans, ",")}
		}
		l.event("reconcile.failed", "", out.Failed, attrs)
	}

	// Medium/High work nobody approved becomes one exact preview a human
	// can approve, on drift ticks and plan ticks alike: a queued Plan no
	// longer carries it through.
	if len(unauthorized) > 0 {
		id, err := l.proposeDrift(revision, unauthorized, accepted)
		if err != nil {
			return out, err
		}
		out.DriftProposed = id
	}
	if deferred {
		l.failStreak.Add(1)
		log.Warn("reconcile tick deferred", "plans", out.PlanIDs,
			"backoff_streak", l.failStreak.Load())
	} else if out.Failed != "" {
		l.failStreak.Add(1)
		log.Warn("reconcile tick failed", "detail", out.Failed, "plans", out.PlanIDs,
			"backoff_streak", l.failStreak.Load())
	} else {
		l.failStreak.Store(0)
	}
	return out, nil
}

// observedPlacements maps resKey to the node its observed row records.
func (l *Loop) observedPlacements() (map[string]string, error) {
	rows, err := l.DB.ListObserved()
	if err != nil {
		return nil, fmt.Errorf("loop: loading observed placements: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[resKey(resource.Kind(row.Kind), row.Name)] = row.Node
	}
	return out, nil
}

// seedPendingDeletes restores Reconciler ownership for delete targets after a
// process restart. Stored proposals name the resource but deletion refs do not
// carry node placement, while observed rows carry the node but unhealthy rows
// are intentionally excluded from the ordinary startup seed. Joining the two
// retains exactly the pending, plan-authorized deletion work.
func (l *Loop) seedPendingDeletes(records []runtimedb.PlanRecord) error {
	targets := map[string]bool{}
	for _, record := range records {
		var stored plan.StoredProposal
		if err := json.Unmarshal([]byte(record.PlanJSON), &stored); err != nil {
			return fmt.Errorf("loop: decoding applying plan %s: %w", record.ID, err)
		}
		for _, ref := range stored.Delete {
			targets[ref.Kind+"/"+ref.Name] = true
		}
		if stored.Plan != nil {
			for _, step := range stored.Plan.Steps {
				if step.Action == plan.ActionDelete {
					targets[string(step.Kind)+"/"+step.Name] = true
				}
			}
		}
	}
	if len(targets) == 0 {
		return nil
	}
	observed, err := l.DB.ListObserved()
	if err != nil {
		return fmt.Errorf("loop: loading pending delete observations: %w", err)
	}
	byNode := map[string][]reconcile.ManagedRef{}
	for _, row := range observed {
		if row.Node == "" || !targets[row.Kind+"/"+row.Name] {
			continue
		}
		byNode[row.Node] = append(byNode[row.Node], reconcile.ManagedRef{
			Kind: resource.Kind(row.Kind), Name: row.Name,
		})
	}
	for node, refs := range byNode {
		l.Rec.SeedManaged(node, refs)
	}
	return nil
}

// proposeDrift records Medium/High work nobody approved as a proposed
// system/drift plan for a human to approve through the API — at most one
// open at a time (ADR-W008 A2).
//
// The proposal carries the exact steps it would authorize (ADR-W008
// A2a): each held step, bound to the content the accepted revision holds
// for it. Approving it permits those effects and nothing else; drift that
// changes after it was raised is a different proposal.
func (l *Loop) proposeDrift(head string, held []plan.PlanStep, accepted map[string]string) (string, error) {
	steps := make([]plan.PlanStep, 0, len(held))
	for _, st := range held {
		if st.Action != plan.ActionDelete {
			// Bind to the Statefile's content, not the projected copy the
			// pass diffed: covers() compares against the accepted revision.
			st.Digest = accepted[resKey(st.Kind, st.Name)]
		}
		steps = append(steps, st)
	}
	sort.Slice(steps, func(i, j int) bool { return stepKey(steps[i]) < stepKey(steps[j]) })

	open, err := l.DB.PlansInState(runtimedb.PlanProposed)
	if err != nil {
		return "", err
	}
	for _, p := range open {
		if p.Principal != driftPrincipal {
			continue
		}
		// The open proposal only speaks for the desired state and the exact
		// steps it was computed from. Approval refuses one whose base has
		// moved and supersedes it there (F28) — but until someone clicked
		// approve, this dedupe kept returning early and no fresh proposal
		// was ever raised, so the drift stayed unrepairable and the only
		// visible artifact was a plan that 409s when you finally act on it
		// (defect B). Supersede it here instead: the operator's one
		// available action stops being the one that cannot work.
		var stored plan.StoredProposal
		if err := json.Unmarshal([]byte(p.PlanJSON), &stored); err == nil && stored.BasedOnHead == head && sameSteps(stored.Plan, steps) {
			return "", nil // still speaks for exactly this drift
		}
		if err := l.DB.TransitionPlan(p.ID, runtimedb.PlanSuperseded, ""); err != nil {
			return "", err
		}
		l.event("drift.superseded", "", "statefile or drift changed since this drift proposal", map[string]string{"plan": p.ID})
	}
	summary := "drift repair requires approval (Medium/High risk)"
	// BasedOnHead must be the accepted revision, not "": approval refuses a
	// proposal whose base has moved, and no head ever equals "", so an
	// unset field made every drift repair permanently unapprovable and
	// left a drifted resource broken with no supported way back (F28).
	//
	// Put and Delete stay empty, and that is correct — desired state did
	// not drift, reality did. Approving this commits no documents; what
	// it does is authorize exactly these steps on the next tick.
	reviewed := plan.WithSteps(plan.Plan{Summary: summary}, steps)
	stored, err := json.Marshal(plan.StoredProposal{
		BasedOnHead: head,
		Summary:     summary,
		Plan:        &reviewed,
		Drift:       true,
	})
	if err != nil {
		return "", err
	}
	id, err := l.DB.InsertPlan(driftPrincipal, "", summary, reviewed.OverallRisk.String(), string(stored))
	if err != nil {
		return "", err
	}
	l.event("drift.pending-approval", "", summary, map[string]string{"plan": id})
	return id, nil
}

func stepKey(st plan.PlanStep) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s/%s", st.Kind, st.Name, st.Action, st.Backend, st.Digest, st.Risk)
}

// sameSteps reports whether a stored drift proposal covers exactly steps.
func sameSteps(p *plan.Plan, steps []plan.PlanStep) bool {
	if p == nil || len(p.Steps) != len(steps) {
		return false
	}
	have := make([]string, len(p.Steps))
	for i, st := range p.Steps {
		have[i] = stepKey(st)
	}
	sort.Strings(have)
	for i, st := range steps {
		if have[i] != stepKey(st) {
			return false
		}
	}
	return true
}

// recordObserved upserts observed status per desired resource (ADR-W008
// A4). Delete steps concern resources that have LEFT desired, so they are
// handled from the step results directly: a successful delete removes the
// observed row, a failed one keeps the row and marks it unhealthy.
//
// Health is read off the plan, not off the step results alone. A resource
// with no step is converged — observe found it present and matching, and
// that is the only evidence "healthy" ever had. A resource WITH a step is
// by definition not in its desired state, so it is healthy only once that
// step has been applied and passed its health gate this tick.
//
// Consulting only the step results was defect A: on a drift tick that
// stopped at PendingApproval, or any tick whose plan was never applied,
// Steps is empty, no resource is implicated in a failure, and every
// desired resource was stamped Healthy — so a container that observe had
// just found missing was reported green by `wild status`. The status
// surface said the opposite of what the reconciler had seen, in exactly
// the situation an operator opens it.
func (l *Loop) recordObserved(g nodeGroup, outcome *reconcile.Outcome) {
	unhealthy := map[string]string{}
	// A blocked resource has been REMOVED from the plan, so nothing below
	// implicates it and it would otherwise be stamped healthy — the same
	// lie F75 was about, told about the one resource that is genuinely
	// broken.
	for _, b := range outcome.Blocked {
		unhealthy[string(b.Kind)+"/"+b.Name] = b.Summary()
	}
	for _, sr := range outcome.Steps {
		if sr.Applied && sr.HealthOK {
			if sr.Step.Action == plan.ActionDelete {
				if err := l.DB.DeleteObserved(string(sr.Step.Kind), sr.Step.Name); err != nil {
					slog.Default().Error("observed delete failed", "resource", sr.Step.Name, "err", err)
				}
			}
			continue
		}
		detail := sr.Error
		if detail == "" {
			detail = "health gate failed"
		}
		if sr.Step.Action == plan.ActionDelete {
			if err := l.DB.UpsertObserved(runtimedb.Observed{
				Kind: string(sr.Step.Kind), Name: sr.Step.Name, Node: g.node, Detail: detail,
			}); err != nil {
				slog.Default().Error("observed upsert failed", "resource", sr.Step.Name, "err", err)
			}
			continue
		}
		unhealthy[string(sr.Step.Kind)+"/"+sr.Step.Name] = detail
	}
	// Planned (non-delete) steps, and whether each one landed this tick.
	planned := map[string]plan.Action{}
	for _, st := range outcome.Plan.Steps {
		if st.Action == plan.ActionDelete {
			continue
		}
		planned[string(st.Kind)+"/"+st.Name] = st.Action
	}
	converged := map[string]bool{}
	for _, sr := range outcome.Steps {
		if sr.Applied && sr.HealthOK && sr.Step.Action != plan.ActionDelete {
			converged[string(sr.Step.Kind)+"/"+sr.Step.Name] = true
		}
	}

	for _, r := range g.resources {
		key := string(r.Kind) + "/" + r.Metadata.Name
		o := runtimedb.Observed{
			Kind: string(r.Kind), Name: r.Metadata.Name, Node: g.node,
			Healthy: true,
			// The application's own opinion, independent of whether
			// Wildstone considers this converged. A container can be
			// exactly what desired state asked for and still be
			// downloading a model and refusing connections.
			AppHealth: outcome.AppHealth[key],
		}
		if action, needsWork := planned[key]; needsWork && !converged[key] {
			o.Healthy = false
			// WHY it is not applied is the part an operator needs, and
			// the two reasons are nothing alike (F75). A Medium/High
			// plan waits for a human — the system behaving exactly as
			// designed — and saying only "not applied" makes that
			// indistinguishable from a homelab that has broken.
			//
			// On the alpha VM a restart showed 29 of 51 red while every
			// container was up; the reading fooled the person who wrote
			// the fix, with the plan history open in front of them.
			if outcome.PendingApproval {
				o.AwaitingApproval = true
				o.Detail = action.String() + " is waiting for your approval; nothing is wrong with this resource"
			} else {
				switch action {
				case plan.ActionCreate:
					o.Detail = "absent: create planned, not applied"
				default:
					o.Detail = "drifted: " + action.String() + " planned, not applied"
				}
			}
		}
		if detail, bad := unhealthy[key]; bad {
			o.Healthy = false
			o.Detail = detail
		}
		if err := l.DB.UpsertObserved(o); err != nil {
			slog.Default().Error("observed upsert failed", "resource", key, "err", err)
		}
		delete(unhealthy, key)
	}
	// Anything left is implicated in a failure but is not in desired
	// state — a delete that keeps failing, most likely. It still exists,
	// and the operator is the only one who can do anything about it, so
	// it gets a row of its own rather than disappearing quietly.
	for key, detail := range unhealthy {
		kind, name, ok := strings.Cut(key, "/")
		if !ok {
			continue
		}
		if err := l.DB.UpsertObserved(runtimedb.Observed{
			Kind: kind, Name: name, Node: g.node, Detail: detail,
		}); err != nil {
			slog.Default().Error("observed upsert failed", "resource", key, "err", err)
		}
	}
}

func (l *Loop) event(typ, node, msg string, attrs map[string]string) {
	if _, err := l.DB.AppendEvent(runtimedb.Event{Type: typ, Node: node, Message: msg, Attrs: attrs}); err != nil {
		slog.Default().Error("loop event append failed", "type", typ, "err", err)
	}
}

type nodeGroup struct {
	node      string
	resources []resource.Resource
}

// groupByNode buckets desired by metadata.node ("" holds the
// non-node-scoped kinds — DnsRecord, which dispatches through a Provider
// and needs no agent, and Automation, which Keystone itself owns),
// node-sorted for deterministic pass order.
// Nothing node-scoped may land in "": resource.Validate requires
// metadata.node for those kinds precisely so this bucketing stays
// meaningful, and Reconcile names the offender if one slips through.
func groupByNode(desired []resource.Resource) []nodeGroup {
	byNode := map[string][]resource.Resource{}
	for _, r := range desired {
		node := r.Metadata.Node
		if reconcile.TargetOf(r).Kind == reconcile.TargetCluster {
			node = "@cluster"
		}
		byNode[node] = append(byNode[node], r)
	}
	nodes := make([]string, 0, len(byNode))
	for n := range byNode {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	out := make([]nodeGroup, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeGroup{node: n, resources: byNode[n]})
	}
	return out
}

// hasAgentKinds reports whether any resource in rs dispatches through the
// Wardstone Applier (vs. provider-backed or declared-only kinds), i.e.
// whether a live agent session matters for this group (ADR-W008 A3).
func hasAgentKinds(rs []resource.Resource) bool {
	for _, r := range rs {
		switch r.Kind {
		case resource.KindContainer, resource.KindVolume, resource.KindRoute, resource.KindOidcClient:
			return true
		}
	}
	return false
}

// Run ticks until ctx is done: every base interval (stretched by the R2b
// failure backoff), or immediately on Poke (R2a). A Tick error counts
// toward the backoff streak like a failed pass.
func (l *Loop) Run(ctx context.Context, base time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(l.delay(base)):
		case <-l.pokeCh():
		}
		if _, err := l.Tick(ctx); err != nil {
			l.failStreak.Add(1)
			log := l.Log
			if log == nil {
				log = slog.Default()
			}
			log.Error("reconcile tick", "err", err)
		}
	}
}

// pruneObserved removes status rows for resources that are no longer
// desired anywhere and that no plan this tick is still acting on.
//
// A delete step normally clears the row when it applies. A resource that
// never existed has no delete step — nothing was ever created — so
// without this its row sits red forever, describing a service the
// operator has already removed. That is how a quarantined create looks
// after the operator takes the fix everyone recommends.
//
// Tick-wide rather than per node: a node whose resources never got as
// far as being managed has no group at all, which is exactly the case
// this exists for.
func (l *Loop) pruneObserved(desired []resource.Resource, acting, unvisited map[string]bool) {
	keep := map[string]bool{}
	for _, r := range desired {
		keep[string(r.Kind)+"/"+r.Metadata.Name] = true
	}
	// A pending delete keeps its row: the resource still exists in the
	// runtime, and saying otherwise before it is gone is the same kind
	// of lie as stamping it healthy.
	for k := range acting {
		keep[k] = true
	}
	obs, err := l.DB.ListObserved()
	if err != nil {
		slog.Default().Error("observed list failed", "err", err)
		return
	}
	for _, o := range obs {
		if keep[o.Kind+"/"+o.Name] || unvisited[o.Node] {
			continue
		}
		if err := l.DB.DeleteObserved(o.Kind, o.Name); err != nil {
			slog.Default().Error("observed prune failed", "resource", o.Name, "err", err)
		}
	}
}
