// Package ingest is the Statefile out-of-band ingestion loop (ADR-W007
// A6): the piece that makes Keystone a well-behaved co-editor of a repo
// humans may legitimately edit directly — the escape hatch is product
// surface. Each Tick compares the repo HEAD against the runtime DB's
// statefile_head marker; a foreign delta is validated and either accepted
// (auto-approved plan record + advanced marker) or reverted (invalid
// trees never stay in place, where they would wedge every later
// reconcile).
package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/policy"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/publish"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/reconcile"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

// oobPrincipal is the recorded principal on plan records this loop
// creates: the change came in through git, not through any API token.
const oobPrincipal = "system/oob"

// Outcome says what one Tick did.
type Outcome string

const (
	// OutcomeNoChange: HEAD equals the marker (or the repo is empty).
	OutcomeNoChange Outcome = "no-change"
	// OutcomeAdopted: first run against a repo with history — the tree
	// validated and became the baseline without a plan record.
	OutcomeAdopted Outcome = "adopted"
	// OutcomeAccepted: a valid foreign edit was recorded as an
	// auto-approved plan and the marker advanced.
	OutcomeAccepted Outcome = "accepted"
	// OutcomeReverted: an invalid foreign edit was reverted by a new
	// commit restoring the last accepted tree.
	OutcomeReverted Outcome = "reverted"
	// OutcomeDiverged: the branch holds history that does not contain the
	// accepted revision. Nothing is accepted or reverted until a human
	// puts the branch back on top of it.
	OutcomeDiverged Outcome = "diverged"
)

// Result reports one Tick.
type Result struct {
	Outcome Outcome
	// Head is the repo HEAD after the tick (== the marker, except for
	// OutcomeNoChange on an empty repo where both are "").
	Head string
	// PlanID is set for OutcomeAccepted.
	PlanID string
	// Detail carries the validation error for OutcomeReverted.
	Detail string
}

// Ingestor wires the loop's dependencies. The caller (cmd/keystone, or
// E3's reconcile loop) decides the tick cadence.
type Ingestor struct {
	State *statefile.Store
	DB    *runtimedb.DB
	Log   *slog.Logger
	// Policy, when set, validates the foreign tree against the standing
	// invariants (ADR-W009 A1): repo access does not outrank policy, so a
	// policy-invalid hand edit is reverted exactly like a schema-invalid
	// one. nil skips policy (unit-test posture; cmd/keystone always sets
	// it).
	Policy *policy.Engine
	// ManagedPrefix is the reconciler's -managed-prefix, so an accepted
	// edit's plan also lists the containers it restarts through the files
	// they mount (reconcile.DependentRestarts). Its approval covers only
	// listed steps (ADR-W008 A2a).
	ManagedPrefix string
	// Publisher accepts adopted, accepted and reverting commits, the same
	// one API approval uses (ADR-W007 A8). nil builds one over State/DB,
	// which is what tests get.
	Publisher *publish.Publisher

	// diverged is the last head reported as diverged, so a standing
	// conflict is one event, not one per tick.
	diverged string
}

func (in *Ingestor) publisher(log *slog.Logger) *publish.Publisher {
	if in.Publisher == nil {
		in.Publisher = &publish.Publisher{State: in.State, DB: in.DB, Log: log}
	}
	return in.Publisher
}

