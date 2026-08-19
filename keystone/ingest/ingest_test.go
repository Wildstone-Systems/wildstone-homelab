package ingest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

func harness(t *testing.T) (*Ingestor, *statefile.Store) {
	t.Helper()
	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "keystone.db"))
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
