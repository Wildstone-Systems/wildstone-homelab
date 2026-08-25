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
	"fmt"
	"log/slog"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
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

	// First contact with a repo that already has history: validate and
	// adopt as baseline. There is no prior accepted tree to revert to, so
	// an invalid tree here is an error the operator must fix, not a
	// revert.
	if marker == "" {
		if _, err := in.State.Load(); err != nil {
			in.event(log, "statefile.invalid", head, err.Error())
			return Result{}, fmt.Errorf("ingest: initial statefile tree is invalid: %w", err)
		}
		if err := in.DB.SetStatefileHead(head); err != nil {
			return Result{}, err
		}
		in.event(log, "statefile.adopted", head, "")
		return Result{Outcome: OutcomeAdopted, Head: head}, nil
	}

	subject, err := in.State.Subject(head)
	if err != nil {
		return Result{}, err
	}

	// Validate the foreign tree. Load parses AND validates via
	// resource.Unmarshal (strict YAML + Validate), so any schema failure
	// lands here.
	newTree, err := in.State.Load()
	if err != nil {
		return in.revert(log, marker, head, subject, err)
	}

	oldTree, err := in.State.LoadAt(marker)
	if err != nil {
		return Result{}, fmt.Errorf("ingest: loading accepted tree %s: %w", marker, err)
	}

	// Standing invariants (ADR-W009 A1): repo access does not outrank
	// policy — a policy-invalid hand edit is reverted like a
	// schema-invalid one. Exemption uses are surfaced as events.
	if in.Policy != nil {
		violations, exempted := in.Policy.Validate(policy.Change{
			Proposed: newTree, Current: oldTree, Principal: oobPrincipal,
		})
		for _, v := range exempted {
			in.event(log, "policy.exempted", head, v.String())
		}
		if len(violations) > 0 {
			msgs := make([]string, 0, len(violations))
			for _, v := range violations {
				msgs = append(msgs, v.String())
			}
			perr := fmt.Errorf("violates standing policy: %s", strings.Join(msgs, "; "))
			return in.revert(log, marker, head, subject, perr)
		}
	}

	// Valid: record the delta as an auto-approved plan — approved because
	// the edit is already the committed desired state (whoever made it
	// holds repo access, an authority senior to any API token — though
	// not senior to policy, above); the plan record gives the change the
	// same audit/review/reconcile surface as an API-proposed one, and
	// E3's reconciler converges from it.
	p := plan.Diff(newTree, oldTree)
	summary := fmt.Sprintf("out-of-band edit %.12s: %s", head, subject)
	p.Summary = summary
	planID := ""
	if len(p.Steps) == 0 {
		// Commit(s) that don't change the resource tree (docs, comments,
		// whitespace): accept silently, no plan needed.
		if err := in.DB.SetStatefileHead(head); err != nil {
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
	if err := in.DB.TransitionPlan(planID, runtimedb.PlanApproved, oobPrincipal); err != nil {
		return Result{}, err
	}
	if err := in.DB.SetStatefileHead(head); err != nil {
		return Result{}, err
	}
	in.event(log, "statefile.oob-accepted", head, subject)
	return Result{Outcome: OutcomeAccepted, Head: head, PlanID: planID}, nil
}

// revert restores the last accepted tree with a structured commit naming
// the failure (ADR-W007 A6). The foreign commits stay in history.
func (in *Ingestor) revert(log *slog.Logger, marker, head, subject string, cause error) (Result, error) {
	why := fmt.Sprintf("revert out-of-band edit %.12s (%s): %v", head, subject, cause)
	if rerr := in.State.RevertTo(marker, oobPrincipal, why); rerr != nil {
		return Result{}, fmt.Errorf("ingest: reverting invalid edit: %w", rerr)
	}
	newHead, herr := in.State.Head()
	if herr != nil {
		return Result{}, herr
	}
	if err := in.DB.SetStatefileHead(newHead); err != nil {
		return Result{}, err
	}
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
