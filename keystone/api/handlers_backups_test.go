package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

func TestBackupEndpoints(t *testing.T) {
	h := newHarness(t)

	// Unwired: 503.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/backups/photos/run", nil, nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unwired = %d, want 503", resp.StatusCode)
	}

	var ran, restored []string
	h.s.BackupRun = func(_ context.Context, plan string) error {
		ran = append(ran, plan)
		if plan == "broken" {
			return errors.New("no BackupPlan broken")
		}
		return nil
	}
	h.s.BackupRestore = func(_ context.Context, plan string) error {
		restored = append(restored, plan)
		return nil
	}

	// approve verb: the Butler (propose-only) can never trigger a run or
	// restore — the structural safety claim carried through (ADR-W015 A2).
	for _, path := range []string{"/api/v1/backups/photos/run", "/api/v1/backups/photos/restore"} {
		if resp := h.do(runtimedb.RoleButler, http.MethodPost, path, nil, nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("butler on %s = %d, want 403", path, resp.StatusCode)
		}
	}
	if len(ran)+len(restored) != 0 {
		t.Fatal("butler request reached the engine")
	}

	// The operator can run and restore.
	if resp := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/backups/photos/run", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("run = %d", resp.StatusCode)
	}
	if resp := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/backups/photos/restore", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("restore = %d", resp.StatusCode)
	}
	if len(ran) != 1 || ran[0] != "photos" || len(restored) != 1 || restored[0] != "photos" {
		t.Fatalf("engine calls = run %v, restore %v", ran, restored)
	}

	// Engine failure surfaces.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/backups/broken/run", nil, nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("broken run = %d, want 502", resp.StatusCode)
	}

	// Ledger list (read verb — the Butler may read).
	if _, err := h.s.DB.StartBackupRun("photos", runtimedb.BackupRunBackup, "plan-1"); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Runs []runtimedb.BackupRun `json:"runs"`
	}
	if resp := h.do(runtimedb.RoleButler, http.MethodGet, "/api/v1/backups", nil, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", resp.StatusCode)
	}
	if len(out.Runs) != 1 || out.Runs[0].PlanName != "photos" {
		t.Fatalf("runs = %+v", out.Runs)
	}
}

// TestBackupRestoreFrom: ?from picks the local or the off-site copy, and
// anything else is refused rather than guessed.
func TestBackupRestoreFrom(t *testing.T) {
	h := newHarness(t)
	var local, offsite []string
	h.s.BackupRestore = func(_ context.Context, plan string) error { local = append(local, plan); return nil }
	path := "/api/v1/backups/photos/restore"

	// Unwired off-site restore: 503, and the local path is not used instead.
	if resp := h.do(runtimedb.RoleOperator, http.MethodPost, path+"?from=offsite", nil, nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unwired off-site restore = %d, want 503", resp.StatusCode)
	}
	h.s.BackupRestoreOffsite = func(_ context.Context, plan string) error { offsite = append(offsite, plan); return nil }

	for _, q := range []string{"", "?from=local"} {
		if resp := h.do(runtimedb.RoleOperator, http.MethodPost, path+q, nil, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("restore%s = %d", q, resp.StatusCode)
		}
	}
	if resp := h.do(runtimedb.RoleOperator, http.MethodPost, path+"?from=offsite", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("off-site restore = %d", resp.StatusCode)
	}
	if resp := h.do(runtimedb.RoleOperator, http.MethodPost, path+"?from=cloud", nil, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("from=cloud = %d, want 400", resp.StatusCode)
	}
	if resp := h.do(runtimedb.RoleButler, http.MethodPost, path+"?from=offsite", nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("butler off-site restore = %d, want 403", resp.StatusCode)
	}
	if len(local) != 2 || len(offsite) != 1 || offsite[0] != "photos" {
		t.Fatalf("engine calls: local %v, offsite %v", local, offsite)
	}
}

