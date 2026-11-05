package api

// Backup endpoints (ADR-W015 A2): the ledger read plus on-demand run and
// restore-alongside. Run/restore carry the approve verb — they are
// human-initiated EXECUTIONS, so the Butler's propose-only role cannot
// trigger them; scheduled runs remain the engine's standing duty
// (ADR-W011 A4). The engine is reached through function hooks
// cmd/keystone wires (the MintJoin pattern), keeping this package free
// of a backup-engine dependency.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

type backupPasswordStatus struct {
	State       string `json:"state"`
	Reason      string `json:"reason"`
	PasswordRef string `json:"passwordRef"`
	Source      string `json:"source"`
	Remedy      string `json:"remedy"`
	Held        bool   `json:"held"`
	CanRepair   bool   `json:"canRepair"`
}

type backupPlanView struct {
	Name                    string                   `json:"name"`
	Node                    string                   `json:"node"`
	Owner                   string                   `json:"owner"`
	Spec                    *resource.BackupPlanSpec `json:"spec"`
	SourceBytes             *uint64                  `json:"sourceBytes,omitempty"`
	EstimatedMonthlyCostUSD *float64                 `json:"estimatedMonthlyCostUsd,omitempty"`
	CostNote                string                   `json:"costNote,omitempty"`
	Dependency              backupPasswordStatus     `json:"dependency"`
}

type backupTargetView struct {
	Name  string                     `json:"name"`
	Node  string                     `json:"node"`
	Owner string                     `json:"owner"`
	Spec  *resource.BackupTargetSpec `json:"spec"`
}

type backupTargetRequest struct {
	Name string                    `json:"name"`
	Node string                    `json:"node"`
	Spec resource.BackupTargetSpec `json:"spec"`
}

type backupPlanRequest struct {
	Node string                  `json:"node"`
	Spec resource.BackupPlanSpec `json:"spec"`
}

func (s *Server) listBackups(r *http.Request, id principal.Identity) (any, int, error) {
	desired, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("loading statefile: %w", err)
	}
	runs, err := s.DB.ListBackupRuns(100)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if runs == nil {
		runs = []runtimedb.BackupRun{}
	}
	targets := backupTargets(desired)
	plans := make([]backupPlanView, 0)
	inventories := make(map[string]backupSecretSnapshot)
	for _, d := range desired {
		if d.Kind != resource.KindBackupPlan || d.BackupPlan == nil {
			continue
		}
		inventory, ok := inventories[d.Metadata.Node]
		if !ok {
			inventory = s.backupSecretInventory(r.Context(), d.Metadata.Node)
			inventories[d.Metadata.Node] = inventory
		}
		password, err := s.backupPasswordState(desired, d, inventory, id.Role == runtimedb.RoleAdmin)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		v := backupPlanView{Name: d.Metadata.Name, Node: d.Metadata.Node, Owner: d.Metadata.Owner, Spec: d.BackupPlan, Dependency: password}
		if samples, sampleErr := s.DB.LatestBackupSourceSamples(d.Metadata.Name, 1); sampleErr != nil {
			return nil, http.StatusInternalServerError, sampleErr
		} else if len(samples) > 0 {
			bytes := samples[0].Bytes
			v.SourceBytes = &bytes
			if d.BackupPlan.Offsite != nil {
				if t, ok := targets[d.BackupPlan.Offsite.Target]; ok {
					cost, note := storageCost(bytes, t.Spec.Endpoint)
					v.EstimatedMonthlyCostUSD, v.CostNote = &cost, note
				}
			}
		}
		plans = append(plans, v)
	}
	targetList := make([]backupTargetView, 0, len(targets))
	for _, d := range desired {
		if d.Kind == resource.KindBackupTarget && d.BackupTarget != nil {
			targetList = append(targetList, backupTargetView{Name: d.Metadata.Name, Node: d.Metadata.Node, Owner: d.Metadata.Owner, Spec: d.BackupTarget})
		}
	}
	return map[string]any{"plans": plans, "targets": targetList, "runs": runs}, http.StatusOK, nil
}

type backupPasswordDeclaration struct {
	ref    string
	source string
}

