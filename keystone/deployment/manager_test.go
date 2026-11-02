package deployment

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/localgit"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/publish"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
	"gopkg.in/yaml.v3"
)

var migrated = dbtemplate.New(func(path string) error {
	db, err := runtimedb.Open(path)
	if err != nil {
		return err
	}
	return db.Close()
})

type fixture struct {
	m            *Manager
	cfg          Config
	path, dbPath string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	dbPath := migrated.Path(t)
	db, err := runtimedb.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, name := range []string{"example-owner", "example-other-owner"} {
		if err := db.RecordButlerMember(name, runtimedb.RoleOwner, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember(name, "example-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.AcquireCoordinator("example-coordinator"); err != nil {
		t.Fatal(err)
	}
	state, err := statefile.New(filepath.Join(root, "statefile"))
	if err != nil {
		t.Fatal(err)
	}
	state.Accepted = db.StatefileHead
	profile, err := os.ReadFile("../capability/testdata/audiobookshelf-helm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Instance: "example-app", ProfileFile: filepath.Join(root, "profile.yaml"), EncryptedSecretFile: filepath.Join(root, "secret.sops.yaml"), Repository: filepath.Join(root, "deployment.git"), MirrorRepository: filepath.Join(root, "delivery.git"), Bindings: capability.HelmBindings{Name: "example-app", Namespace: "example-app", ServiceAccount: "example-reconciler", ConfigClaim: "example-config", MetadataClaim: "example-metadata", MediaClaim: "example-media", SecretName: "example-jwt"}}
	if err := os.WriteFile(cfg.ProfileFile, profile, 0600); err != nil {
		t.Fatal(err)
	}
	// Encrypted-shaped opaque fixture only, not actual credential/key material.
	secret := map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": cfg.Bindings.SecretName, "namespace": cfg.Bindings.Namespace}, "stringData": map[string]any{"jwt-secret": "ENC[" + "REPLACED-payload]"}, "sops": map[string]any{"mac": "ENC[" + "REPLACED-mac]"}}
	raw, err := yaml.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.EncryptedSecretFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "deployment.json")
	writeConfig(t, path, cfg)
	product := &publish.Publisher{DB: db, State: state}
	m, err := Open(path, db, state, product)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{m: m, cfg: cfg, path: path, dbPath: dbPath}
}
func writeConfig(t *testing.T, path string, cfg Config) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func propose(t *testing.T, f *fixture, zone string) runtimedb.PlanRecord {
	t.Helper()
	rec, err := f.m.Propose("example-owner", f.cfg.Instance, zone)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}
func approve(t *testing.T, f *fixture, id string) {
	t.Helper()
	if err := f.m.Approve(id, "example-owner"); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerPlanToLocalPromotionAndMirrorObservation(t *testing.T) {
	f := newFixture(t)
	rec := propose(t, f, "UTC")
	if head, err := f.m.repo.Head(); err != nil || head != "" {
		t.Fatal("proposal exposed deployment output", err)
	}
	if head, err := f.m.State.AcceptedRevision(); err != nil || head != "" {
		t.Fatal("proposal accepted product intent", err)
	}
	if state := f.m.Status(rec.ID); state.Phase != "awaiting_approval" || state.Application != "unobserved" {
		t.Fatal(state)
	}
	f.m.Resume()
	if head, _ := f.m.repo.Head(); head != "" {
		t.Fatal("unapproved Plan executed")
	}
	approve(t, f, rec.ID)
	doc, err := runtimedb.DecodeDeploymentPlan(rec.PlanJSON)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := f.m.State.AcceptedRevision()
	if err != nil || accepted != doc.Deployment.SourceRevision {
		t.Fatal("acceptance split from Plan", err)
	}
	rs, err := f.m.State.Load()
	if err != nil || len(rs) != 1 || rs[0].Kind != resource.KindHelmDeployment {
		t.Fatal("canonical desired input missing", err)
	}
	if head, _ := f.m.repo.Head(); head != "" {
		t.Fatal("approval bypassed dispatcher")
	}
	f.m.Resume()
	current, err := f.m.DB.GetPlan(rec.ID)
	if err != nil || current.State != runtimedb.PlanApplied {
		t.Fatal("local promotion not acknowledged", err)
	}
	status := f.m.Status(rec.ID)
	if status.Phase != "promoted" || status.Delivery != "unavailable" || status.Reconciliation != "unobserved" || status.Application != "unobserved" {
		t.Fatalf("promotion implied runtime success: %+v", status)
	}
	// Delivery remains independent. Execute only the standard local mirror
	// scripts against synthetic repositories, with no daemon or network setup.
	c := localgit.Config{ListenAddress: "127.0.0.1", Port: 22222, ClientCIDR: "127.0.0.1/32", ServiceUser: "example-source", Repository: f.cfg.Repository, MirrorRepository: f.cfg.MirrorRepository, InstallDirectory: filepath.Join(t.TempDir(), "installed"), Namespace: f.cfg.Bindings.Namespace, SourceName: "example-source", CredentialSecret: "example-source-key"}
	files, err := localgit.Render(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(c.InstallDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"init-mirror", "mirror"} {
		path := filepath.Join(c.InstallDirectory, name)
		if err := os.WriteFile(path, files[name], 0600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("sh", path).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	status = f.m.Status(rec.ID)
	if status.Delivery != "mirrored" || status.MirrorRevision != status.PromotedRevision || status.Application != "unobserved" {
		t.Fatal(status)
	}
}

func TestRejectChangedReviewInputsAndAuthority(t *testing.T) {
	for _, change := range []string{"inventory", "profile", "secret", "cross-instance", "repository", "owner-revoked", "owner-role-restored", "product-moved", "unaccepted-edit"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			rec := propose(t, f, "UTC")
			switch change {
			case "inventory":
				raw, _ := os.ReadFile(f.path)
				if err := os.WriteFile(f.path, append(raw, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			case "profile":
				raw, _ := os.ReadFile(f.cfg.ProfileFile)
				if err := os.WriteFile(f.cfg.ProfileFile, append(raw, []byte("\n# changed artifact file\n")...), 0600); err != nil {
					t.Fatal(err)
				}
			case "secret":
				raw, _ := os.ReadFile(f.cfg.EncryptedSecretFile)
				raw = []byte(strings.Replace(string(raw), "REPLACED-payload", "REPLACED-new-payload", 1))
				if err := os.WriteFile(f.cfg.EncryptedSecretFile, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "cross-instance":
				f.cfg.Instance = "example-other"
				f.cfg.Bindings.Name = f.cfg.Instance
				writeConfig(t, f.path, f.cfg)
			case "repository":
				f.cfg.Repository = filepath.Join(t.TempDir(), "other.git")
				writeConfig(t, f.path, f.cfg)
			case "owner-revoked":
				if err := f.m.DB.DisableButlerMember("example-owner", "example-other-owner"); err != nil {
					t.Fatal(err)
				}
			case "owner-role-restored":
				if err := f.m.DB.SetButlerMemberRole("example-owner", runtimedb.RoleMember, "example-other-owner"); err != nil {
					t.Fatal(err)
				}
				if err := f.m.DB.SetButlerMemberRole("example-owner", runtimedb.RoleOwner, "example-other-owner"); err != nil {
					t.Fatal(err)
				}
			case "product-moved":
				if err := f.m.DB.SetStatefileHead(strings.Repeat("c", 40)); err != nil {
					t.Fatal(err)
				}
			case "unaccepted-edit":
				doc, err := runtimedb.DecodeDeploymentPlan(rec.PlanJSON)
				if err != nil {
					t.Fatal(err)
				}
				want, err := resource.Unmarshal([]byte(doc.Deployment.Review.DesiredResource))
				if err != nil {
					t.Fatal(err)
				}
				if err := f.m.State.Put(want); err != nil {
					t.Fatal(err)
				}
				if err := f.m.State.Commit("example-admin", "external-edit", "example edit"); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.m.Approve(rec.ID, "example-owner"); err == nil {
				t.Fatal("stale/changed review approved")
			}
			f.m.Resume()
			if head, _ := f.m.repo.Head(); head != "" {
				t.Fatal("ref advanced after refusal")
			}
		})
	}
}

func TestRevocationAfterApprovalBlocksNewEffect(t *testing.T) {
	f := newFixture(t)
	rec := propose(t, f, "UTC")
	approve(t, f, rec.ID)
	if err := f.m.DB.DisableButlerMember("example-owner", "example-other-owner"); err != nil {
		t.Fatal(err)
	}
	f.m.Resume()
	if head, _ := f.m.repo.Head(); head != "" {
		t.Fatal("revoked approval executed")
	}
	if status := f.m.Status(rec.ID); status.Phase != "blocked" {
		t.Fatal(status)
	}
}

func TestNewApprovalSupersedesOldEvenWhenProductRevisionUnchanged(t *testing.T) {
	f := newFixture(t)
	initial := propose(t, f, "UTC")
	approve(t, f, initial.ID)
	f.m.Resume()
	a := propose(t, f, "UTC")
	b := propose(t, f, "UTC")
	approve(t, f, a.ID)
	approve(t, f, b.ID)
	f.m.Resume()
	old, err := runtimedb.DecodeDeploymentPlan(a.PlanJSON)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.DB.GetDeploymentPromotion(old.Deployment.Review.OperationID); !errors.Is(err, runtimedb.ErrNotFound) {
		t.Fatal("superseded operation was prepared", err)
	}
	current, err := f.m.DB.GetPlan(b.ID)
	if err != nil || current.State != runtimedb.PlanApplied {
		t.Fatal("latest exact approval failed", err)
	}
	if status := f.m.Status(a.ID); status.Phase != "blocked" {
		t.Fatal(status)
	}
}

// Simulate process interruption at the two durable journal/ref boundaries.
func TestResumeFromPreparedAndUnacknowledgedRef(t *testing.T) {
	for _, phase := range []string{"prepared", "advanced", "advanced-then-revoked", "advanced-then-plan-revoked"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			rec := propose(t, f, "UTC")
			approve(t, f, rec.ID)
			doc, err := runtimedb.DecodeDeploymentPlan(rec.PlanJSON)
			if err != nil {
				t.Fatal(err)
			}
			req := request(rec.ID, doc)
			candidate, err := f.m.repo.Build(req.ExpectedRevision, req.OperationID, req.Files)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.m.repo.Retain(req.OperationID, candidate); err != nil {
				t.Fatal(err)
			}
			tree, err := f.m.repo.Tree(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.m.DB.PrepareDeploymentPromotion(runtimedb.DeploymentPromotion{OperationID: req.OperationID, Binding: doc.Deployment.Binding, ExpectedRevision: req.ExpectedRevision, CandidateRevision: candidate, TreeDigest: tree}); err != nil {
				t.Fatal(err)
			}
			if err := f.m.DB.TransitionPlan(rec.ID, runtimedb.PlanApplying, ""); err != nil {
				t.Fatal(err)
			}
			if phase != "prepared" {
				if err := f.m.repo.Promote(req.ExpectedRevision, candidate); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "advanced-then-revoked" {
				if err := f.m.DB.DisableButlerMember("example-owner", "example-other-owner"); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "advanced-then-plan-revoked" {
				if err := f.m.Revoke(rec.ID, "example-other-owner"); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.m.DB.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := runtimedb.Open(f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.AcquireCoordinator("example-coordinator"); err != nil {
				t.Fatal(err)
			}
			state, err := statefile.New(f.m.State.Root)
			if err != nil {
				t.Fatal(err)
			}
			state.Accepted = db.StatefileHead
			restarted, err := Open(f.path, db, state, &publish.Publisher{DB: db, State: state})
			if err != nil {
				t.Fatal(err)
			}
			restarted.Resume()
			head, err := restarted.repo.Head()
			if err != nil || head != candidate {
				t.Fatal("resume changed exact candidate", err)
			}
			row, err := db.GetDeploymentPromotion(req.OperationID)
			if err != nil || row.State != "promoted" {
				t.Fatal("observed ref was not acknowledged", err)
			}
			final, err := db.GetPlan(rec.ID)
			expectedState := runtimedb.PlanApplied
			if phase == "advanced-then-plan-revoked" {
				expectedState = runtimedb.PlanFailed
			}
			if err != nil || final.State != expectedState {
				t.Fatal("resume changed authority state or lost publication bookkeeping", err)
			}
			restarted.Resume()
			if again, _ := restarted.repo.Head(); again != head {
				t.Fatal("duplicate effect on retry")
			}
		})
	}
}

func TestPendingPromotionStopsForChangedAuthorityInputsAndOwnerRef(t *testing.T) {
	for _, change := range []string{"explicit-revoke", "secret", "stop", "quarantine", "coordinator", "owner-ref"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			rec := propose(t, f, "UTC")
			approve(t, f, rec.ID)
			switch change {
			case "explicit-revoke":
				if err := f.m.Revoke(rec.ID, "example-other-owner"); err != nil {
					t.Fatal(err)
				}
			case "secret":
				raw, err := os.ReadFile(f.cfg.EncryptedSecretFile)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.cfg.EncryptedSecretFile, append(raw, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			case "stop":
				if err := f.m.DB.StopHouseholdExecution("example-other-owner", "example stop"); err != nil {
					t.Fatal(err)
				}
			case "quarantine":
				if err := f.m.DB.EnterRestoreQuarantine("example restore", time.Now()); err != nil {
					t.Fatal(err)
				}
			case "coordinator":
				other, err := runtimedb.Open(f.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				if _, err := other.PromoteCoordinator("example-next-coordinator"); err != nil {
					t.Fatal(err)
				}
			case "owner-ref":
				candidate, err := f.m.repo.Build("", "example-external", map[string][]byte{"source.yaml": []byte("example external source"), "release.yaml": []byte("example external release"), "kustomization.yaml": []byte("example external resources")})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.m.repo.Retain("example-external", candidate); err != nil {
					t.Fatal(err)
				}
				if err := f.m.repo.Promote("", candidate); err != nil {
					t.Fatal(err)
				}
			}
			before, err := f.m.repo.Head()
			if err != nil {
				t.Fatal(err)
			}
			f.m.Resume()
			f.m.Resume()
			if after, err := f.m.repo.Head(); err != nil || after != before {
				t.Fatal("held publication changed ref", err)
			}
			status := f.m.Status(rec.ID)
			if status.Application != "unobserved" || status.Phase == "promoted" {
				t.Fatal(status)
			}
		})
	}
}

func TestRevokePreparedPlanCannotResumeNewEffect(t *testing.T) {
	f := newFixture(t)
	rec := propose(t, f, "UTC")
	approve(t, f, rec.ID)
	doc, err := runtimedb.DecodeDeploymentPlan(rec.PlanJSON)
	if err != nil {
		t.Fatal(err)
	}
	req := request(rec.ID, doc)
	candidate, err := f.m.repo.Build(req.ExpectedRevision, req.OperationID, req.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.repo.Retain(req.OperationID, candidate); err != nil {
		t.Fatal(err)
	}
	tree, err := f.m.repo.Tree(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.DB.PrepareDeploymentPromotion(runtimedb.DeploymentPromotion{OperationID: req.OperationID, Binding: doc.Deployment.Binding, ExpectedRevision: req.ExpectedRevision, CandidateRevision: candidate, TreeDigest: tree}); err != nil {
		t.Fatal(err)
	}
	if err := f.m.DB.TransitionPlan(rec.ID, runtimedb.PlanApplying, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Revoke(rec.ID, "example-other-owner"); err != nil {
		t.Fatal(err)
	}
	f.m.Resume()
	if head, _ := f.m.repo.Head(); head != "" {
		t.Fatal("revoked prepared candidate executed")
	}
	if status := f.m.Status(rec.ID); status.Phase != "blocked" {
		t.Fatal(status)
	}
}