// Tick runs one detect → validate → decide pass (ADR-W007 A6).
func (in *Ingestor) Tick() (Result, error) {
	log := in.Log
	if log == nil {
		log = slog.Default()
	}
	head, err := in.State.Head()
	if err != nil {
		return Result{}, err
	}
	marker, err := in.DB.StatefileHead()
	if err != nil {
		return Result{}, err
	}
	if head == marker || head == "" {
		return Result{Outcome: OutcomeNoChange, Head: head}, nil
	}
	pub := in.publisher(log)

	if marker != "" {
		// The branch is behind the accepted revision: a mirror that did
		// not finish. Catching it up is the whole job.
		if behind, err := in.State.IsAncestor(head, marker); err != nil {
			return Result{}, err
		} else if behind {
			if err := in.State.Mirror(marker); err != nil {
				return Result{}, fmt.Errorf("ingest: catching the branch up to %s: %w", marker, err)
			}
			return Result{Outcome: OutcomeNoChange, Head: marker}, nil
		}
		// History that does not descend from the accepted revision is a
		// conflict, not an edit (contract §4.5): accepting it would drop
		// accepted changes, reverting it would rewrite someone's work.
		// Keystone does neither and says so.
		if descends, err := in.State.IsAncestor(marker, head); err != nil {
			return Result{}, err
		} else if !descends {
			if in.diverged != head {
				in.diverged = head
				in.event(log, "statefile.diverged", head,
					fmt.Sprintf("the branch no longer contains the accepted revision %.12s; reset or rebase it onto that revision", marker))
			}
			return Result{Outcome: OutcomeDiverged, Head: head}, nil
		}
	}

	// First contact with a repo that already has history: validate and
	// adopt as baseline. There is no prior accepted tree to revert to, so
	// an invalid tree here is an error the operator must fix, not a
	// revert. The committed tree is what is validated, never the working
	// files, and standing policy applies here as it does to every later
	// adoption: a tree first seen is not exempt from the rules.
	if marker == "" {
		tree, err := in.State.LoadAt(head)
		if err == nil {
			err = in.checkPolicy(log, head, tree, nil)
		}
		if err != nil {
			in.event(log, "statefile.invalid", head, err.Error())
			return Result{}, fmt.Errorf("ingest: initial statefile tree is invalid: %w", err)
		}
		if _, err := pub.Publish(publish.Request{Source: runtimedb.SourceOutOfBand, Candidate: head}); err != nil {
			return Result{}, err
		}
		in.event(log, "statefile.adopted", head, "")
		return Result{Outcome: OutcomeAdopted, Head: head}, nil
	}

	subject, err := in.State.Subject(head)
	if err != nil {
		return Result{}, err
	}

	// Validate the foreign tree: the committed tree at head, not the
	// working files, which may hold edits nobody has committed. LoadAt
	// parses AND validates via resource.Unmarshal (strict YAML +
	// Validate), so any schema failure lands here.
	newTree, err := in.State.LoadAt(head)
	if err != nil {
		return in.revert(log, marker, head, subject, err)
	}

	oldTree, err := in.State.LoadAt(marker)
	if err != nil {
		return Result{}, fmt.Errorf("ingest: loading accepted tree %s: %w", marker, err)
	}

	// Standing invariants (ADR-W009 A1): repo access does not outrank
	// policy — a policy-invalid hand edit is reverted like a
	// schema-invalid one.
	if err := in.checkPolicy(log, head, newTree, oldTree); err != nil {
		return in.revert(log, marker, head, subject, err)
	}

	// Valid: record the delta as an auto-approved plan — approved because
	// the edit is already the committed desired state (whoever made it
	// holds repo access, an authority senior to any API token — though
	// not senior to policy, above); the plan record gives the change the
	// same audit/review/reconcile surface as an API-proposed one, and
	// E3's reconciler converges from it.
	p := plan.Diff(newTree, oldTree)
	if restarts := reconcile.DependentRestarts(in.ManagedPrefix, oldTree, newTree); len(restarts) > 0 && len(p.Steps) > 0 {
		reviewed := plan.WithSteps(*p, append(p.Steps, restarts...))
		p = &reviewed
	}
	summary := fmt.Sprintf("out-of-band edit %.12s: %s", head, subject)
	p.Summary = summary
	planID := ""
	if len(p.Steps) == 0 {
		// Commit(s) that don't change the resource tree (docs, comments,
		// whitespace): accept silently, no plan needed.
		if _, err := pub.Publish(publish.Request{Source: runtimedb.SourceOutOfBand, Expected: marker, Candidate: head}); err != nil {
			return Result{}, err
		}
		in.event(log, "statefile.oob-accepted", head, subject)
		return Result{Outcome: OutcomeAccepted, Head: head}, nil
	}
	stored, err := json.Marshal(map[string]any{"basedOnHead": head, "summary": summary, "plan": p, "oob": true})
	if err != nil {
		return Result{}, err
	}
	planID, err = in.DB.InsertPlan(oobPrincipal, "", summary, p.OverallRisk.String(), string(stored))
	if err != nil {
		return Result{}, err
	}
	// Approved in the transaction that accepts the commit: never an
	// approved Plan for a revision that was not accepted.
	if _, err := pub.Publish(publish.Request{
		Source: runtimedb.SourceOutOfBand, PlanID: planID, PlanTo: runtimedb.PlanApproved, ResolvedBy: oobPrincipal,
		Expected: marker, Candidate: head,
	}); err != nil {
		if errors.Is(err, publish.ErrSuperseded) {
			_ = in.DB.TransitionPlan(planID, runtimedb.PlanSuperseded, "")
		}
		return Result{}, err
	}
	in.event(log, "statefile.oob-accepted", head, subject)
	return Result{Outcome: OutcomeAccepted, Head: head, PlanID: planID}, nil
}

// checkPolicy validates a candidate tree against the standing invariants
// (ADR-W009 A1). Exemption uses are surfaced as events.
func (in *Ingestor) checkPolicy(log *slog.Logger, head string, proposed, current []resource.Resource) error {
	if in.Policy == nil {
		return nil
	}
	violations, exempted := in.Policy.Validate(policy.Change{
		Proposed: proposed, Current: current, Principal: oobPrincipal,
	})
	for _, v := range exempted {
		in.event(log, "policy.exempted", head, v.String())
	}
	if len(violations) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(violations))
	for _, v := range violations {
		msgs = append(msgs, v.String())
	}
	return fmt.Errorf("violates standing policy: %s", strings.Join(msgs, "; "))
}

// revert restores the last accepted tree with a structured commit naming
// the failure (ADR-W007 A6). The foreign commits stay in history.
func (in *Ingestor) revert(log *slog.Logger, marker, head, subject string, cause error) (Result, error) {
	why := fmt.Sprintf("revert out-of-band edit %.12s (%s): %v", head, subject, cause)
	// Built on top of the foreign commits, with the accepted tree, and
	// fast-forwarded onto the branch: history keeps the human's work, and
	// no working file is rewritten wholesale to get there.
	revert, err := in.State.BuildRevert(marker, head, oobPrincipal, why)
	if err != nil {
		return Result{}, fmt.Errorf("ingest: reverting invalid edit: %w", err)
	}
	res, err := in.publisher(log).Publish(publish.Request{Source: runtimedb.SourceOutOfBand, Expected: marker, Candidate: revert})
	if err != nil {
		return Result{}, fmt.Errorf("ingest: reverting invalid edit: %w", err)
	}
	newHead := res.Revision
	in.event(log, "statefile.oob-reverted", head, cause.Error())
	return Result{Outcome: OutcomeReverted, Head: newHead, Detail: cause.Error()}, nil
}

func (in *Ingestor) event(log *slog.Logger, typ, head, msg string) {
	if _, err := in.DB.AppendEvent(runtimedb.Event{
		Type:    typ,
		Message: msg,
		Attrs:   map[string]string{"head": head},
	}); err != nil {
		log.Error("ingest event append failed", "type", typ, "err", err)
	}
}
