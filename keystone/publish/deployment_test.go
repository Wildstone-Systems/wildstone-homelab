package publish

import (
	"encoding/json"
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
	for _, member := range []string{"example-owner", "example-other-owner"} {
		if err := db.RecordButlerMember(member, runtimedb.RoleOwner, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember(member, "example-admin"); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := db.GetButlerMember("example-owner")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := deploymentBinding(repo.Identity(), r)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(runtimedb.DeploymentPlanDocument{BasedOnHead: rev, Summary: "Example deployment", Deployment: &runtimedb.DeploymentApproval{Binding: binding, SourceRevision: rev, OwnerPersonID: owner.PersonID, OwnerGeneration: owner.Generation}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertPlan("example-owner", "", "Example deployment", "high", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if id != r.PlanID {
		t.Fatal("unexpected fixture Plan ID")
	}
	if err := db.TransitionPlan(id, runtimedb.PlanApproved, "example-owner"); err != nil {
		t.Fatal(err)
	}
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
	for _, change := range []string{"missing-gate", "unapproved", "stale", "changed-review", "changed-files", "owner-edit", "ownership-ref-change", "lost-authority", "invalid-files", "missing-private-bytes", "changed-private-bytes", "arbitrary-private-path"} {
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
			case "missing-private-bytes":
				r.PrivateFileDigests = map[string]string{"secret.sops.yaml": strings.Repeat("a", 64)}
			case "changed-private-bytes":
				r.PrivateFileDigests = map[string]string{"secret.sops.yaml": strings.Repeat("a", 64)}
				r.PrivateFiles = map[string][]byte{"secret.sops.yaml": []byte("different ciphertext")}
			case "arbitrary-private-path":
				r.PrivateFileDigests = map[string]string{"../secret.sops.yaml": strings.Repeat("a", 64)}
				r.PrivateFiles = map[string][]byte{"../secret.sops.yaml": []byte("ciphertext")}
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

func TestDeploymentBindingUsesPrivateDigestNotPrivateBytes(t *testing.T) {
	_, req := deploymentHarness(t)
	req.PrivateFileDigests = map[string]string{"secret.sops.yaml": strings.Repeat("c", 64)}
	req.PrivateFiles = map[string][]byte{"secret.sops.yaml": []byte("first private bytes")}
	a, err := DeploymentBinding(strings.Repeat("d", 64), req)
	if err != nil {
		t.Fatal(err)
	}
	req.PrivateFiles["secret.sops.yaml"] = []byte("other private bytes")
	b, err := DeploymentBinding(strings.Repeat("d", 64), req)
	if err != nil || a != b {
		t.Fatal("binding serialized private bytes", err)
	}
	req.PrivateFileDigests["secret.sops.yaml"] = strings.Repeat("e", 64)
	c, err := DeploymentBinding(strings.Repeat("d", 64), req)
	if err != nil || c == a {
		t.Fatal("binding omitted private file identity", err)
	}
}

func TestDeploymentGateReceivesPrivateIdentityOnly(t *testing.T) {
	p, req := deploymentHarness(t)
	req.PrivateFileDigests = map[string]string{"secret.sops.yaml": strings.Repeat("c", 64)}
	req.PrivateFiles = map[string][]byte{"secret.sops.yaml": []byte("private ciphertext")}
	p.Gate = func(got DeploymentRequest) error {
		if got.PrivateFiles != nil || got.PrivateFileDigests["secret.sops.yaml"] != strings.Repeat("c", 64) {
			t.Fatal("gate received ciphertext or lost its safe identity")
		}
		return nil
	}
	if err := p.authorize(req); err != nil {
		t.Fatal(err)
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

// These changes occur after the publisher's preliminary authorization check.
// Only its mandatory SQLite approval fence can prevent the prepared ref effect.
func TestDeploymentExactPlanBoundary(t *testing.T) {
	for _, change := range []string{"approved", "revoked-at-boundary", "superseded-at-boundary", "new-source-at-boundary", "new-coordinator-at-boundary", "other-instance", "changed-secret-output"} {
		t.Run(change, func(t *testing.T) {
			p, req := deploymentHarness(t)
			p.interrupt = func(string) error { return errors.New("prepared fixture") }
			prepared, err := p.Promote(req)
			if err == nil {
				t.Fatal("missing interruption")
			}
			p.interrupt = nil
			calls := 0
			p.Gate = func(DeploymentRequest) error {
				calls++
				if calls != 2 {
					return nil
				}
				switch change {
				case "revoked-at-boundary":
					return p.DB.DisableButlerMember("example-owner", "example-other-owner")
				case "superseded-at-boundary":
					return p.DB.TransitionPlan(req.PlanID, runtimedb.PlanSuperseded, "")
				case "new-source-at-boundary":
					return p.DB.SetStatefileHead(strings.Repeat("c", 40))
				case "new-coordinator-at-boundary":
					_, err := p.DB.PromoteCoordinator("example-successor")
					return err
				}
				return nil
			}
			switch change {
			case "other-instance":
				p.Repo, err = statefile.OpenLocalDeployment(filepath.Join(t.TempDir(), "other.git"))
				if err != nil {
					t.Fatal(err)
				}
			case "changed-secret-output":
				req.Files["release.yaml"] = []byte("example changed reference version")
			}
			got, err := p.Promote(req)
			head, headErr := p.Repo.Head()
			if headErr != nil {
				t.Fatal(headErr)
			}
			if change != "approved" {
				if err == nil || head != "" {
					t.Fatalf("unsafe promotion: head=%q err=%v", head, err)
				}
				return
			}
			if err != nil || head != prepared.CandidateRevision || got.State != "promoted" {
				t.Fatal("approved exact output did not promote", err)
			}
			restarted := &DeploymentPublisher{Repo: p.Repo, DB: p.DB, Gate: p.Gate}
			if _, err := restarted.Promote(req); err != nil {
				t.Fatal("approved exact retry failed", err)
			}
		})
	}
}

func TestDeploymentRevocationAfterRefAdvancePreservesUnknownOutcome(t *testing.T) {
	p, req := deploymentHarness(t)
	p.interrupt = func(phase string) error {
		if phase != "promoted" {
			return nil
		}
		if err := p.DB.DisableButlerMember("example-owner", "example-other-owner"); err != nil {
			return err
		}
		return errors.New("example interruption before journal acknowledgment")
	}
	row, err := p.Promote(req)
	if err == nil {
		t.Fatal("expected interruption")
	}
	before, err := p.Repo.Head()
	if err != nil || before != row.CandidateRevision {
		t.Fatal("advanced ref lost", err)
	}
	restarted := &DeploymentPublisher{Repo: p.Repo, DB: p.DB, Gate: p.Gate}
	if _, err := restarted.Promote(req); err == nil {
		t.Fatal("revoked approval resumed an effect")
	}
	after, err := p.Repo.Head()
	if err != nil || after != before {
		t.Fatal("ref was rewound after revocation", err)
	}
	journal, err := p.DB.GetDeploymentPromotion(req.OperationID)
	if err != nil || journal.State != "prepared" {
		t.Fatal("unknown outcome falsely acknowledged", err)
	}
}

func TestDeploymentFinalInputFenceRefusesChangedMaterial(t *testing.T) {
	p, r := deploymentHarness(t)
	checked := false
	p.ValidateInputs = func(DeploymentRequest) error { checked = true; return errors.New("example input version changed") }
	if _, err := p.Promote(r); err == nil || !checked {
		t.Fatal("final input check was bypassed", err)
	}
	if head, err := p.Repo.Head(); err != nil || head != "" {
		t.Fatal("input refusal changed served ref", err)
	}
	// Retrying after a refused check cannot infer that the earlier candidate
	// was applied. It must enter the final fence again.
	p.ValidateInputs = func(DeploymentRequest) error { return errors.New("still changed") }
	if _, err := p.Promote(r); err == nil {
		t.Fatal("retry bypassed current input check")
	}
}
