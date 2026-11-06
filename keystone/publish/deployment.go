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
	// PrivateFileDigests are safe approval identity. PrivateFiles contains the
	// exact local ciphertext only inside the dispatcher and is deliberately
	// excluded from JSON binding/Plan serialization.
	PrivateFileDigests map[string]string
	PrivateFiles       map[string][]byte `json:"-"`
}

// DeploymentPublisher reuses the publication package and runtime DB but keeps
// generated deployment history separate from accepted product Statefile data.
// The opt-in deployment Manager supplies production Plan/input validation.
type DeploymentPublisher struct {
	Repo *statefile.LocalDeployment
	DB   *runtimedb.DB
	Gate func(DeploymentRequest) error
	// ValidateInputs is a bounded local-file fingerprint check, run under the
	// final authority fence. It must make no DB calls or network requests.
	ValidateInputs func(DeploymentRequest) error
	mu             sync.Mutex
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
	if req.PrivateFileDigests != nil {
		gateReq.PrivateFileDigests = make(map[string]string, len(req.PrivateFileDigests))
		for name, digest := range req.PrivateFileDigests {
			gateReq.PrivateFileDigests[name] = digest
		}
	}
	// Gates receive only the reviewed private filename/digest identity. Raw
	// ciphertext is confined to this publisher's internal candidate build.
	gateReq.PrivateFiles = nil
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
	if len(req.PrivateFileDigests) != len(req.PrivateFiles) || len(req.PrivateFiles) > 1 {
		return runtimedb.DeploymentPromotion{}, fmt.Errorf("deployment: exact private projection files required")
	}
	total := 0
	for _, data := range req.Files {
		total += len(data)
		if total > 1<<20 {
			return runtimedb.DeploymentPromotion{}, fmt.Errorf("deployment: projection exceeds 1 MiB")
		}
	}
	for name, data := range req.PrivateFiles {
		if name != "secret.sops.yaml" || len(data) == 0 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(req.PrivateFileDigests[name]) {
			return runtimedb.DeploymentPromotion{}, fmt.Errorf("deployment: invalid private projection file")
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != req.PrivateFileDigests[name] {
			return runtimedb.DeploymentPromotion{}, fmt.Errorf("deployment: private projection digest mismatch")
		}
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
	var privateDigests map[string]string
	if req.PrivateFileDigests != nil {
		privateDigests = make(map[string]string, len(req.PrivateFileDigests))
		for name, digest := range req.PrivateFileDigests {
			privateDigests[name] = digest
		}
	}
	req.PrivateFileDigests = privateDigests
	var privateFiles map[string][]byte
	if req.PrivateFiles != nil {
		privateFiles = make(map[string][]byte, len(req.PrivateFiles))
		for name, data := range req.PrivateFiles {
			privateFiles[name] = append([]byte(nil), data...)
		}
	}
	req.PrivateFiles = privateFiles
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
		complete := make(map[string][]byte, len(req.Files)+len(req.PrivateFiles))
		for name, data := range req.Files {
			complete[name] = data
		}
		for name, data := range req.PrivateFiles {
			if complete[name] != nil {
				return row, fmt.Errorf("deployment: overlapping projection files")
			}
			complete[name] = data
		}
		candidate, err := p.Repo.Build(req.ExpectedRevision, req.OperationID, complete)
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
		if p.ValidateInputs != nil {
			safeReq := req
			safeReq.PrivateFiles = nil
			if err := p.ValidateInputs(safeReq); err != nil {
				return err
			}
		}
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
	doc, parseErr := runtimedb.DecodeDeploymentPlan(approval.PlanJSON)
	if parseErr != nil {
		return row, parseErr
	}
	if doc.Deployment.Review != nil && p.ValidateInputs == nil {
		return row, fmt.Errorf("deployment: current input verification is required")
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
		Request struct {
			OperationID        string
			PlanID             string
			AcceptedRevision   string
			ReviewDigest       string
			ExpectedRevision   string
			Files              map[string][]byte
			PrivateFileDigests map[string]string `json:",omitempty"`
		}
	}{repositoryIdentity, struct {
		OperationID        string
		PlanID             string
		AcceptedRevision   string
		ReviewDigest       string
		ExpectedRevision   string
		Files              map[string][]byte
		PrivateFileDigests map[string]string `json:",omitempty"`
	}{req.OperationID, req.PlanID, req.AcceptedRevision, req.ReviewDigest, req.ExpectedRevision, req.Files, req.PrivateFileDigests}})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:]), nil
}

// DeploymentBinding computes review identity only. It is never approval.
func DeploymentBinding(repositoryIdentity string, req DeploymentRequest) (string, error) {
	return deploymentBinding(repositoryIdentity, req)
}
