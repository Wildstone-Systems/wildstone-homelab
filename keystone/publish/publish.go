// Package publish is the one coordinator for desired-state publication
// (ADR-W007 A8, contract D2/D3): API Plan approval, the household tracker
// writer and out-of-band ingestion all publish through it, rather than
// each writing files, committing, moving statefile_head and transitioning
// a Plan as separate steps.
//
// A publication is journaled before anything else, its candidate is
// built as a commit object from the accepted tree (never the working
// files) and made reachable by an internal ref, and it is accepted in one
// runtime-DB transaction that compares the expected revision, selects the
// candidate and moves the Plan. The branch then catches up by
// fast-forward. A crash at any point recovers to either the old or the
// new accepted revision; never to a mixed tree, and never to a Plan
// recorded as approved for a change that was not accepted.
package publish

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

// ErrSuperseded: another publication was accepted first. Nothing of this
// one was published; it must be reviewed again against the new revision.
var ErrSuperseded = runtimedb.ErrRevisionMoved

// Publisher serializes publications within the process; the journal and
// the acceptance transaction fence them across restarts.
type Publisher struct {
	State *statefile.Store
	DB    *runtimedb.DB
	Log   *slog.Logger

	mu sync.Mutex
}

// Request is one publication.
type Request struct {
	Source string // runtimedb.Source*
	// PlanID, when set, moves to PlanTo in the acceptance transaction.
	PlanID     string
	PlanTo     runtimedb.PlanState
	ResolvedBy string
	// Expected is the accepted revision the change was computed against
	// ("" for an empty repository).
	Expected string

	// The change to build from Expected...
	Put    []resource.Resource
	Delete []statefile.ResourceRef
	Who    string
	Why    string
	// ...or an existing commit to accept as it is (out-of-band).
	Candidate string
}

// Result is an accepted publication.
type Result struct {
	ID       string
	Revision string // the accepted revision now; Expected when nothing changed
	Changed  bool
}

func (p *Publisher) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// Publish journals, builds, accepts and mirrors one change. ErrSuperseded
// means a concurrent publication won and this one changed nothing.
func (p *Publisher) Publish(req Request) (Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.State.Accepted == nil && req.Source != runtimedb.SourceOutOfBand {
		// No accepted-revision source (test fixtures that write the working
		// tree directly): there the working files are desired state by
		// definition, as Load reads them, so they are committed and adopted
		// as what an API writer's comparison starts from — what the
		// pre-journal writers' `git add -A` did. Ingestion is exempt: a
		// HEAD that differs from the record is exactly what it judges.
		// Production always wires a source, and never takes this branch.
		expected, err := p.adoptWorkingTree(req.Expected)
		if err != nil {
			return Result{}, err
		}
		req.Expected = expected
	}
	id, err := newID()
	if err != nil {
		return Result{}, err
	}
	if err := p.DB.PreparePublication(runtimedb.Publication{
		ID: id, Source: req.Source, PlanID: req.PlanID, ExpectedRevision: req.Expected,
	}); err != nil {
		return Result{}, err
	}
	abandon := func(cause error) error {
		if err := p.DB.MarkPublication(id, runtimedb.PublicationAbandoned, cause.Error()); err != nil {
			p.log().Error("abandoning a publication", "publication", id, "err", err)
		}
		return cause
	}

	candidate := req.Candidate
	if candidate == "" {
		candidate, err = p.State.BuildCandidate(req.Expected, req.Put, req.Delete, req.Who, req.PlanID, req.Why)
		if err != nil {
			return Result{}, abandon(err)
		}
	} else if !p.State.HasCommit(candidate) {
		return Result{}, abandon(fmt.Errorf("publish: candidate %s is not a commit here", candidate))
	}
	changed := candidate != req.Expected
	ref := statefile.PublicationRefPrefix + id
	if changed {
		// Reachable before it can be selected: the ref is what keeps the
		// candidate alive through gc and into a recovery bundle.
		if err := p.State.CreateRef(ref, candidate); err != nil {
			return Result{}, abandon(err)
		}
	}
	digest, err := p.State.Tree(candidate)
	if err != nil {
		return Result{}, abandon(err)
	}
	if err := p.DB.SetPublicationCandidate(id, candidate, digest); err != nil {
		return Result{}, abandon(err)
	}

	if err := p.DB.AcceptPublication(id, req.PlanTo, req.ResolvedBy); err != nil {
		if errors.Is(err, runtimedb.ErrRevisionMoved) && changed {
			if derr := p.State.DeleteRef(ref, candidate); derr != nil {
				p.log().Warn("removing a superseded candidate ref", "ref", ref, "err", derr)
			}
		}
		// Anything else leaves the row prepared: it granted nothing, and
		// recovery abandons it.
		return Result{}, err
	}

	p.mirror(id, candidate)
	return Result{ID: id, Revision: candidate, Changed: changed}, nil
}