func TestBackupConfigurationEndpoints(t *testing.T) {
	h := newHarness(t)
	target := resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindBackupTarget,
		Metadata:     resource.Metadata{Name: "offsite", Owner: "system", Node: "n1"},
		BackupTarget: &resource.BackupTargetSpec{Type: "s3", Endpoint: "https://s3.example.com", Bucket: "wildstone-backups", AccessKeySecret: "secret/access", SecretKeySecret: "secret/key"}}
	plan := resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindBackupPlan,
		Metadata:   resource.Metadata{Name: "photos", Owner: "capability/photos", Node: "n1"},
		BackupPlan: &resource.BackupPlanSpec{Volumes: []string{"photos"}, Repository: "repo", PasswordSecret: "secret/restic", Schedule: "daily", Class: "critical"}}
	for _, r := range []resource.Resource{target, plan} {
		if err := h.s.State.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	var listed struct {
		Plans   []backupPlanView      `json:"plans"`
		Targets []backupTargetView    `json:"targets"`
		Runs    []runtimedb.BackupRun `json:"runs"`
	}
	if resp := h.do(runtimedb.RoleButler, http.MethodGet, "/api/v1/backups", nil, &listed); resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", resp.StatusCode)
	}
	if len(listed.Plans) != 1 || listed.Plans[0].Name != "photos" || len(listed.Runs) != 0 {
		t.Fatalf("never-run plans missing from list: %+v", listed)
	}
	if len(listed.Targets) != 1 || listed.Targets[0].Spec.Bucket != "wildstone-backups" {
		t.Fatalf("targets = %+v", listed.Targets)
	}

	tested := ""
	h.s.BackupTargetTest = func(_ context.Context, name string) error { tested = name; return nil }
	if resp := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/backups/targets/offsite/test", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("read-only target test = %d", resp.StatusCode)
	}
	if tested != "offsite" {
		t.Fatalf("tested %q", tested)
	}
	h.s.BackupEstimate = func(_ context.Context, got resource.Resource) (uint64, string, error) {
		if got.Metadata.Name != "photos" || got.BackupPlan.Repository != "repo" {
			t.Fatalf("estimate received %+v", got)
		}
		return 42 << 30, "42.0 GiB of sources; enough local space", nil
	}
	var estimate struct {
		SourceBytes uint64 `json:"sourceBytes"`
		DiskCheck   string `json:"diskCheck"`
	}
	if resp := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/backups/plans/photos/estimate",
		backupPlanRequest{Node: "n1", Spec: *plan.BackupPlan}, &estimate); resp.StatusCode != http.StatusOK {
		t.Fatalf("estimate = %d", resp.StatusCode)
	}
	if estimate.SourceBytes != 42<<30 || estimate.DiskCheck == "" {
		t.Fatalf("estimate body = %+v", estimate)
	}

	req := backupTargetRequest{Name: "archive", Node: "n1", Spec: resource.BackupTargetSpec{
		Type: "s3", Endpoint: "https://s3.example.com", Bucket: "archive-bucket", AccessKeySecret: "secret/archive-access", SecretKeySecret: "secret/archive-key"}}
	if resp := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/backups/targets", req, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("target proposal = %d", resp.StatusCode)
	}
	proposed, err := h.s.DB.PlansInState(runtimedb.PlanProposed)
	if err != nil || len(proposed) != 1 {
		t.Fatalf("proposal records = %+v, %v", proposed, err)
	}
}

func TestContainerLogsEndpointDemuxes(t *testing.T) {
	h := newHarness(t)
	// Framed docker stream: stdout "hello\n", stderr "oops\n".
	framed := append(frame(1, "hello\n"), frame(2, "oops\n")...)
	h.s.Agents = applierFunc(func(_ context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
		logsOp := op.GetContainerLogs()
		if logsOp == nil || node != "n1" {
			t.Fatalf("unexpected dispatch: node=%s op=%v", node, op)
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_ContainerLogs{ContainerLogs: &wsv1.ContainerLogsResult{
			Logs: framed,
		}}}, nil
	})

	var out struct {
		Logs string `json:"logs"`
	}
	if resp := h.do(runtimedb.RoleButler, http.MethodGet, "/api/v1/nodes/n1/containers/app/logs?tail=50", nil, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("logs = %d", resp.StatusCode)
	}
	if out.Logs != "hello\noops\n" {
		t.Fatalf("logs = %q, want demuxed text", out.Logs)
	}
}

func TestDemuxDockerStream(t *testing.T) {
	if got := demuxDockerStream([]byte("raw tty output")); got != "raw tty output" {
		t.Fatalf("unframed passthrough = %q", got)
	}
	if got := demuxDockerStream(nil); got != "" {
		t.Fatalf("empty = %q", got)
	}
	// Truncated frame header falls back to raw.
	trunc := frame(1, "x")[:5]
	if got := demuxDockerStream(trunc); got != string(trunc) {
		t.Fatalf("truncated = %q", got)
	}
}

func frame(stream byte, payload string) []byte {
	n := len(payload)
	head := []byte{stream, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	return append(head, payload...)
}

// applierFunc adapts a function to reconcile.Applier for tests.
type applierFunc func(ctx context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error)

func (f applierFunc) Apply(ctx context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	return f(ctx, node, op)
}

// TestProposalPreflight: a proposal the backup preflight refuses never
// becomes a plan, and says why; the check sees the proposed resources.
func TestProposalPreflight(t *testing.T) {
	h := newHarness(t)
	var saw []string
	h.s.BackupPreflight = func(puts []resource.Resource) error {
		for _, r := range puts {
			saw = append(saw, string(r.Kind)+"/"+r.Metadata.Name)
		}
		return errors.New("BackupPlan photos: its local copy will not fit")
	}
	doc := containerYAML(t, "immich", "ghcr.io/immich-app/immich-server@"+testDigest, nil)
	resp := h.do(runtimedb.RoleOperator, "POST", "/api/v1/plans", proposalRequest{Summary: "x", Put: []string{doc}}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("refused proposal = %d, want 422", resp.StatusCode)
	}
	if len(saw) != 1 || saw[0] != "Container/immich" {
		t.Fatalf("preflight saw %v", saw)
	}
	plans, err := h.s.DB.PlansInState(runtimedb.PlanProposed)
	if err != nil || len(plans) != 0 {
		t.Fatalf("a refused proposal was recorded: %v %v", plans, err)
	}

	h.s.BackupPreflight = func([]resource.Resource) error { return nil }
	if resp := h.do(runtimedb.RoleOperator, "POST", "/api/v1/plans", proposalRequest{Summary: "x", Put: []string{doc}}, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("accepted proposal = %d", resp.StatusCode)
	}
}
