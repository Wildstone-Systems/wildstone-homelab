package publish

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

var migrated = dbtemplate.New(func(path string) error {
	db, err := runtimedb.Open(path)
	if err != nil {
		return err
	}
	return db.Close()
})

func harness(t *testing.T) *Publisher {
	t.Helper()
	db, err := runtimedb.Open(migrated.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	state, err := statefile.New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatal(err)
	}
	state.Accepted = db.StatefileHead
	return &Publisher{State: state, DB: db}
}

func volume(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindVolume,
		Metadata: resource.Metadata{Name: name, Owner: "capability/test", Node: "n1"},
		Volume:   &resource.VolumeSpec{Name: name},
	}
}

func proposed(t *testing.T, p *Publisher) string {
	t.Helper()
	id, err := p.DB.InsertPlan("op", "", "change", "low", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func planState(t *testing.T, p *Publisher, id string) runtimedb.PlanState {
	t.Helper()
	rec, err := p.DB.GetPlan(id)
	if err != nil {
		t.Fatal(err)
	}
	return rec.State
}

func accepted(t *testing.T, p *Publisher) string {
	t.Helper()
	rev, _, _, err := p.DB.AcceptedRecord()
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

func names(t *testing.T, p *Publisher) []string {
	t.Helper()
	rs, err := p.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rs {
		out = append(out, r.Metadata.Name)
	}
	return out
}

// One publication: the candidate is accepted, the Plan moves in the same
// transaction, the branch catches up and the journal says so.
func TestPublishAcceptsMovesThePlanAndMirrors(t *testing.T) {
	p := harness(t)
	plan := proposed(t, p)
	res, err := p.Publish(Request{
		Source: runtimedb.SourceAPIPlan, PlanID: plan, PlanTo: runtimedb.PlanApproved, ResolvedBy: "owner",
		Put: []resource.Resource{volume("data")}, Who: "owner", Why: "add data",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !res.Changed || accepted(t, p) != res.Revision {
		t.Fatalf("result %+v, accepted %s", res, accepted(t, p))
	}
	if st := planState(t, p, plan); st != runtimedb.PlanApproved {
		t.Fatalf("plan = %s, want approved", st)
	}
	if head, _ := p.State.Head(); head != res.Revision {
		t.Fatalf("branch = %s, want mirrored to %s", head, res.Revision)
	}
	pub, err := p.DB.GetPublication(res.ID)
	if err != nil || pub.State != runtimedb.PublicationMirrored {
		t.Fatalf("journal = %+v, %v", pub, err)
	}
	if target, _ := p.State.RefTarget(statefile.PublicationRefPrefix + res.ID); target != res.Revision {
		t.Fatalf("publication ref = %q, want %s", target, res.Revision)
	}
	_, digest, _, _ := p.DB.AcceptedRecord()
	if tree, _ := p.State.Tree(res.Revision); digest != tree {
		t.Fatalf("recorded digest %s, tree %s", digest, tree)
	}
}

// Two publications computed from the same base: exactly one is accepted;
// the other publishes nothing, its Plan does not move, and its candidate
// ref is gone (contract §7, serialized publication).
func TestSecondPublicationFromTheSameBaseIsSuperseded(t *testing.T) {
	p := harness(t)
	a, b := proposed(t, p), proposed(t, p)
	first, err := p.Publish(Request{Source: runtimedb.SourceAPIPlan, PlanID: a, PlanTo: runtimedb.PlanApproved,
		ResolvedBy: "owner", Put: []resource.Resource{volume("a")}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Publish(Request{Source: runtimedb.SourceAPIPlan, PlanID: b, PlanTo: runtimedb.PlanApproved,
		ResolvedBy: "owner", Put: []resource.Resource{volume("b")}})
	if !errors.Is(err, ErrSuperseded) {
		t.Fatalf("second Publish = %v, want ErrSuperseded", err)
	}
	if accepted(t, p) != first.Revision {
		t.Fatal("the superseded publication moved the accepted revision")
	}
	if st := planState(t, p, b); st != runtimedb.PlanProposed {
		t.Fatalf("superseded plan = %s, want still proposed", st)
	}
	if got := names(t, p); len(got) != 1 || got[0] != "a" {
		t.Fatalf("desired = %v; want only a", got)
	}
	// for-each-ref lists every ref under the prefix: only the winner's.
	refs, _ := p.State.RefTarget(statefile.PublicationRefPrefix)
	if refs != first.Revision {
		t.Fatalf("publication refs left behind: %q", refs)
	}
}

// Interrupted after the candidate ref, before acceptance: recovery
// abandons it, removes its ref, and the accepted revision and the Plan
// are as they were (contract §3).
func TestRecoveryAbandonsAPublicationInterruptedBeforeAcceptance(t *testing.T) {
	p := harness(t)
	plan := proposed(t, p)
	if err := p.DB.PreparePublication(runtimedb.Publication{ID: "pub-crash", Source: runtimedb.SourceAPIPlan, PlanID: plan}); err != nil {
		t.Fatal(err)
	}
	sha, err := p.State.BuildCandidate("", []resource.Resource{volume("data")}, nil, "owner", plan, "add")
	if err != nil {
		t.Fatal(err)
	}
	ref := statefile.PublicationRefPrefix + "pub-crash"
	if err := p.State.CreateRef(ref, sha); err != nil {
		t.Fatal(err)
	}
	tree, _ := p.State.Tree(sha)
	if err := p.DB.SetPublicationCandidate("pub-crash", sha, tree); err != nil {
		t.Fatal(err)
	}
	// Crash here.

	if err := p.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	pub, _ := p.DB.GetPublication("pub-crash")
	if pub.State != runtimedb.PublicationAbandoned {
		t.Fatalf("journal = %s, want abandoned", pub.State)
	}
	if target, _ := p.State.RefTarget(ref); target != "" {
		t.Fatalf("interrupted candidate ref survived: %s", target)
	}
	if accepted(t, p) != "" || planState(t, p, plan) != runtimedb.PlanProposed {
		t.Fatal("an unaccepted publication changed the accepted revision or its Plan")
	}
}

// Interrupted after acceptance, before the branch caught up: the accepted
// revision is already authoritative, and recovery finishes the mirror.
func TestRecoveryFinishesAMirrorInterruptedAfterAcceptance(t *testing.T) {
	p := harness(t)
	if err := p.DB.PreparePublication(runtimedb.Publication{ID: "pub-mirror", Source: runtimedb.SourceAPIPlan}); err != nil {
		t.Fatal(err)
	}
	sha, _ := p.State.BuildCandidate("", []resource.Resource{volume("data")}, nil, "owner", "", "add")
	if err := p.State.CreateRef(statefile.PublicationRefPrefix+"pub-mirror", sha); err != nil {
		t.Fatal(err)
	}
	tree, _ := p.State.Tree(sha)
	if err := p.DB.SetPublicationCandidate("pub-mirror", sha, tree); err != nil {
		t.Fatal(err)
	}
	if err := p.DB.AcceptPublication("pub-mirror", "", ""); err != nil {
		t.Fatal(err)
	}
	// Crash here: accepted, not mirrored.
	if got := names(t, p); len(got) != 1 {
		t.Fatalf("accepted state not readable before the mirror: %v", got)
	}

	if err := p.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if head, _ := p.State.Head(); head != sha {
		t.Fatalf("branch = %s, want %s", head, sha)
	}
	if pub, _ := p.DB.GetPublication("pub-mirror"); pub.State != runtimedb.PublicationMirrored {
		t.Fatalf("journal = %s, want mirrored", pub.State)
	}
}

// An accepted revision that is not in the repository (a restore whose
// bundle lacks it) is an error, not a fallback.
func TestRecoveryRefusesAMissingAcceptedRevision(t *testing.T) {
	p := harness(t)
	if err := p.DB.SetStatefileHead(strings.Repeat("ab", 20)); err != nil {
		t.Fatal(err)
	}
	if err := p.Recover(); err == nil || !strings.Contains(err.Error(), "not in the Statefile repository") {
		t.Fatalf("Recover = %v, want a missing-revision error", err)
	}
}

// A publication is built from the accepted tree: an uncommitted file in
// the working tree is neither in the commit nor lost by the mirror.
func TestPublishLeavesUncommittedEditsOut(t *testing.T) {
	p := harness(t)
	if _, err := p.Publish(Request{Source: runtimedb.SourceAPIPlan, Put: []resource.Resource{volume("a")}}); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(p.State.Root, "NOTES.md")
	if err := os.WriteFile(note, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := p.Publish(Request{Source: runtimedb.SourceAPIPlan, Expected: accepted(t, p), Put: []resource.Resource{volume("b")}})
	if err != nil {
		t.Fatal(err)
	}
	files, err := p.State.LoadAt(res.Revision)
	if err != nil || len(files) != 2 {
		t.Fatalf("published tree = %+v, %v", files, err)
	}
	if _, err := os.Stat(note); err != nil {
		t.Fatalf("the mirror discarded an uncommitted file: %v", err)
	}
}
