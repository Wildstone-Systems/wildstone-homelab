package deployment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/publish"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

// Manager is opt-in installation wiring for one pre-provisioned application.
// All mutations enter through an existing human-approved public Plan. Its worker
// performs only local Git promotion; no Helm/Kubernetes apply is implemented.
type Manager struct {
	configPath string
	DB         *runtimedb.DB
	State      *statefile.Store
	Product    *publish.Publisher
	repo       *statefile.LocalDeployment
	mu         sync.Mutex
	wake       chan struct{}
}

func Open(path string, db *runtimedb.DB, state *statefile.Store, product *publish.Publisher) (*Manager, error) {
	if db == nil || state == nil || state.Accepted == nil || product == nil {
		return nil, fmt.Errorf("deployment: accepted product publisher required")
	}
	s, err := load(path)
	if err != nil {
		return nil, err
	}
	revision, err := state.AcceptedRevision()
	if err != nil {
		return nil, err
	}
	if err := state.CheckLocalInput(revision); err != nil {
		return nil, err
	}
	repo, err := statefile.OpenLocalDeployment(s.config.Repository)
	if err != nil {
		return nil, fmt.Errorf("deployment: isolated local publication repository unavailable")
	}
	return &Manager{configPath: path, DB: db, State: state, Product: product, repo: repo, wake: make(chan struct{}, 1)}, nil
}

func (m *Manager) Poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	m.Resume()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Resume()
		case <-m.wake:
			m.Resume()
		}
	}
}

func asFiles(projection *capability.HelmProjection) map[string]string {
	out := map[string]string{}
	for name, data := range projection.Files {
		out[name] = string(data)
	}
	return out
}
func request(id string, doc runtimedb.DeploymentPlanDocument) publish.DeploymentRequest {
	r := doc.Deployment.Review
	files := map[string][]byte{}
	for name, data := range r.Files {
		files[name] = []byte(data)
	}
	return publish.DeploymentRequest{OperationID: r.OperationID, PlanID: id, AcceptedRevision: doc.Deployment.SourceRevision, ReviewDigest: r.ProjectionDigest, ExpectedRevision: r.ExpectedDeploymentRevision, Files: files}
}
func review(s inputs, want resource.Resource, source, base, operation, expected string, p *capability.HelmProjection) *runtimedb.DeploymentReview {
	raw, _ := resource.Marshal(want)
	h := s.pkg.Helm
	return &runtimedb.DeploymentReview{Instance: s.config.Instance, OperationID: operation, PreviousProductRevision: base, ProductRevision: source, ExpectedDeploymentRevision: expected, ProjectionDigest: p.Digest, ProfileDigest: s.profile, InventoryDigest: s.inventory, EncryptedSecretRevision: s.secret, Package: s.pkg.Metadata.Name, PackageVersion: s.pkg.Metadata.Version, ChartSource: h.SourceURL, ChartCommit: h.SourceCommit, ChartPath: h.ChartPath, ChartVersion: h.ChartVersion, Image: h.Image, Namespace: s.config.Bindings.Namespace, Release: s.config.Bindings.Name, Timezone: want.HelmDeployment.Timezone, DesiredResource: string(raw), Files: asFiles(p)}
}

