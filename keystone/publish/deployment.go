package publish

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

// DeploymentRequest is internal dispatcher input. It is not an HTTP request
// or proof of approval. Gate must recheck exact Owner approval, input/output
// identity, instance ownership and encrypted-secret version at each boundary.
type DeploymentRequest struct {
	OperationID      string
	PlanID           string
	AcceptedRevision string
	ReviewDigest     string
	ExpectedRevision string
	Files            map[string][]byte
}

// DeploymentPublisher reuses the publication package and runtime DB but keeps
// generated deployment history separate from accepted product Statefile data.
// No production caller is wired until the Plan/authority integration is ready.
type DeploymentPublisher struct {
	Repo *statefile.LocalDeployment
	DB   *runtimedb.DB
	Gate func(DeploymentRequest) error
	mu   sync.Mutex
	// interrupt is a local test seam, not a caller-supplied execution hook.
	interrupt func(string) error
}

func (p *DeploymentPublisher) authorize(req DeploymentRequest) error {
	if p.Gate == nil {
		return fmt.Errorf("deployment: approval gate is not configured")
	}
	if err := p.DB.CheckHouseholdExecution(); err != nil {
		return err
	}
	current, err := p.DB.StatefileHead()
	if err != nil {
		return err
	}
	if current != req.AcceptedRevision {
		return ErrSuperseded
	}
	// Gate code cannot mutate the bytes subsequently built or compared with
	// the journal. It receives its own copy of review material.
	gateReq := req
	gateReq.Files = make(map[string][]byte, len(req.Files))
	for name, data := range req.Files {
		gateReq.Files[name] = append([]byte(nil), data...)
	}
	return p.Gate(gateReq)
}

// Promote prepares a complete candidate, journals it, then advances only the
// locally served ref with CAS. Retry after ref advancement but before DB ack
// recognizes the recorded exact candidate. It never pushes, fetches or starts
// serving. A changed owner ref is left untouched.
func (p *DeploymentPublisher) Promote(req DeploymentRequest) (runtimedb.DeploymentPromotion, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Repo == nil || p.DB == nil {
		return runtimedb.DeploymentPromotion{}, fmt.Errorf("deployment: publisher unavailable")
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9-]{1,80}$`).MatchString(req.OperationID) || req.PlanID == "" || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(req.AcceptedRevision) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(req.ReviewDigest) {
		return runtimedb.DeploymentPromotion{}, fmt.Errorf("deployment: invalid review identity")
	}
	if len(req.Files) != 3 {
		return runtimedb.DeploymentPromotion{}, fmt.Errorf("deployment: exact projection files required")
	}
	total := 0
	for _, data := range req.Files {
		total += len(data)
		if total > 1<<20 {
			return runtimedb.DeploymentPromotion{}, fmt.Errorf("deployment: projection exceeds 1 MiB")
		}
	}
	// Freeze caller-owned buffers before validation/journal binding.
	files := make(map[string][]byte, len(req.Files))
	for name, data := range req.Files {
		files[name] = append([]byte(nil), data...)
	}
	req.Files = files
	binding, err := deploymentBinding(p.Repo.Identity(), req)
	if err != nil {
		return runtimedb.DeploymentPromotion{}, err
	}
	if err := p.authorize(req); err != nil {
		return runtimedb.DeploymentPromotion{}, err
	}
	row, err := p.DB.GetDeploymentPromotion(req.OperationID)
	if errors.Is(err, runtimedb.ErrNotFound) {
		head, err := p.Repo.Head()
		if err != nil {
			return row, err
		}
		if head != req.ExpectedRevision {
			return row, statefile.ErrDiverged
		}
		candidate, err := p.Repo.Build(req.ExpectedRevision, req.OperationID, req.Files)
		if err != nil {
			return row, err
		}
		// Retain by candidate identity before recording it. Concurrent builders
		// can leave harmless private refs; only the journal winner is promoted.
		if err := p.Repo.Retain(candidate, candidate); err != nil {
			return row, err
		}
		tree, err := p.Repo.Tree(candidate)
		if err != nil {
			return row, err
		}
		row, err = p.DB.PrepareDeploymentPromotion(runtimedb.DeploymentPromotion{OperationID: req.OperationID, Binding: binding, ExpectedRevision: req.ExpectedRevision, CandidateRevision: candidate, TreeDigest: tree})
		if err != nil {
			return row, err
		}
	} else if err != nil {
		return row, err
	}
	if row.Binding != binding || row.ExpectedRevision != req.ExpectedRevision {
		return row, fmt.Errorf("deployment: operation conflicts with existing review binding")
	}
	// A journaled candidate must still exist and have exactly the recorded
	// tree. Ref/DB recovery never rebuilds it from mutable working files.
	tree, err := p.Repo.Tree(row.CandidateRevision)
	if err != nil {
		return row, err
	}
	if tree != row.TreeDigest {
		return row, fmt.Errorf("deployment: candidate tree mismatch")
	}
	if p.interrupt != nil {
		if err := p.interrupt("prepared"); err != nil {
			return row, err
		}
	}
	if err := p.authorize(req); err != nil {
		return row, err
	}
	effect := func() error {
		head, err := p.Repo.Head()
		if err != nil {
			return err
		}
		if head == row.CandidateRevision {
			return nil
		}
		if row.State == "promoted" || head != row.ExpectedRevision {
			return statefile.ErrDiverged
		}
		return p.Repo.Promote(row.ExpectedRevision, row.CandidateRevision)
	}
	approval, err := p.DB.GetPlan(req.PlanID)
	if err != nil {
		return row, err
	}
	err = p.DB.WithDeploymentApproval(req.PlanID, approval.PlanJSON, binding, req.AcceptedRevision, effect)
	if err != nil {
		return row, err
	}
	if p.interrupt != nil {
		if err := p.interrupt("promoted"); err != nil {
			return row, err
		}
	}
	if err := p.DB.MarkDeploymentPromoted(row.OperationID, binding); err != nil {
		return row, err
	}
	row.State = "promoted"
	return row, nil
}

// deploymentBinding is identity only; it does not grant execution authority.
func deploymentBinding(repositoryIdentity string, req DeploymentRequest) (string, error) {
	material, err := json.Marshal(struct {
		Repo    string
		Request DeploymentRequest
	}{repositoryIdentity, req})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:]), nil
}
