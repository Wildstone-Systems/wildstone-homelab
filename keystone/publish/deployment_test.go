package publish

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

func deploymentHarness(t *testing.T) (*DeploymentPublisher, DeploymentRequest) {
	t.Helper()
	db, err := runtimedb.Open(migrated.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.AcquireCoordinator("example-coordinator"); err != nil {
		t.Fatal(err)
	}
	rev := strings.Repeat("a", 40)
	if err := db.SetStatefileHead(rev); err != nil {
		t.Fatal(err)
	}
	repo, err := statefile.OpenLocalDeployment(filepath.Join(t.TempDir(), "deployment.git"))
	if err != nil {
		t.Fatal(err)
	}
	p := &DeploymentPublisher{Repo: repo, DB: db, Gate: func(r DeploymentRequest) error {
		if r.PlanID != "plan-1" {
			return errors.New("not approved")
		}
		return nil
	}}
	r := DeploymentRequest{OperationID: "example-op", PlanID: "plan-1", AcceptedRevision: rev, ReviewDigest: strings.Repeat("b", 64), Files: map[string][]byte{"source.yaml": []byte("example source\n"), "release.yaml": []byte("example release\n"), "kustomization.yaml": []byte("example list\n")}}
	return p, r
}

func TestDeploymentPromotionCrashRecovery(t *testing.T) {
	for _, phase := range []string{"prepared", "promoted"} {
		t.Run(phase, func(t *testing.T) {
			p, r := deploymentHarness(t)
			p.interrupt = func(at string) error {
				if at == phase {
					return errors.New("simulated interruption")
				}
				return nil
			}
			first, err := p.Promote(r)
			if err == nil {
				t.Fatal("expected interruption")
			}
			head, _ := p.Repo.Head()
			if phase == "prepared" && head != "" {
				t.Fatal("prepared candidate visible")
			}
			if phase == "promoted" && head != first.CandidateRevision {
				t.Fatal("missing promoted ref")
			}
			// New publisher has no in-memory operation state.
			restarted := &DeploymentPublisher{Repo: p.Repo, DB: p.DB, Gate: p.Gate}
			got, err := restarted.Promote(r)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != "promoted" || got.CandidateRevision != first.CandidateRevision {
				t.Fatal("recovery changed candidate")
			}
			again, err := restarted.Promote(r)
			if err != nil || again.CandidateRevision != got.CandidateRevision {
				t.Fatal("retry not idempotent", err)
			}
		})
	}
}

func TestDeploymentPromotionRefusesChangedOrUnapprovedWork(t *testing.T) {
	for _, change := range []string{"missing-gate", "unapproved", "stale", "changed-review", "changed-files", "owner-edit", "ownership-ref-change", "lost-authority", "invalid-files"} {
		t.Run(change, func(t *testing.T) {
			p, r := deploymentHarness(t)
			if change == "changed-review" || change == "changed-files" || change == "owner-edit" || change == "ownership-ref-change" {
				p.interrupt = func(string) error { return errors.New("pause") }
				if _, err := p.Promote(r); err == nil {
					t.Fatal("pause failed")
				}
				p.interrupt = nil
			}
			switch change {
			case "missing-gate":
				p.Gate = nil
			case "unapproved":
				r.PlanID = "plan-2"
			case "stale":
				r.AcceptedRevision = strings.Repeat("c", 40)
			case "changed-review":
				r.ReviewDigest = strings.Repeat("d", 64)
			case "changed-files":
				r.Files["release.yaml"] = []byte("altered")
			case "owner-edit", "ownership-ref-change":
				c, err := p.Repo.Build("", "owner-change", r.Files)
				if err != nil {
					t.Fatal(err)
				}
				if err := p.Repo.Promote("", c); err != nil {
					t.Fatal(err)
				}
			case "lost-authority":
				if err := p.DB.Close(); err != nil {
					t.Fatal(err)
				}
			case "invalid-files":
				delete(r.Files, "source.yaml")
				r.Files["../escape"] = []byte("refused")
			}
			before, _ := p.Repo.Head()
			if _, err := p.Promote(r); err == nil {
				t.Fatal("unsafe change accepted")
			}
			after, _ := p.Repo.Head()
			if before != after {
				t.Fatal("ref changed after refusal")
			}
		})
	}
}

func TestDeploymentConcurrentPublishers(t *testing.T) {
	p, r := deploymentHarness(t)
	other := &DeploymentPublisher{Repo: p.Repo, DB: p.DB, Gate: p.Gate}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, pub := range []*DeploymentPublisher{p, other} {
		wg.Add(1)
		go func(pub *DeploymentPublisher) { defer wg.Done(); _, err := pub.Promote(r); errs <- err }(pub)
	}
	wg.Wait()
	close(errs)
	// A CAS loser may report a conflict; retry must converge on the one journal.
	for err := range errs {
		if err != nil && !errors.Is(err, statefile.ErrDiverged) {
			t.Fatal("unexpected concurrent failure", err)
		}
	}
	got, err := p.Promote(r)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := p.Repo.Head()
	if head != got.CandidateRevision {
		t.Fatal("not journal candidate")
	}
}

func TestDeploymentSourceCheckoutAndRemotesRefused(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := statefile.OpenLocalDeployment(filepath.Join(root, "deployment")); err == nil {
		t.Fatal("source checkout accepted")
	}
	root = filepath.Join(t.TempDir(), "deployment.git")
	repo, err := statefile.OpenLocalDeployment(root)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "--git-dir="+root, "remote", "add", "origin", "https://example.invalid/not-used.git")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if _, err := repo.Head(); err == nil {
		t.Fatal("remote accepted")
	}
}