func backupPasswordDeclarations(desired []resource.Resource, plan resource.Resource) []backupPasswordDeclaration {
	refs := []string{plan.BackupPlan.PasswordSecret}
	if plan.BackupPlan.Offsite != nil {
		if plan.BackupPlan.Offsite.Route == resource.BackupRouteOffsiteOnly {
			refs = refs[:0]
		}
		refs = append(refs, plan.BackupPlan.Offsite.PasswordSecret)
	}
	declarations := make([]backupPasswordDeclaration, 0, len(refs))
	for _, ref := range refs {
		declarations = append(declarations, backupPasswordDeclaration{ref: ref, source: backupPasswordSource(desired, plan, ref)})
	}
	return declarations
}

func backupPasswordSource(desired []resource.Resource, plan resource.Resource, ref string) string {
	name := strings.TrimPrefix(ref, "secret/")
	if !strings.HasPrefix(ref, "secret/") || name == "" {
		return "unknown"
	}
	for _, d := range desired {
		if d.Kind != resource.KindSecret || d.Secret == nil || d.Secret.Name != name ||
			d.Metadata.Node != plan.Metadata.Node || d.Metadata.Owner != plan.Metadata.Owner {
			continue
		}
		if d.Secret.Provided {
			return "provided"
		}
		return "generated"
	}
	return "unknown"
}

type backupSecretSnapshot struct {
	have      map[string]bool
	available bool
}

func (s *Server) backupSecretInventory(ctx context.Context, node string) backupSecretSnapshot {
	if s.Agents == nil {
		return backupSecretSnapshot{}
	}
	res, err := s.Agents.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_ListSecrets{ListSecrets: &wsv1.ListSecrets{}}})
	if err != nil || res == nil || res.GetError() != nil || res.GetListSecrets() == nil {
		return backupSecretSnapshot{}
	}
	have := make(map[string]bool, len(res.GetListSecrets().GetSecrets()))
	for _, secret := range res.GetListSecrets().GetSecrets() {
		have[secret.GetName()] = true
	}
	return backupSecretSnapshot{have: have, available: true}
}

func (s *Server) backupPasswordState(desired []resource.Resource, plan resource.Resource, inventory backupSecretSnapshot, owner bool) (backupPasswordStatus, error) {
	declarations := backupPasswordDeclarations(desired, plan)
	status := backupPasswordStatus{PasswordRef: declarations[0].ref, Source: declarations[0].source, Remedy: "none"}
	if !inventory.available {
		status.State, status.Reason, status.Held = "inventory-unavailable", "secret-inventory-unavailable", true
		return status, nil
	}
	missing := make([]backupPasswordDeclaration, 0, len(declarations))
	for _, declaration := range declarations {
		name := strings.TrimPrefix(declaration.ref, "secret/")
		if !strings.HasPrefix(declaration.ref, "secret/") || name == "" || !inventory.have[name] {
			missing = append(missing, declaration)
		}
	}
	if len(missing) > 0 {
		// Prefer a dependency that reconciliation cannot repair. Offering a
		// generated-secret action while a provided or unowned route password
		// is also absent would claim the whole plan can converge from that click.
		selected := missing[0]
		for _, declaration := range missing {
			if declaration.source != "generated" {
				selected = declaration
				break
			}
		}
		status.PasswordRef, status.Source = selected.ref, selected.source
		status.State, status.Reason, status.Held = "missing", "password-secret-missing", true
		switch selected.source {
		case "generated":
			status.Remedy, status.CanRepair = "reconcile", owner
		case "provided":
			status.Remedy = "enter-secret"
		}
		return status, nil
	}
	repaired, err := s.DB.HasUnexercisedBackupDependencyRepair(plan.Metadata.Name)
	if err != nil {
		return backupPasswordStatus{}, err
	}
	if repaired {
		status.State, status.Reason = "repaired", "password-secret-repaired"
	} else {
		status.State, status.Reason = "present", "password-secret-present"
	}
	return status, nil
}

