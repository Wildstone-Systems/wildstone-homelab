package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

type backupInventoryAgent struct {
	secrets     map[string][]string
	unavailable map[string]bool
}

func (f *backupInventoryAgent) Apply(_ context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	if op.GetListSecrets() == nil {
		return nil, errors.New("unexpected non-inventory operation")
	}
	if f.unavailable[node] {
		return nil, errors.New("synthetic inventory outage")
	}
	out := &wsv1.ListSecretsResult{}
	for _, name := range f.secrets[node] {
		out.Secrets = append(out.Secrets, &wsv1.SecretInfo{Name: name, Version: 1})
	}
	return &wsv1.OpResult{Result: &wsv1.OpResult_ListSecrets{ListSecrets: out}}, nil
}

func putBackupPasswordFixture(t *testing.T, h *harness, planName, node, owner, secret string, provided bool) {
	t.Helper()
	for _, r := range []resource.Resource{
		{ApiVersion: resource.ApiVersion, Kind: resource.KindSecret,
			Metadata: resource.Metadata{Name: secret, Node: node, Owner: owner}, Secret: &resource.SecretSpec{Name: secret, Provided: provided}},
		{ApiVersion: resource.ApiVersion, Kind: resource.KindBackupPlan,
			Metadata: resource.Metadata{Name: planName, Node: node, Owner: owner}, BackupPlan: &resource.BackupPlanSpec{
				Volumes: []string{"data"}, Repository: "repo", PasswordSecret: "secret/" + secret, Schedule: "daily"}},
	} {
		if err := h.s.State.Put(r); err != nil {
			t.Fatal(err)
		}
	}
}

func backupPlanStatus(t *testing.T, h *harness, role runtimedb.Role, plan string) backupPasswordStatus {
	t.Helper()
	var out struct {
		Plans []backupPlanView `json:"plans"`
	}
	if resp := h.do(role, http.MethodGet, "/api/v1/backups", nil, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("list backups = %d", resp.StatusCode)
	}
	for _, p := range out.Plans {
		if p.Name == plan {
			return p.Dependency
		}
	}
	t.Fatalf("plan %s missing from response: %+v", plan, out.Plans)
	return backupPasswordStatus{}
}

func TestBackupPasswordStatusAndRepair(t *testing.T) {
	h := newHarness(t)
	fake := &backupInventoryAgent{secrets: map[string][]string{"node-example": {}}, unavailable: map[string]bool{}}
	h.s.Agents = fake
	pokes := 0
	h.s.OnBackupRepair = func() { pokes++ }
	putBackupPasswordFixture(t, h, "photos", "node-example", "capability/photos", "restic-photos", false)

	readOnly := backupPlanStatus(t, h, runtimedb.RoleButler, "photos")
	if readOnly.State != "missing" || readOnly.Source != "generated" || readOnly.Remedy != "reconcile" || readOnly.CanRepair || !readOnly.Held {
		t.Fatalf("Butler status = %+v", readOnly)
	}
	owner := backupPlanStatus(t, h, runtimedb.RoleAdmin, "photos")
	if !owner.CanRepair {
		t.Fatalf("Owner status = %+v", owner)
	}
	for _, role := range []runtimedb.Role{runtimedb.RoleOperator, runtimedb.RoleButler} {
		if resp := h.do(role, http.MethodPost, "/api/v1/backups/photos/repair-password", nil, nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s repair = %d, want 403", role, resp.StatusCode)
		}
	}
	var repaired map[string]string
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/backups/photos/repair-password", nil, &repaired); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("Owner repair = %d", resp.StatusCode)
	}
	if pokes != 1 || repaired["state"] != "pending" || repaired["node"] != "node-example" || repaired["passwordRef"] != "secret/restic-photos" {
		t.Fatalf("repair = %+v, pokes %d", repaired, pokes)
	}

	if _, _, err := h.s.DB.RecordBackupDependencyUnavailable("photos", runtimedb.BackupRunBackup, "closed synthetic reason"); err != nil {
		t.Fatal(err)
	}
	fake.secrets["node-example"] = []string{"restic-photos"}
	present := backupPlanStatus(t, h, runtimedb.RoleAdmin, "photos")
	if present.State != "repaired" || present.Held || present.CanRepair {
		t.Fatalf("repaired status = %+v", present)
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/backups/photos/repair-password", nil, &repaired); resp.StatusCode != http.StatusOK {
		t.Fatalf("idempotent repaired request = %d", resp.StatusCode)
	}
	if pokes != 1 || repaired["state"] != "present" {
		t.Fatalf("idempotent repair = %+v, pokes %d", repaired, pokes)
	}
}