func (m *Manager) Propose(owner, instance, timezone string) (runtimedb.PlanRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.DB.CheckHouseholdExecution(); err != nil {
		return runtimedb.PlanRecord{}, err
	}
	s, err := load(m.configPath)
	if err != nil {
		return runtimedb.PlanRecord{}, err
	}
	if instance != s.config.Instance {
		return runtimedb.PlanRecord{}, fmt.Errorf("deployment: instance does not match installed inventory")
	}
	if s.config.repoIdentity() != m.repo.Identity() {
		return runtimedb.PlanRecord{}, fmt.Errorf("deployment: repository binding changed")
	}
	head, err := m.State.AcceptedRevision()
	if err != nil {
		return runtimedb.PlanRecord{}, err
	}
	if branch, err := m.State.Head(); err != nil || branch != head {
		return runtimedb.PlanRecord{}, fmt.Errorf("deployment: product branch changed or awaits recovery; publication held")
	}
	if err := m.State.CheckLocalInput(head); err != nil {
		return runtimedb.PlanRecord{}, err
	}
	current, err := m.State.Load()
	if err != nil {
		return runtimedb.PlanRecord{}, err
	}
	for _, r := range current {
		if r.Kind == resource.KindHelmDeployment && r.Metadata.Name == instance && r.HelmDeployment.Placement != s.placement {
			return runtimedb.PlanRecord{}, fmt.Errorf("deployment: placement change requires separate adoption/retention review")
		}
	}
	want := resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindHelmDeployment, Metadata: resource.Metadata{Name: instance, Owner: "capability/audiobooks", Package: s.pkg.Metadata.Name}, HelmDeployment: &resource.HelmDeploymentSpec{Package: s.pkg.Metadata.Name, Version: s.pkg.Metadata.Version, Timezone: timezone, Placement: s.placement}}
	if err := want.Validate(); err != nil {
		return runtimedb.PlanRecord{}, err
	}
	expected, err := m.repo.Head()
	if err != nil {
		return runtimedb.PlanRecord{}, err
	}
	if expected != "" && !m.knownHead(instance, expected) {
		return runtimedb.PlanRecord{}, fmt.Errorf("deployment: unreviewed local ref change; publication held")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return runtimedb.PlanRecord{}, err
	}
	operation := "deploy-" + hex.EncodeToString(random[:])
	summary := fmt.Sprintf("Deploy Audiobookshelf %s to %s", s.pkg.Metadata.Version, instance)
	source, err := m.State.BuildCandidate(head, []resource.Resource{want}, nil, owner, operation, summary)
	if err != nil {
		return runtimedb.PlanRecord{}, fmt.Errorf("deployment: product candidate unavailable")
	}
	if err := m.State.CreateRef(statefile.PublicationRefPrefix+operation, source); err != nil {
		return runtimedb.PlanRecord{}, err
	}
	projection, err := s.project(want, source)
	if err != nil {
		return runtimedb.PlanRecord{}, err
	}
	material := review(s, want, source, head, operation, expected, projection)
	id, err := m.DB.InsertDeploymentPlan(owner, head, func(id string, authority runtimedb.DeploymentApproval) (runtimedb.DeploymentPlanDocument, error) {
		authority.SourceRevision = source
		authority.Review = material
		doc := runtimedb.DeploymentPlanDocument{BasedOnHead: head, Summary: summary, Deployment: &authority}
		binding, err := publish.DeploymentBinding(m.repo.Identity(), request(id, doc))
		authority.Binding = binding
		return doc, err
	})
	if err != nil {
		return runtimedb.PlanRecord{}, err
	}
	return m.DB.GetPlan(id)
}

// checkPlan derives review material again from current installation inventory,
// immutable candidate resources, profile bytes and encrypted configuration.
func (m *Manager) checkPlan(rec runtimedb.PlanRecord, approving bool) (runtimedb.DeploymentPlanDocument, error) {
	doc, err := runtimedb.DecodeDeploymentPlan(rec.PlanJSON)
	if err != nil {
		return doc, err
	}
	r := doc.Deployment.Review
	if r == nil {
		return doc, fmt.Errorf("deployment: complete review material required")
	}
	s, err := load(m.configPath)
	if err != nil {
		return doc, err
	}
	if s.config.repoIdentity() != m.repo.Identity() {
		return doc, fmt.Errorf("deployment: repository binding changed")
	}
	accepted, err := m.State.AcceptedRevision()
	if err != nil {
		return doc, err
	}
	expected := r.ProductRevision
	if approving {
		expected = r.PreviousProductRevision
	}
	if accepted != expected {
		return doc, runtimedb.ErrRevisionMoved
	}
	if branch, err := m.State.Head(); err != nil || branch != accepted {
		return doc, fmt.Errorf("deployment: product branch changed or awaits recovery; publication held")
	}
	if err := m.State.CheckLocalInput(accepted); err != nil {
		return doc, err
	}
	rs, err := m.State.LoadAt(r.ProductRevision)
	if err != nil {
		return doc, fmt.Errorf("deployment: exact product candidate unavailable")
	}
	var want resource.Resource
	found := false
	for _, candidate := range rs {
		if candidate.Kind == resource.KindHelmDeployment && candidate.Metadata.Name == r.Instance {
			want = candidate
			found = true
		}
	}
	if !found {
		return doc, fmt.Errorf("deployment: instance is missing from reviewed input")
	}
	projection, err := s.project(want, r.ProductRevision)
	if err != nil {
		return doc, err
	}
	expectedReview := review(s, want, r.ProductRevision, r.PreviousProductRevision, r.OperationID, r.ExpectedDeploymentRevision, projection)
	if !reflect.DeepEqual(expectedReview, r) {
		return doc, fmt.Errorf("deployment: reviewed input/profile/encrypted-secret version changed")
	}
	binding, err := publish.DeploymentBinding(m.repo.Identity(), request(rec.ID, doc))
	if err != nil || binding != doc.Deployment.Binding {
		return doc, fmt.Errorf("deployment: publication review binding mismatch")
	}
	return doc, nil
}

func (m *Manager) Approve(id, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, err := m.DB.GetPlan(id)
	if err != nil {
		return err
	}
	if rec.State != runtimedb.PlanProposed {
		return fmt.Errorf("deployment: proposed Plan required")
	}
	doc, err := m.checkPlan(rec, true)
	if err != nil {
		return err
	}
	if _, err := m.Product.Publish(publish.Request{Source: runtimedb.SourceAPIPlan, PlanID: id, PlanTo: runtimedb.PlanApproved, ResolvedBy: owner, Expected: doc.BasedOnHead, Candidate: doc.Deployment.SourceRevision, Who: owner, Why: doc.Summary}); err != nil {
		return err
	}
	m.Poke()
	return nil
}

