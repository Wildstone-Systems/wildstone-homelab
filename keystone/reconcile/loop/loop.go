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
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
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
// Providers) configured; Loop owns Rec.Approve — callers must leave it
// nil (see Tick).
type Loop struct {
	State *statefile.Store
	DB    *runtimedb.DB
	Rec   *reconcile.Reconciler
	// Connected reports whether a node has a live agent session
	// (gateway.Gateway.Connected in production). nil assumes connected —
	// the fake-applier test posture.
	Connected func(node string) bool
	Log       *slog.Logger

	// allowMediumHigh is consulted by the Approve callback installed on
	// Rec: true only while a plan tick is executing human-approved
	// changes (ADR-W008 A2). Loop is the only driver of Rec, so this is
	// a mode switch, not a synchronization point; atomic keeps it honest
	// if a caller ever runs Tick concurrently by mistake.
	allowMediumHigh atomic.Bool
	installed       bool

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
}

// Tick runs one service pass (ADR-W008 A1–A5).
func (l *Loop) Tick(ctx context.Context) (TickOutcome, error) {
	log := l.Log
	if log == nil {
		log = slog.Default()
	}
	if !l.installed {
		l.Rec.Approve = func(*plan.Plan) bool { return l.allowMediumHigh.Load() }
		l.installed = true
	}

	approved, err := l.DB.PlansInState(runtimedb.PlanApproved)
	if err != nil {
		return TickOutcome{}, err
	}
	// Self-driven records pass through 'approved' microseconds before
	// their owning component moves them to 'applying'; grabbing one here
	// would double-drive its state machine (ADR-W011 A4, ADR-W012 A5).
	filtered := approved[:0]
	for _, p := range approved {
		if !selfDrivenPrincipals[p.Principal] {
			filtered = append(filtered, p)
		}
	}
	approved = filtered
	desired, err := l.State.Load()
	if err != nil {
		return TickOutcome{}, fmt.Errorf("loop: loading statefile: %w", err)
	}

	out := TickOutcome{}
	planTick := len(approved) > 0
	for _, p := range approved {
		out.PlanIDs = append(out.PlanIDs, p.ID)
		if err := l.DB.TransitionPlan(p.ID, runtimedb.PlanApplying, ""); err != nil {
			return out, err
		}
	}
	// Plans are intents, not scripts (ADR-W008 A1): approval already
	// committed the change, so executing them means converging to the
	// Statefile now, with Medium/High steps authorized by that approval.
	l.allowMediumHigh.Store(planTick)
	defer l.allowMediumHigh.Store(false)

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
	acting := map[string]bool{}
	// Nodes this tick did not actually get to. Their status rows are the
	// only durable record of what the reconciler manages there — the
	// managed set is seeded from them on restart (F76) — so a node that
	// was skipped must keep every row it has.
	unvisited := map[string]bool{}
	pendingDrift := false
	for _, g := range groups {
		if g.node != "" && l.Connected != nil && !l.Connected(g.node) && hasAgentKinds(g.resources) {
			if planTick {
				// The operator approved a change that cannot land: loud
				// failure (ADR-W008 A3).
				failures = append(failures, fmt.Sprintf("node %s not connected", g.node))
			} else {
				l.event("reconcile.skipped-offline", g.node, "", nil)
			}
			unvisited[g.node] = true
			continue
		}
		outcome, err := l.Rec.Reconcile(ctx, g.node, g.resources)
		if err != nil {
			failures = append(failures, fmt.Sprintf("node %s: %v", g.node, err))
			unvisited[g.node] = true
			continue
		}
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
		case outcome.Incident != "":
			failures = append(failures, fmt.Sprintf("node %s: %s", g.node, outcome.Incident))
		case outcome.Rolledback:
			failures = append(failures, fmt.Sprintf("node %s: health gate failed, rolled back", g.node))
		case outcome.PendingApproval:
			pendingDrift = true
		case len(outcome.Plan.Steps) > 0 && outcome.Applied:
			if planTick {
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
	if planTick {
		// One target tree, one uniform verdict for every plan that
		// described it (ADR-W008 A2). The Statefile is not reverted on
		// failure: desired stays desired and the next tick retries.
		to := runtimedb.PlanApplied
		if out.Failed != "" {
			to = runtimedb.PlanFailed
			l.event("reconcile.failed", "", out.Failed, map[string]string{"plans": strings.Join(out.PlanIDs, ",")})
		}
		for _, id := range out.PlanIDs {
			if err := l.DB.TransitionPlan(id, to, ""); err != nil {
				return out, err
			}
		}
	} else if out.Failed != "" {
		l.event("reconcile.failed", "", out.Failed, nil)
	}

	if pendingDrift && !planTick {
		id, err := l.proposeDrift(desired)
		if err != nil {
			return out, err
		}
		out.DriftProposed = id
	}
	if out.Failed != "" {
		l.failStreak.Add(1)
		log.Warn("reconcile tick failed", "detail", out.Failed, "plans", out.PlanIDs,
			"backoff_streak", l.failStreak.Load())
	} else {
		l.failStreak.Store(0)
	}
	return out, nil
}

// proposeDrift records Medium/High drift as a proposed system/drift plan
// for a human to approve through the API — at most one open at a time
// (ADR-W008 A2).
func (l *Loop) proposeDrift(desired []resource.Resource) (string, error) {
	head, err := l.State.Head()
	if err != nil {
		return "", fmt.Errorf("loop: reading statefile head for drift proposal: %w", err)
	}
	open, err := l.DB.PlansInState(runtimedb.PlanProposed)
	if err != nil {
		return "", err
	}
	for _, p := range open {
		if p.Principal != driftPrincipal {
			continue
		}
		// The open proposal only speaks for the desired state it was
		// computed from. Approval refuses one whose base has moved and
		// supersedes it there (F28) — but until someone clicked approve,
		// this dedupe kept returning early and no fresh proposal was ever
		// raised, so the drift stayed unrepairable and the only visible
		// artifact was a plan that 409s when you finally act on it
		// (defect B). Supersede it here instead: the operator's one
		// available action stops being the one that cannot work.
		var stored plan.StoredProposal
		if err := json.Unmarshal([]byte(p.PlanJSON), &stored); err == nil && stored.BasedOnHead == head {
			return "", nil // still speaks for the current desired state
		}
		if err := l.DB.TransitionPlan(p.ID, runtimedb.PlanSuperseded, ""); err != nil {
			return "", err
		}
		l.event("drift.superseded", "", "statefile moved since this drift proposal", map[string]string{"plan": p.ID})
	}
	summary := "drift repair requires approval (Medium/High risk)"
	// BasedOnHead must be the live head, not "": approval refuses a
	// proposal whose base has moved, and no head ever equals "", so an
	// unset field made every drift repair permanently unapprovable and
	// left a drifted resource broken with no supported way back (F28).
	//
	// Put and Delete stay empty, and that is correct — desired state did
	// not drift, reality did. Approving this commits no documents; what
	// it does is make the next tick a plan tick, which authorizes the
	// Medium/High steps that perform the repair.
	stored, err := json.Marshal(plan.StoredProposal{
		BasedOnHead: head,
		Summary:     summary,
		Drift:       true,
	})
	if err != nil {
		return "", err
	}
	id, err := l.DB.InsertPlan(driftPrincipal, "", summary, "medium+", string(stored))
	if err != nil {
		return "", err
	}
	l.event("drift.pending-approval", "", summary, map[string]string{"plan": id})
	_ = desired // the concrete diff re-materializes on the approving tick (level-triggered)
	return id, nil
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
		byNode[r.Metadata.Node] = append(byNode[r.Metadata.Node], r)
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
