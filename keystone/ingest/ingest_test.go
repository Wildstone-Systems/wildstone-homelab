package ingest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/policy"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

// migrated is the schema, built once for this whole test binary rather
// than replayed into every database these tests open (#231). Each test
// still gets its own file.
var migrated = dbtemplate.New(func(path string) error {
	db, err := runtimedb.Open(path)
	if err != nil {
		return err
	}
	return db.Close()
})

func harness(t *testing.T) (*Ingestor, *statefile.Store) {
	t.Helper()
	db, err := runtimedb.Open(migrated.Path(t))
	if err != nil {
		t.Fatalf("runtimedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	state, err := statefile.New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatalf("statefile.New: %v", err)
	}
	return &Ingestor{State: state, DB: db}, state
}

// humanGit runs git in the statefile repo the way a human would — its own
// identity, no Keystone involvement. This is the "real second git client"
// the ADR's test note calls for.
func humanGit(t *testing.T, root string, args ...string) {
	t.Helper()
	args = append([]string{"-c", "user.name=mika", "-c", "user.email=mika@example.invalid", "-c", "gc.auto=0"}, args...)
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func volume(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindVolume,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/test", Node: "n1"},
		Volume:     &resource.VolumeSpec{Name: name},
	}
}

// seed commits one Keystone-authored resource and marks the head accepted.
func seed(t *testing.T, in *Ingestor, state *statefile.Store) string {
	t.Helper()
	if err := state.Put(volume("data")); err != nil {
		t.Fatal(err)
	}
	if err := state.Commit("test", "plan-0", "seed"); err != nil {
		t.Fatal(err)
	}
	head, err := state.Head()
	if err != nil || head == "" {
		t.Fatalf("head after seed: %q %v", head, err)
	}
	if err := in.DB.SetStatefileHead(head); err != nil {
		t.Fatal(err)
	}
	return head
}

func TestNoChange(t *testing.T) {
	in, state := harness(t)
	// Empty repo: nothing to do.
	res, err := in.Tick()
	if err != nil || res.Outcome != OutcomeNoChange {
		t.Fatalf("empty repo tick = %+v, %v", res, err)
	}
	seed(t, in, state)
	res, err = in.Tick()
	if err != nil || res.Outcome != OutcomeNoChange {
		t.Fatalf("in-sync tick = %+v, %v", res, err)
	}
}

func TestAdoptExistingHistory(t *testing.T) {
	in, state := harness(t)
	if err := state.Put(volume("data")); err != nil {
		t.Fatal(err)
	}
	if err := state.Commit("test", "", "pre-existing"); err != nil {
		t.Fatal(err)
	}
	// Marker never set: first contact adopts.
	res, err := in.Tick()
	if err != nil || res.Outcome != OutcomeAdopted {
		t.Fatalf("tick = %+v, %v", res, err)
	}
	marker, _ := in.DB.StatefileHead()
	if marker != res.Head || marker == "" {
		t.Fatalf("marker %q, head %q", marker, res.Head)
	}
}

func TestValidForeignEditAccepted(t *testing.T) {
	in, state := harness(t)
	seed(t, in, state)

	// A human adds a valid volume directly in git.
	doc, err := resource.Marshal(volume("photos"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state.Root, "volume", "photos.yaml")
	if err := os.WriteFile(path, doc, 0o644); err != nil {
		t.Fatal(err)
	}
	humanGit(t, state.Root, "add", "-A")
	humanGit(t, state.Root, "commit", "-m", "add photos volume by hand")

	res, err := in.Tick()
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Outcome != OutcomeAccepted || res.PlanID == "" {
		t.Fatalf("result = %+v", res)
	}
	rec, err := in.DB.GetPlan(res.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != runtimedb.PlanApproved || rec.Principal != "system/oob" {
		t.Fatalf("plan record = %+v", rec)
	}
	if !strings.Contains(rec.Summary, "add photos volume by hand") {
		t.Fatalf("summary lost the foreign subject: %q", rec.Summary)
	}
	head, _ := state.Head()
	marker, _ := in.DB.StatefileHead()
	if marker != head {
		t.Fatalf("marker %q != head %q", marker, head)
	}
	// Second tick: settled.
	res, err = in.Tick()
	if err != nil || res.Outcome != OutcomeNoChange {
		t.Fatalf("second tick = %+v, %v", res, err)
	}
}

func TestNonResourceEditAcceptedWithoutPlan(t *testing.T) {
	in, state := harness(t)
	seed(t, in, state)
	if err := os.WriteFile(filepath.Join(state.Root, "README.md"), []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	humanGit(t, state.Root, "add", "-A")
	humanGit(t, state.Root, "commit", "-m", "docs only")
	res, err := in.Tick()
	if err != nil || res.Outcome != OutcomeAccepted || res.PlanID != "" {
		t.Fatalf("docs-only tick = %+v, %v (want accepted, no plan)", res, err)
	}
}

func TestInvalidForeignEditReverted(t *testing.T) {
	in, state := harness(t)
	seed(t, in, state)
	before, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}

	// A human commits a resource that fails validation (unknown kind is
	// rejected by strict unmarshal).
	bad := filepath.Join(state.Root, "volume", "bad.yaml")
	if err := os.WriteFile(bad, []byte("apiVersion: wildstone.dev/v1\nkind: Rootkit\nmetadata:\n  name: bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	humanGit(t, state.Root, "add", "-A")
	humanGit(t, state.Root, "commit", "-m", "oops")
	foreignHead, _ := state.Head()

	res, err := in.Tick()
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Outcome != OutcomeReverted || res.Detail == "" {
		t.Fatalf("result = %+v", res)
	}
	// The tree is back to the accepted state, via a NEW commit — history
	// keeps the human's work.
	after, err := state.Load()
	if err != nil {
		t.Fatalf("post-revert tree invalid: %v", err)
	}
	if len(after) != len(before) || after[0].Metadata.Name != "data" {
		t.Fatalf("post-revert tree = %+v", after)
	}
	head, _ := state.Head()
	if head == foreignHead {
		t.Fatal("revert did not create a new commit")
	}
	marker, _ := in.DB.StatefileHead()
	if marker != head {
		t.Fatalf("marker %q != head %q", marker, head)
	}
	// The foreign commit is still reachable in the log.
	subject, err := state.Subject(foreignHead)
	if err != nil || subject != "oops" {
		t.Fatalf("foreign commit lost from history: %q %v", subject, err)
	}
	// Settled afterward.
	res, err = in.Tick()
	if err != nil || res.Outcome != OutcomeNoChange {
		t.Fatalf("post-revert tick = %+v, %v", res, err)
	}
	// The event stream recorded the revert.
	events, err := in.DB.EventsAfter(0, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == "statefile.oob-reverted" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no statefile.oob-reverted event in %+v", events)
	}
}

// TestPolicyInvalidForeignEditReverted pins ADR-W009 A1 on the ingestion
// path: repo access does not outrank policy — a hand-committed,
// schema-valid tree that violates a standing invariant is reverted.
func TestPolicyInvalidForeignEditReverted(t *testing.T) {
	in, state := harness(t)
	in.Policy = policy.NewEngine(nil)
	seed(t, in, state)

	// A human commits a schema-VALID but policy-invalid route: public,
	// auth disabled.
	doc, err := resource.Marshal(resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindRoute,
		Metadata:   resource.Metadata{Name: "cam", Owner: "capability/test", Node: "n1"},
		Route: &resource.RouteSpec{
			Host: "cam.example.com", Upstream: "10.0.0.9:80",
			Auth: "none", Classification: "family_public",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state.Root, "route", "cam.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, doc, 0o644); err != nil {
		t.Fatal(err)
	}
	humanGit(t, state.Root, "add", "-A")
	humanGit(t, state.Root, "commit", "-m", "expose camera by hand")

	res, err := in.Tick()
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Outcome != OutcomeReverted || !strings.Contains(res.Detail, "public-requires-auth") {
		t.Fatalf("result = %+v, want policy revert", res)
	}
	after, err := state.Load()
	if err != nil {
		t.Fatalf("post-revert tree: %v", err)
	}
	for _, r := range after {
		if r.Kind == resource.KindRoute && r.Metadata.Name == "cam" {
			t.Fatalf("policy-invalid route survived the revert: %+v", after)
		}
	}
}

// publicRouteDoc is a schema-valid Route that standing policy refuses:
// public, auth disabled.
func publicRouteDoc(t *testing.T) []byte {
	t.Helper()
	doc, err := resource.Marshal(resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindRoute,
		Metadata:   resource.Metadata{Name: "cam", Owner: "capability/test", Node: "n1"},
		Route: &resource.RouteSpec{
			Host: "cam.example.com", Upstream: "10.0.0.9:80",
			Auth: "none", Classification: "family_public",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The committed tree is what ingestion validates. A bad commit whose
// working copy happens to have been fixed (an uncommitted edit after it)
// is still a bad commit, and is reverted rather than accepted on the
// strength of files nobody committed.
func TestInvalidCommitIsRevertedWhateverTheWorkingFilesSay(t *testing.T) {
	in, state := harness(t)
	seed(t, in, state)

	bad := filepath.Join(state.Root, "volume", "bad.yaml")
	writeFile(t, bad, []byte("apiVersion: wildstone.dev/v1\nkind: Rootkit\nmetadata:\n  name: bad\n"))
	humanGit(t, state.Root, "add", "-A")
	humanGit(t, state.Root, "commit", "-m", "oops")
	// The human notices and deletes the file, but has not committed yet.
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}

	res, err := in.Tick()
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Outcome != OutcomeReverted {
		t.Fatalf("result = %+v, want the committed tree reverted", res)
	}
}

// First adoption is held to standing policy too (contract §4.2): a tree
// first seen is not exempt.
func TestInitialAdoptionAppliesStandingPolicy(t *testing.T) {
	in, state := harness(t)
	in.Policy = policy.NewEngine(nil)
	writeFile(t, filepath.Join(state.Root, "route", "cam.yaml"), publicRouteDoc(t))
	humanGit(t, state.Root, "add", "-A")
	humanGit(t, state.Root, "commit", "-m", "history before Keystone")

	if _, err := in.Tick(); err == nil || !strings.Contains(err.Error(), "public-requires-auth") {
		t.Fatalf("Tick err = %v, want a policy refusal", err)
	}
	if marker, _ := in.DB.StatefileHead(); marker != "" {
		t.Fatalf("a policy-invalid tree was adopted as %q", marker)
	}
}

// An uncommitted valid edit is not an out-of-band change: nothing moved
// HEAD, so nothing is accepted.
func TestUncommittedEditIsNotIngested(t *testing.T) {
	in, state := harness(t)
	seed(t, in, state)
	if err := state.Put(volume("scratch")); err != nil {
		t.Fatal(err)
	}
	res, err := in.Tick()
	if err != nil || res.Outcome != OutcomeNoChange {
		t.Fatalf("Tick = %+v, %v; want no change", res, err)
	}
}

// History that no longer contains the accepted revision (a reset or a
// rebase by hand) is a conflict: nothing is accepted or reverted, and it
// is reported once rather than every tick (contract §4.5).
func TestDivergedHistoryIsAConflictNotAnEdit(t *testing.T) {
	in, state := harness(t)
	accepted := seed(t, in, state)
	// Rewrite history: a new root commit the accepted one is not part of.
	humanGit(t, state.Root, "checkout", "-q", "--orphan", "rewrite")
	writeFile(t, filepath.Join(state.Root, "volume", "other.yaml"), []byte("x: y\n"))
	humanGit(t, state.Root, "add", "-A")
	humanGit(t, state.Root, "commit", "-qm", "start over")

	for i := 0; i < 2; i++ {
		res, err := in.Tick()
		if err != nil || res.Outcome != OutcomeDiverged {
			t.Fatalf("tick %d = %+v, %v; want diverged", i, res, err)
		}
	}
	if marker, _ := in.DB.StatefileHead(); marker != accepted {
		t.Fatalf("a diverged branch moved the accepted revision to %s", marker)
	}
	n := 0
	events, _ := in.DB.EventsAfter(0, 50)
	for _, e := range events {
		if e.Type == "statefile.diverged" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("diverged reported %d times, want once", n)
	}
}

// A branch left behind the accepted revision (a mirror interrupted by a
// crash) is caught up, not mistaken for an edit.
func TestBranchBehindTheAcceptedRevisionIsCaughtUp(t *testing.T) {
	in, state := harness(t)
	seed(t, in, state)
	ahead, err := state.BuildCandidate(mustHead(t, state), []resource.Resource{volume("photos")}, nil, "test", "", "add photos")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.DB.SetStatefileHead(ahead); err != nil {
		t.Fatal(err)
	}
	res, err := in.Tick()
	if err != nil || res.Outcome != OutcomeNoChange {
		t.Fatalf("tick = %+v, %v; want a quiet catch-up", res, err)
	}
	if head := mustHead(t, state); head != ahead {
		t.Fatalf("branch = %s, want caught up to %s", head, ahead)
	}
}

// A revert is a new commit with the accepted tree on top of the foreign
// one, fast-forwarded: an unrelated uncommitted file survives it.
func TestRevertKeepsUnrelatedUncommittedFiles(t *testing.T) {
	in, state := harness(t)
	seed(t, in, state)
	writeFile(t, filepath.Join(state.Root, "volume", "bad.yaml"), []byte("apiVersion: wildstone.dev/v1\nkind: Rootkit\nmetadata:\n  name: bad\n"))
	humanGit(t, state.Root, "add", "-A")
	humanGit(t, state.Root, "commit", "-qm", "oops")
	note := filepath.Join(state.Root, "NOTES.md")
	writeFile(t, note, []byte("mine\n"))

	res, err := in.Tick()
	if err != nil || res.Outcome != OutcomeReverted {
		t.Fatalf("tick = %+v, %v; want reverted", res, err)
	}
	if _, err := os.Stat(note); err != nil {
		t.Fatalf("the revert discarded an unrelated uncommitted file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state.Root, "volume", "bad.yaml")); !os.IsNotExist(err) {
		t.Fatalf("the reverted file is still in the working tree: %v", err)
	}
}

func mustHead(t *testing.T, state *statefile.Store) string {
	t.Helper()
	head, err := state.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head
}