func (s *Server) repairBackupPassword(r *http.Request, id principal.Identity) (any, int, error) {
	if code, err := planDecider(id); err != nil {
		return nil, code, err
	}
	if id.AuthTime != nil {
		if err := sameSiteWrite(r); err != nil {
			return nil, http.StatusForbidden, fmt.Errorf("backup password repair requires a same-origin request")
		}
	}
	if s.OnBackupRepair == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("backup reconciliation is not wired")
	}
	desired, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("loading statefile: %w", err)
	}
	name := r.PathValue("plan")
	var plan resource.Resource
	found := false
	for _, d := range desired {
		if d.Kind == resource.KindBackupPlan && d.BackupPlan != nil && d.Metadata.Name == name {
			plan, found = d, true
			break
		}
	}
	if !found {
		return nil, http.StatusNotFound, fmt.Errorf("BackupPlan %s not found", name)
	}
	declarations := backupPasswordDeclarations(desired, plan)
	inventory := s.backupSecretInventory(r.Context(), plan.Metadata.Node)
	if !inventory.available {
		return nil, http.StatusConflict, fmt.Errorf("cannot confirm password-secret absence on node %s", plan.Metadata.Node)
	}
	var missing *backupPasswordDeclaration
	for i := range declarations {
		name := strings.TrimPrefix(declarations[i].ref, "secret/")
		if !strings.HasPrefix(declarations[i].ref, "secret/") || name == "" || !inventory.have[name] {
			if missing == nil || (missing.source == "generated" && declarations[i].source != "generated") {
				missing = &declarations[i]
			}
		}
	}
	if missing == nil {
		return map[string]string{"plan": name, "node": plan.Metadata.Node, "passwordRef": declarations[0].ref, "state": "present"}, http.StatusOK, nil
	}
	if missing.source != "generated" {
		if missing.source == "provided" {
			return nil, http.StatusConflict, fmt.Errorf("%s is operator-provided; enter it through Secrets instead of generating it", missing.ref)
		}
		return nil, http.StatusConflict, fmt.Errorf("%s is not a generated Secret owned by BackupPlan %s on node %s", missing.ref, name, plan.Metadata.Node)
	}
	s.OnBackupRepair()
	return map[string]string{"plan": name, "node": plan.Metadata.Node, "passwordRef": missing.ref, "state": "pending"}, http.StatusAccepted, nil
}

func backupTargets(desired []resource.Resource) map[string]backupTargetView {
	out := make(map[string]backupTargetView)
	for _, d := range desired {
		if d.Kind == resource.KindBackupTarget && d.BackupTarget != nil {
			out[d.Metadata.Name] = backupTargetView{Name: d.Metadata.Name, Node: d.Metadata.Node, Owner: d.Metadata.Owner, Spec: d.BackupTarget}
		}
	}
	return out
}

// storageCost is deliberately a labelled planning estimate, not a bill.
// It uses representative public storage rates and excludes requests,
// retrieval, tax, minimum-retention and free-tier effects.
func storageCost(bytes uint64, endpoint string) (float64, string) {
	host := ""
	if u, err := url.Parse(endpoint); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	rate, provider := 0.023, "S3-compatible baseline"
	switch {
	case strings.Contains(host, "r2.cloudflarestorage.com"):
		rate, provider = 0.015, "Cloudflare R2 standard storage"
	case strings.Contains(host, "backblazeb2.com"):
		rate, provider = 0.006, "Backblaze B2 storage"
	case strings.Contains(host, "wasabisys.com"):
		rate, provider = 0.00699, "Wasabi storage"
	}
	return float64(bytes) / 1e9 * rate, provider + "; rough storage-only estimate, verify current provider pricing"
}

func (s *Server) listBackupTargets(_ *http.Request, _ principal.Identity) (any, int, error) {
	desired, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	list := make([]backupTargetView, 0)
	for _, d := range desired {
		if d.Kind == resource.KindBackupTarget && d.BackupTarget != nil {
			list = append(list, backupTargetView{Name: d.Metadata.Name, Node: d.Metadata.Node, Owner: d.Metadata.Owner, Spec: d.BackupTarget})
		}
	}
	return map[string]any{"targets": list}, http.StatusOK, nil
}

func (s *Server) proposeBackupTarget(r *http.Request, id principal.Identity) (any, int, error) {
	var req backupTargetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing target: %w", err)
	}
	owner := "system"
	if desired, err := s.State.Load(); err == nil {
		for _, d := range desired {
			if d.Kind == resource.KindBackupTarget && d.Metadata.Name == req.Name {
				owner = d.Metadata.Owner
				if req.Node == "" {
					req.Node = d.Metadata.Node
				}
				break
			}
		}
	}
	res := resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindBackupTarget,
		Metadata: resource.Metadata{Name: req.Name, Node: req.Node, Owner: owner}, BackupTarget: &req.Spec}
	return s.proposeBackupResource(id, res, "Configure off-site backup target "+req.Name)
}

