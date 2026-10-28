package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/reconcile"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// Bounded approval (ADR-W008 A2a, contract D4/D5).
//
// An approved Plan used to make its tick a "plan tick", and a plan tick
// authorized every Medium/High step anywhere in the tree. Approving one
// container update therefore also applied whatever unrelated Medium/High
// drift happened to be pending on any node: approval of one change was
// authority for all of them.
//
// Now each queued Plan contributes the steps it reviewed — resource,
// action, digest, risk — and a Medium/High step runs only when one of
// them covers it exactly. Everything else is held and proposed as its
// own exact drift preview. Each Plan then gets its own verdict from what
// happened to its own resources.

// entry is one reviewed step: the exact effect a Plan's approval permits.
type entry struct {
	kind    resource.Kind
	name    string
	action  plan.Action
	digest  string // the accepted content a create/update produces; "" for a delete
	backend string
}

func resKey(kind resource.Kind, name string) string { return string(kind) + "/" + name }

// queuedPlan is one approved or applying Plan and the entries it brings.
type queuedPlan struct {
	id      string
	entries []entry
}

// authority is the union of every queued Plan's entries for one tick,
// against the accepted revision's digests.
type authority struct {
	plans    []queuedPlan
	accepted map[string]string // resKey -> Digest at the accepted revision
}

func newAuthority(records []runtimedb.PlanRecord, accepted map[string]string) (*authority, error) {
	a := &authority{accepted: accepted}
	for _, rec := range records {
		var stored plan.StoredProposal
		if err := json.Unmarshal([]byte(rec.PlanJSON), &stored); err != nil {
			return nil, fmt.Errorf("loop: decoding queued plan %s: %w", rec.ID, err)
		}
		q := queuedPlan{id: rec.ID}
		if stored.Plan != nil {
			for _, st := range stored.Plan.Steps {
				q.entries = append(q.entries, entry{
					kind: st.Kind, name: st.Name, action: st.Action, digest: st.Digest, backend: st.Backend,
				})
			}
		}
		a.plans = append(a.plans, q)
	}
	return a, nil
}

// covers is Reconciler.Authorize: whether some queued Plan reviewed
// exactly this step. Same resource and action, and — for a create or
// update — the content the accepted revision holds now is the content
// that was reviewed. A delete is covered only while the accepted revision
// no longer holds the resource.
//
// Risk is not compared. The reviewer saw risk graded between two desired
// states; the reconciler grades the same step against what it observed,
// and an out-of-date container observes as an empty placeholder, so an
// image update to a container with mounts reads as a High "mount change"
// at reconcile time though nothing about its mounts changed. Requiring
// the reconcile grade to stay within the reviewed one would hold every
// such update forever. The digest is the binding: it pins exactly the
// content the reviewer approved.
//
// A reviewed update never covers a recomputed create or delete (the
// strict action rule): the effect differs even when the spec does not.
// A step recorded before digests existed covers nothing; migration does
// not invent authority for work nobody can show was reviewed.
func (a *authority) covers(st plan.PlanStep) bool {
	k := resKey(st.Kind, st.Name)
	current, stillDesired := a.accepted[k]
	for _, p := range a.plans {
		for _, e := range p.entries {
			if e.kind != st.Kind || e.name != st.Name || e.backend != st.Backend || !sameEffect(st.Kind, e.action, st.Action) {
				continue
			}
			if st.Action == plan.ActionDelete {
				if !stillDesired {
					return true
				}
				continue
			}
			if e.digest != "" && e.digest == current {
				return true
			}
		}
	}
	return false
}

// grant names the exact queued Plan step that authorizes a cluster write.
// A delete has no desired-resource digest; its stable step digest binds
// the reviewed (plan, kind, name, backend, delete) tuple instead.
func (a *authority) grant(st plan.PlanStep) (planID, digest string, ok bool) {
	if !a.covers(st) {
		return "", "", false
	}
	for _, p := range a.plans {
		for _, e := range p.entries {
			if e.kind != st.Kind || e.name != st.Name || e.backend != st.Backend || !sameEffect(st.Kind, e.action, st.Action) {
				continue
			}
			if st.Action != plan.ActionDelete {
				if e.digest == a.accepted[resKey(st.Kind, st.Name)] {
					return p.id, e.digest, true
				}
				continue
			}
			sum := sha256.Sum256([]byte(p.id + "\x00" + string(st.Kind) + "\x00" + st.Name + "\x00" + st.Backend + "\x00Delete"))
			return p.id, "sha256:" + hex.EncodeToString(sum[:]), true
		}
	}
	return "", "", false
}