func TestBackupPasswordStatusUsesEveryConfiguredRoute(t *testing.T) {
	h := newHarness(t)
	plan := resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindBackupPlan,
		Metadata: resource.Metadata{Name: "photos", Node: "node-example", Owner: "capability/photos"},
		BackupPlan: &resource.BackupPlanSpec{Volumes: []string{"data"}, Offsite: &resource.BackupOffsite{
			Target: "archive", Route: resource.BackupRouteOffsiteOnly, PasswordSecret: "secret/restic-offsite", Schedule: "daily"}}}
	desired := []resource.Resource{
		{ApiVersion: resource.ApiVersion, Kind: resource.KindSecret,
			Metadata: resource.Metadata{Name: "restic-offsite", Node: "node-example", Owner: "capability/photos"},
			Secret:   &resource.SecretSpec{Name: "restic-offsite"}},
		plan,
	}
	status, err := h.s.backupPasswordState(desired, plan, backupSecretSnapshot{available: true, have: map[string]bool{}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.PasswordRef != "secret/restic-offsite" || status.Source != "generated" || !status.CanRepair || !status.Held {
		t.Fatalf("offsite-only status = %+v", status)
	}

	plan.BackupPlan.Repository = "repo"
	plan.BackupPlan.PasswordSecret = "secret/restic-local"
	plan.BackupPlan.Schedule = "daily"
	plan.BackupPlan.Offsite.Route = resource.BackupRouteLocalAndOffsite
	desired = append([]resource.Resource{
		{ApiVersion: resource.ApiVersion, Kind: resource.KindSecret,
			Metadata: resource.Metadata{Name: "restic-local", Node: "node-example", Owner: "capability/photos"},
			Secret:   &resource.SecretSpec{Name: "restic-local"}},
	}, desired...)
	status, err = h.s.backupPasswordState(desired, plan, backupSecretSnapshot{available: true, have: map[string]bool{"restic-local": true}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.PasswordRef != "secret/restic-offsite" || status.Source != "generated" || !status.Held {
		t.Fatalf("local-and-offsite status = %+v", status)
	}
	desired[1].Secret.Provided = true
	status, err = h.s.backupPasswordState(desired, plan, backupSecretSnapshot{available: true, have: map[string]bool{}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.PasswordRef != "secret/restic-offsite" || status.Source != "provided" || status.Remedy != "enter-secret" || status.CanRepair {
		t.Fatalf("mixed generated/provided status = %+v", status)
	}
}

func TestBackupPasswordRepairRefusesUnconfirmedAndProvidedSecrets(t *testing.T) {
	for _, tc := range []struct {
		name        string
		provided    bool
		unavailable bool
		wantSource  string
		wantRemedy  string
		wantState   string
	}{
		{name: "provided", provided: true, wantSource: "provided", wantRemedy: "enter-secret", wantState: "missing"},
		{name: "inventory unavailable", unavailable: true, wantSource: "generated", wantRemedy: "none", wantState: "inventory-unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.s.Agents = &backupInventoryAgent{secrets: map[string][]string{"node-example": {}}, unavailable: map[string]bool{"node-example": tc.unavailable}}
			pokes := 0
			h.s.OnBackupRepair = func() { pokes++ }
			putBackupPasswordFixture(t, h, "photos", "node-example", "capability/photos", "restic-photos", tc.provided)
			status := backupPlanStatus(t, h, runtimedb.RoleAdmin, "photos")
			if status.State != tc.wantState || status.Source != tc.wantSource || status.Remedy != tc.wantRemedy || status.CanRepair {
				t.Fatalf("status = %+v", status)
			}
			if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/backups/photos/repair-password", nil, nil); resp.StatusCode != http.StatusConflict {
				t.Fatalf("repair = %d, want 409", resp.StatusCode)
			}
			if pokes != 0 {
				t.Fatalf("refused repair poked reconciler %d times", pokes)
			}
		})
	}
}

func TestBackupPasswordRepairRefusesUnrelatedDeclaration(t *testing.T) {
	h := newHarness(t)
	h.s.Agents = &backupInventoryAgent{secrets: map[string][]string{"node-example": {}}, unavailable: map[string]bool{}}
	pokes := 0
	h.s.OnBackupRepair = func() { pokes++ }
	plan := resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindBackupPlan,
		Metadata:   resource.Metadata{Name: "photos", Node: "node-example", Owner: "capability/photos"},
		BackupPlan: &resource.BackupPlanSpec{Volumes: []string{"data"}, Repository: "repo", PasswordSecret: "secret/restic-photos", Schedule: "daily"}}
	// The same name on another node and under another capability grants no
	// authority to repair this plan's dependency.
	unrelated := resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindSecret,
		Metadata: resource.Metadata{Name: "restic-photos", Node: "other-node", Owner: "capability/other"},
		Secret:   &resource.SecretSpec{Name: "restic-photos"}}
	for _, r := range []resource.Resource{plan, unrelated} {
		if err := h.s.State.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	status := backupPlanStatus(t, h, runtimedb.RoleAdmin, "photos")
	if status.Source != "unknown" || status.Remedy != "none" || status.CanRepair {
		t.Fatalf("unrelated declaration status = %+v", status)
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/backups/photos/repair-password", nil, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("unrelated repair = %d, want 409", resp.StatusCode)
	}
	if pokes != 0 {
		t.Fatalf("unrelated repair poked reconciler %d times", pokes)
	}
}

func TestBackupPasswordRepairRejectsCrossSiteConsoleRequest(t *testing.T) {
	h := freshnessHarness(t)
	h.s.Agents = &backupInventoryAgent{secrets: map[string][]string{"node-example": {}}, unavailable: map[string]bool{}}
	pokes := 0
	h.s.OnBackupRepair = func() { pokes++ }
	putBackupPasswordFixture(t, h, "photos", "node-example", "capability/photos", "restic-photos", false)
	session := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/backups/photos/repair-password", nil)
	req.Header.Set(TrustedIngressHeader, "mika")
	req.Header.Set(ConsoleGroupsHeader, ConsoleGroup)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(session)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || pokes != 0 {
		t.Fatalf("cross-site repair = %d, pokes %d", res.StatusCode, pokes)
	}
	sameOrigin, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/backups/photos/repair-password", nil)
	sameOrigin.Header.Set(TrustedIngressHeader, "mika")
	sameOrigin.Header.Set(ConsoleGroupsHeader, ConsoleGroup)
	sameOrigin.Header.Set("Sec-Fetch-Site", "same-origin")
	sameOrigin.AddCookie(session)
	allowed, err := http.DefaultClient.Do(sameOrigin)
	if err != nil {
		t.Fatal(err)
	}
	allowed.Body.Close()
	if allowed.StatusCode != http.StatusAccepted || pokes != 1 {
		t.Fatalf("same-origin repair = %d, pokes %d", allowed.StatusCode, pokes)
	}
	records, err := h.s.DB.ListAudit(20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range records {
		if record.Path == "/api/v1/backups/photos/repair-password" && record.Status == http.StatusForbidden {
			found = true
		}
	}
	if !found {
		t.Fatal("cross-site refusal was not audited")
	}
}

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