func (s *Server) proposeBackupPlan(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("plan")
	var req backupPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing backup plan: %w", err)
	}
	owner := "system"
	if desired, err := s.State.Load(); err == nil {
		for _, d := range desired {
			if d.Kind == resource.KindBackupPlan && d.Metadata.Name == name {
				owner = d.Metadata.Owner
				if req.Node == "" {
					req.Node = d.Metadata.Node
				}
				break
			}
		}
	}
	res := resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindBackupPlan,
		Metadata: resource.Metadata{Name: name, Node: req.Node, Owner: owner}, BackupPlan: &req.Spec}
	return s.proposeBackupResource(id, res, "Configure backup plan "+name)
}

func (s *Server) estimateBackupPlan(r *http.Request, _ principal.Identity) (any, int, error) {
	if s.BackupEstimate == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("backup estimator not wired")
	}
	name := r.PathValue("plan")
	var req backupPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing backup plan: %w", err)
	}
	res := resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindBackupPlan,
		Metadata: resource.Metadata{Name: name, Node: req.Node, Owner: "system"}, BackupPlan: &req.Spec}
	if err := res.Validate(); err != nil {
		return nil, http.StatusBadRequest, err
	}
	bytes, disk, err := s.BackupEstimate(r.Context(), res)
	if err != nil {
		return nil, http.StatusUnprocessableEntity, err
	}
	out := map[string]any{"sourceBytes": bytes, "diskCheck": disk}
	if req.Spec.Offsite != nil {
		desired, loadErr := s.State.Load()
		if loadErr != nil {
			return nil, http.StatusInternalServerError, loadErr
		}
		if target, ok := backupTargets(desired)[req.Spec.Offsite.Target]; ok {
			cost, note := storageCost(bytes, target.Spec.Endpoint)
			out["estimatedMonthlyCostUsd"], out["costNote"] = cost, note
		}
	}
	return out, http.StatusOK, nil
}

func (s *Server) proposeBackupResource(id principal.Identity, res resource.Resource, summary string) (any, int, error) {
	if err := res.Validate(); err != nil {
		return nil, http.StatusBadRequest, err
	}
	doc, err := resource.Marshal(res)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return s.submitProposal(id, proposalRequest{Summary: summary, Put: []string{string(doc)}})
}

func (s *Server) testBackupTarget(r *http.Request, _ principal.Identity) (any, int, error) {
	if s.BackupTargetTest == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("backup target tester not wired")
	}
	name := r.PathValue("target")
	if err := s.BackupTargetTest(r.Context(), name); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("testing BackupTarget %s: %w", name, err)
	}
	return map[string]string{"target": name, "result": "read-only bucket check succeeded"}, http.StatusOK, nil
}

func (s *Server) runBackup(r *http.Request, id principal.Identity) (any, int, error) {
	return s.backupExec(r, id, s.BackupRun, "backup run")
}

// restoreBackup restores alongside from the local repository, or with
// ?from=offsite from the plan's off-site copy (ADR-W045 A3) — the path
// after a total loss, when the local copy is gone too.
func (s *Server) restoreBackup(r *http.Request, id principal.Identity) (any, int, error) {
	switch from := r.URL.Query().Get("from"); from {
	case "", "local":
		return s.backupExec(r, id, s.BackupRestore, "restore")
	case "offsite":
		return s.backupExec(r, id, s.BackupRestoreOffsite, "off-site restore")
	default:
		return nil, http.StatusBadRequest, fmt.Errorf("from must be local or offsite, not %q", from)
	}
}

func (s *Server) backupExec(r *http.Request, id principal.Identity, hook func(context.Context, string) error, what string) (any, int, error) {
	if hook == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("backup engine not wired")
	}
	plan := r.PathValue("plan")
	if plan == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("plan name required")
	}
	// The engine records its own full provenance (plan record, ledger,
	// events); the audit middleware already records who called this
	// endpoint. Errors surface with the engine's detail — a run that
	// fails mid-way still left its failed ledger row.
	if err := hook(r.Context(), plan); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("%s %s: %w", what, plan, err)
	}
	_ = id
	return map[string]string{"plan": plan, "result": what + " completed"}, http.StatusOK, nil
}