// Resume re-observes durable journals before any effect. Observing/acknowledging
// an already promoted ref is bookkeeping, including after Owner revocation;
// it never grants a new effect. All actual promotions re-enter the final fence.
func (m *Manager) Resume() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.DB.CheckHouseholdExecution(); err != nil {
		return
	}
	records, err := m.DB.DeploymentPlansToResume()
	if err != nil {
		return
	}
	for _, rec := range records {
		m.resumeOne(rec)
	}
}
func (m *Manager) resumeOne(rec runtimedb.PlanRecord) {
	doc, err := runtimedb.DecodeDeploymentPlan(rec.PlanJSON)
	if err != nil || doc.Deployment.Review == nil {
		return
	}
	r := doc.Deployment.Review
	req := request(rec.ID, doc)
	note := func(phase, code string) { _ = m.DB.RecordDeploymentAttempt(rec.ID, phase, code) }
	binding, err := publish.DeploymentBinding(m.repo.Identity(), req)
	if err != nil || binding != doc.Deployment.Binding {
		note("blocked", "inputs_changed")
		return
	}
	if row, err := m.DB.GetDeploymentPromotion(r.OperationID); err == nil {
		head, e := m.repo.Head()
		tree, t := m.repo.Tree(row.CandidateRevision)
		if e != nil || t != nil {
			note("unknown", "local_unavailable")
			return
		}
		if head == row.CandidateRevision && tree == row.TreeDigest && row.Binding == binding {
			if err := m.DB.AcknowledgeObservedDeployment(rec.ID, r.OperationID, binding, head); err != nil {
				note("unknown", "ack_pending")
			} else {
				note("promoted", "observed")
			}
			return
		}
	}
	if rec.State != runtimedb.PlanApproved && rec.State != runtimedb.PlanApplying {
		return
	}
	if _, err := m.checkPlan(rec, false); err != nil {
		note("blocked", "inputs_changed")
		return
	}
	if err := m.DB.ObserveDeploymentAuthority(rec.ID); err != nil {
		note("blocked", "authority_changed")
		return
	}
	if rec.State == runtimedb.PlanApproved {
		if err := m.DB.TransitionPlan(rec.ID, runtimedb.PlanApplying, ""); err != nil {
			note("unknown", "local_unavailable")
			return
		}
	}
	baseline, err := load(m.configPath)
	if err != nil {
		note("blocked", "inputs_changed")
		return
	}
	if baseline.inventory != r.InventoryDigest || baseline.profile != r.ProfileDigest || baseline.secret != r.EncryptedSecretRevision {
		note("blocked", "inputs_changed")
		return
	}
	p := &publish.DeploymentPublisher{Repo: m.repo, DB: m.DB, Gate: func(got publish.DeploymentRequest) error {
		latest, err := m.DB.GetPlan(rec.ID)
		if err != nil {
			return err
		}
		if _, err := m.checkPlan(latest, false); err != nil {
			return err
		}
		if !reflect.DeepEqual(req, got) {
			return fmt.Errorf("deployment: changed dispatcher input")
		}
		return nil
	}, ValidateInputs: func(publish.DeploymentRequest) error { return baseline.checkFiles(m.configPath) }}
	row, err := p.Promote(req)
	if err != nil {
		if errors.Is(err, statefile.ErrDiverged) {
			note("blocked", "ref_conflict")
		} else if e := m.DB.ObserveDeploymentAuthority(rec.ID); e != nil {
			note("blocked", "authority_changed")
		} else {
			note("failed", "local_unavailable")
		}
		return
	}
	if err := m.DB.AcknowledgeObservedDeployment(rec.ID, r.OperationID, binding, row.CandidateRevision); err != nil {
		note("unknown", "ack_pending")
	} else {
		note("promoted", "observed")
	}
}

func (m *Manager) knownHead(instance, head string) bool {
	for _, state := range []runtimedb.PlanState{runtimedb.PlanApproved, runtimedb.PlanApplying, runtimedb.PlanApplied, runtimedb.PlanFailed, runtimedb.PlanSuperseded} {
		records, err := m.DB.PlansInState(state)
		if err != nil {
			return false
		}
		for _, rec := range records {
			doc, err := runtimedb.DecodeDeploymentPlan(rec.PlanJSON)
			if err != nil || doc.Deployment.Review == nil || doc.Deployment.Review.Instance != instance {
				continue
			}
			row, err := m.DB.GetDeploymentPromotion(doc.Deployment.Review.OperationID)
			if err == nil && row.CandidateRevision == head && row.Binding == doc.Deployment.Binding {
				return true
			}
		}
	}
	return false
}

func (m *Manager) Revoke(id, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.DB.RevokeDeploymentPlan(id, owner)
}