func (a *authority) clusterDelete(kind resource.Kind, name string) bool {
	for _, p := range a.plans {
		for _, e := range p.entries {
			if e.kind == kind && e.name == name && e.action == plan.ActionDelete && e.backend == "k3s" {
				return true
			}
		}
	}
	return false
}

// sameEffect reports whether a reviewed action and a reconcile action are
// the same effect. They must be equal — a reviewed update does not cover
// a recomputed create or delete — with one exception: a Job has no
// create. Its observe reports an absent run and a stale one alike as
// present-but-different, so the reconciler always plans an Update, which
// means "run this spec". The review saw a Create of the same content; both
// are the one effect of running that digest once.
func sameEffect(kind resource.Kind, reviewed, planned plan.Action) bool {
	if reviewed == planned {
		return true
	}
	return kind == resource.KindJob && reviewed == plan.ActionCreate && planned == plan.ActionUpdate
}

// reviewedAny reports whether any of steps is one a queued Plan
// reviewed, which is what makes an applied pass that Plan's work rather
// than drift repair.
func (a *authority) reviewedAny(steps []plan.PlanStep) bool {
	for _, st := range steps {
		for _, p := range a.plans {
			for _, e := range p.entries {
				if e.kind == st.Kind && e.name == st.Name && sameEffect(st.Kind, e.action, st.Action) {
					return true
				}
			}
		}
	}
	return false
}

// acceptedDigests maps every resource at the accepted revision to its
// digest. Read before projection, so the map describes the Statefile and
// nothing the runtime DB added to the copy.
func acceptedDigests(desired []resource.Resource) map[string]string {
	out := make(map[string]string, len(desired))
	for _, r := range desired {
		out[resKey(r.Kind, r.Metadata.Name)] = plan.Digest(r)
	}
	return out
}

// nodeResult is what one node's pass did, in the terms a Plan verdict
// needs.
type nodeResult struct {
	deferred bool            // offline, or an inconclusive check: retry later
	failed   string          // incident, rollback, or a pass that errored
	pending  bool            // held by Medium/High steps nobody approved
	planned  map[string]bool // resKeys the pass planned a step for
	done     map[string]bool // resKeys whose step applied and passed its gate
}

func (nr *nodeResult) record(o *reconcile.Outcome) {
	nr.planned = map[string]bool{}
	nr.done = map[string]bool{}
	for _, st := range o.Plan.Steps {
		nr.planned[resKey(st.Kind, st.Name)] = true
	}
	for _, sr := range o.Steps {
		if sr.Error == "" && sr.HealthOK {
			nr.done[resKey(sr.Step.Kind, sr.Step.Name)] = true
		}
	}
}

// resourceNodes says which node each resource lives on: the node a pass
// planned it on, else the node desired state places it, else the node its
// observed row records (a delete target, which desired no longer holds).
// A resource in none of them has already gone and needs nothing.
func resourceNodes(placements map[string]string, desired []resource.Resource, results map[string]*nodeResult) map[string]string {
	homes := map[string]string{}
	for k, node := range placements {
		homes[k] = node
	}
	for _, r := range desired {
		node := r.Metadata.Node
		if reconcile.TargetOf(r).Kind == reconcile.TargetCluster {
			node = "@cluster"
		}
		homes[resKey(r.Kind, r.Metadata.Name)] = node
	}
	for node, nr := range results {
		for k := range nr.planned {
			homes[k] = node
		}
	}
	return homes
}

type verdict int

const (
	verdictApplied verdict = iota
	verdictWait
	verdictFailed
)

// verdict is this Plan's own outcome. It waits while any of its
// resources is on a node that was deferred, or held its step for
// approval; it failed when one of its resources had a step that did not
// apply; it applied when every one of its resources is converged. Work
// on resources it did not review never decides it.
func (p queuedPlan) verdict(homes map[string]string, results map[string]*nodeResult) verdict {
	out := verdictApplied
	for _, e := range p.entries {
		k := resKey(e.kind, e.name)
		node, ok := homes[k]
		if !ok {
			continue
		}
		nr := results[node]
		if nr == nil {
			continue
		}
		switch {
		case nr.deferred:
			return verdictWait
		case nr.done[k]:
		case nr.failed != "" && (nr.planned == nil || nr.planned[k]):
			out = verdictFailed
		case nr.pending && nr.planned[k]:
			return verdictWait
		}
	}
	return out
}