// mirror brings the branch to the accepted revision. A failure is not a
// failed publication: the revision is already accepted, and recovery or
// the next publication retries the mirror.
func (p *Publisher) mirror(id, revision string) {
	if err := p.State.Mirror(revision); err != nil {
		p.log().Warn("statefile branch not mirrored to the accepted revision; it will be retried",
			"publication", id, "revision", revision, "err", err)
		return
	}
	if err := p.DB.MarkPublication(id, runtimedb.PublicationMirrored, ""); err != nil {
		p.log().Error("recording a mirrored publication", "publication", id, "err", err)
	}
}

// Recover runs before reconciliation, ingestion or any new publication.
// It verifies the accepted revision against its recorded tree digest,
// abandons publications interrupted before acceptance (they granted
// nothing) and finishes mirrors interrupted after it. A missing or
// mismatched accepted revision is returned as an error: the caller keeps
// desired state unreadable rather than fall back to the working files or
// a nearby commit.
func (p *Publisher) Recover() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	rev, digest, _, err := p.DB.AcceptedRecord()
	if err != nil {
		return err
	}
	if rev != "" {
		if !p.State.HasCommit(rev) {
			return fmt.Errorf("publish: the accepted revision %s is not in the Statefile repository", rev)
		}
		if digest != "" {
			tree, err := p.State.Tree(rev)
			if err != nil {
				return err
			}
			if tree != digest {
				return fmt.Errorf("publish: the accepted revision %s has tree %s, recorded %s", rev, tree, digest)
			}
		}
	}

	open, err := p.DB.OpenPublications()
	if err != nil {
		return err
	}
	for _, pub := range open {
		switch pub.State {
		case runtimedb.PublicationPrepared:
			if pub.CandidateRevision != "" && pub.CandidateRevision != pub.ExpectedRevision {
				ref := statefile.PublicationRefPrefix + pub.ID
				if target, _ := p.State.RefTarget(ref); target == pub.CandidateRevision {
					if err := p.State.DeleteRef(ref, target); err != nil {
						p.log().Warn("removing an interrupted candidate ref", "ref", ref, "err", err)
					}
				}
			}
			if err := p.DB.MarkPublication(pub.ID, runtimedb.PublicationAbandoned, "interrupted before acceptance"); err != nil {
				return err
			}
			p.log().Info("abandoned a publication interrupted before acceptance", "publication", pub.ID, "plan", pub.PlanID)
		case runtimedb.PublicationAccepted:
			if pub.CandidateRevision != rev {
				// A later publication superseded it; only the latest is
				// mirrored.
				if err := p.DB.MarkPublication(pub.ID, runtimedb.PublicationMirrored, "superseded before its mirror"); err != nil {
					return err
				}
				continue
			}
			p.mirror(pub.ID, rev)
		}
	}
	return nil
}

// adoptWorkingTree commits the working files and records the result as
// accepted, returning the revision a writer that read them should be
// compared from. A writer computed against the branch HEAD read HEAD plus
// its working files, so it is compared from the adopted commit; any other
// expected revision is left alone, so a stale writer still loses.
func (p *Publisher) adoptWorkingTree(expected string) (string, error) {
	before, err := p.State.Head()
	if err != nil {
		return "", err
	}
	if err := p.State.Commit("fixture", "", "adopt the working tree"); err != nil {
		return "", err
	}
	head, err := p.State.Head()
	if err != nil || head == "" {
		return expected, err
	}
	if current, err := p.DB.StatefileHead(); err != nil {
		return "", err
	} else if current != head {
		if err := p.DB.SetStatefileHead(head); err != nil {
			return "", err
		}
	}
	if expected == before {
		return head, nil
	}
	return expected, nil
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("publish: publication id: %w", err)
	}
	return "pub-" + hex.EncodeToString(b[:]), nil
}
