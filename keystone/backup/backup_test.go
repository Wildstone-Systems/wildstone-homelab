package backup

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

const testResticImage = "restic/restic@sha256:" + "9940e6c7421ab3a21ee9fdc4e91596d04c792b600d31b102696c75cbf3ac4481"

// fakeRunner records every run and can fail selected container names:
// fail with a transport-style error, exit with a restic exit code.
type fakeRunner struct {
	mu   sync.Mutex
	runs []recordedRun
	fail map[string]bool
	exit map[string]int32
}

type recordedRun struct {
	node string
	name string
	spec resource.ContainerSpec
}

// migrated is the schema, built once for this whole test binary rather
// than replayed into every database these tests open (#231). Each test
// still gets its own file.
var migrated = dbtemplate.New(func(path string) error {
	db, err := runtimedb.Open(path)
	if err != nil {
		return err
	}
	return db.Close()
})

func (f *fakeRunner) RunContainer(_ context.Context, node, name string, spec resource.ContainerSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, recordedRun{node: node, name: name, spec: spec})
	if f.fail[name] {
		return errors.New("boom: " + name)
	}
	if code, ok := f.exit[name]; ok {
		return &ExitError{Name: name, Code: code}
	}
	return nil
}

func (f *fakeRunner) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.runs))
	for _, r := range f.runs {
		out = append(out, r.name)
	}
	return out
}

func backupPlanResource(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindBackupPlan,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/photos", Node: "n1"},
		BackupPlan: &resource.BackupPlanSpec{
			Volumes:        []string{"immich-library", "immich-db"},
			Repository:     "backup-repo",
			PasswordSecret: "secret/restic-" + name,
			Schedule:       "daily",
			Retention:      resource.BackupRetention{KeepDaily: 7, KeepWeekly: 4},
		},
	}
}

func newEngine(t *testing.T, run Runner) (*Engine, *runtimedb.DB, *statefile.Store) {
	t.Helper()
	db, err := runtimedb.Open(migrated.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	state, err := statefile.New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{State: state, DB: db, Run: run, ResticImage: testResticImage}, db, state
}

func seedPlan(t *testing.T, state *statefile.Store, r resource.Resource) {
	t.Helper()
	if err := r.Validate(); err != nil {
		t.Fatalf("test fixture BackupPlan is invalid: %v", err)
	}
	if err := state.Put(r); err != nil {
		t.Fatal(err)
	}
	if err := state.Commit("test", "seed", "add backup plan"); err != nil {
		t.Fatal(err)
	}
}

// TestFirstTickRunsBackupAndDrill: a never-run plan is due for its backup,
// integrity check and restore drill.
func TestFirstTickRunsBackupAndDrill(t *testing.T) {
	run := &fakeRunner{}
	e, db, state := newEngine(t, run)
	seedPlan(t, state, backupPlanResource("photos"))

	reports, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 3 || reports[0].Err != "" || reports[1].Err != "" || reports[2].Err != "" {
		t.Fatalf("reports = %+v, want clean backup + check + drill", reports)
	}

	names := run.names()
	want := []string{"backup-photos-probe", "backup-photos", "backup-photos-forget", "check-photos", "drill-photos"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("runs = %v, want %v", names, want)
	}

	// The backup pass: sources read-only, repo read-write, secret is a
	// reference, command deterministic.
	spec := run.runs[1].spec
	if spec.Image != testResticImage {
		t.Fatalf("image = %s", spec.Image)
	}
	if got := strings.Join(spec.Command, " "); got != "backup /backup/immich-db /backup/immich-library --tag "+configTag(backupPlanResource("photos").BackupPlan) {
		t.Fatalf("backup command = %q", got)
	}
	if spec.Env["RESTIC_PASSWORD"] != "secret/restic-photos" {
		t.Fatalf("password env = %q, must stay a secret reference", spec.Env["RESTIC_PASSWORD"])
	}
	roCount := 0
	for _, m := range spec.Mounts {
		if m.Source == "backup-repo" && (m.ReadOnly || m.Target != "/repo") {
			t.Fatalf("repo mount = %+v, want rw at /repo", m)
		}
		if strings.HasPrefix(m.Target, "/backup/") {
			if !m.ReadOnly {
				t.Fatalf("source mount %+v must be read-only", m)
			}
			roCount++
		}
	}
	if roCount != 2 {
		t.Fatalf("source mounts = %d, want 2", roCount)
	}

	// Forget pass carries the retention flags, and only the declared ones.
	if got := strings.Join(run.runs[2].spec.Command, " "); got != "forget --prune --keep-daily 7 --keep-weekly 4" {
		t.Fatalf("forget command = %q", got)
	}

	// Drill restores alongside: target volume photos-drill, never a source.
	check := run.runs[3].spec
	if got := strings.Join(check.Command, " "); got != "check" {
		t.Fatalf("check command = %q", got)
	}

	drill := run.runs[4].spec
	if got := strings.Join(drill.Command, " "); got != "restore latest --target /restore" {
		t.Fatalf("drill command = %q", got)
	}
	foundScratch := false
	for _, m := range drill.Mounts {
		if m.Source == "photos-drill" && m.Target == "/restore" {
			foundScratch = true
		}
		if m.Source == "immich-library" || m.Source == "immich-db" {
			t.Fatalf("drill mounted a live source volume: %+v", m)
		}
	}
	if !foundScratch {
		t.Fatalf("drill mounts = %+v, want scratch photos-drill at /restore", drill.Mounts)
	}

	// Provenance: two applied plan records by system/backup, ledger rows,
	// events.
	plans, err := db.ListPlans(10)
	if err != nil {
		t.Fatal(err)
	}
	applied := 0
	for _, p := range plans {
		if p.Principal == Principal && p.State == runtimedb.PlanApplied {
			applied++
		}
	}
	if applied != 3 {
		t.Fatalf("applied system/backup plan records = %d, want 3 (run + check + drill)", applied)
	}
	if _, err := db.LastBackupRun("photos", runtimedb.BackupRunBackup, runtimedb.BackupRunSucceeded); err != nil {
		t.Fatalf("no successful backup ledger row: %v", err)
	}
	if _, err := db.LastBackupRun("photos", runtimedb.BackupRunDrill, runtimedb.BackupRunSucceeded); err != nil {
		t.Fatalf("no successful drill ledger row: %v", err)
	}
	if _, err := db.LastBackupRun("photos", runtimedb.BackupRunCheck, runtimedb.BackupRunSucceeded); err != nil {
		t.Fatalf("no successful check ledger row: %v", err)
	}
	events, err := db.EventsAfter(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	for _, ev := range events {
		if ev.Type == "backup.completed" {
			completed++
		}
	}
	if completed != 3 {
		t.Fatalf("backup.completed events = %d, want 3", completed)
	}
}

// TestScheduleGate: a fresh success suppresses re-runs until the interval
// elapses; a clock past the interval makes the plan due again.
func TestScheduleGate(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	seedPlan(t, state, backupPlanResource("photos"))

	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := len(run.names())

	// Immediately again: nothing due.
	reports, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 0 || len(run.names()) != first {
		t.Fatalf("second tick ran anyway: reports=%+v runs=%v", reports, run.names())
	}

	// 25h later: the daily backup is due, the 30-day drill is not.
	e.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	reports, err = e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Kind != runtimedb.BackupRunBackup {
		t.Fatalf("25h reports = %+v, want exactly one backup run", reports)
	}

	// 31 days later: backup, weekly check and monthly drill are due.
	e.Now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	reports, err = e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 3 {
		t.Fatalf("31d reports = %+v, want backup + check + drill", reports)
	}
}

func TestCorruptedRepositoryIsReportedByCheck(t *testing.T) {
	run := &fakeRunner{exit: map[string]int32{"check-photos": 1}}
	e, db, state := newEngine(t, run)
	seedPlan(t, state, backupPlanResource("photos"))

	reports, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var failed *RunReport
	for i := range reports {
		if reports[i].Kind == runtimedb.BackupRunCheck {
			failed = &reports[i]
		}
	}
	if failed == nil || !strings.Contains(failed.Err, "restic exited 1") {
		t.Fatalf("check report = %+v", failed)
	}
	last, err := db.LastBackupRunAny("photos", runtimedb.BackupRunCheck)
	if err != nil {
		t.Fatal(err)
	}
	if last.Status != runtimedb.BackupRunFailed || !strings.Contains(last.Detail, "repository integrity check") {
		t.Fatalf("check ledger = %+v", last)
	}
}

func TestSourceSizeSamplesTrackBackupInputs(t *testing.T) {
	run := &fakeRunner{}
	e, db, state := newEngine(t, run)
	agent := &fakeAgent{usage: map[string]uint64{"immich-library": 800, "immich-db": 200}}
	e.Agent = agent
	seedPlan(t, state, backupPlanResource("photos"))

	if err := e.RunNow(context.Background(), "photos"); err != nil {
		t.Fatal(err)
	}
	agent.usage["immich-library"] = 300
	if err := e.RunNow(context.Background(), "photos"); err != nil {
		t.Fatal(err)
	}
	samples, err := db.LatestBackupSourceSamples("photos", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || samples[0].Bytes != 500 || samples[1].Bytes != 1000 {
		t.Fatalf("source samples = %+v", samples)
	}
}

// TestFailedRunRecordedAndRetried: a failing backup pass yields a failed
// plan record/ledger row/event, and the schedule retries it once
// RetryAfter has passed — not on every tick, and not a full interval later.
func TestFailedRunRecordedAndRetried(t *testing.T) {
	run := &fakeRunner{fail: map[string]bool{"backup-photos": true}}
	e, db, state := newEngine(t, run)
	seedPlan(t, state, backupPlanResource("photos"))

	reports, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var backupRep *RunReport
	for i := range reports {
		if reports[i].Kind == runtimedb.BackupRunBackup {
			backupRep = &reports[i]
		}
	}
	if backupRep == nil || backupRep.Err == "" {
		t.Fatalf("reports = %+v, want a failed backup run", reports)
	}
	if _, err := db.LastBackupRun("photos", runtimedb.BackupRunBackup, runtimedb.BackupRunFailed); err != nil {
		t.Fatalf("no failed ledger row: %v", err)
	}
	failedPlans, err := db.PlansInState(runtimedb.PlanFailed)
	if err != nil {
		t.Fatal(err)
	}
	if len(failedPlans) == 0 {
		t.Fatal("no failed plan record for the failed run")
	}

	// The forget pass never ran after the failed backup pass.
	for _, n := range run.names() {
		if n == "backup-photos-forget" {
			t.Fatal("forget pass ran despite a failed backup pass")
		}
	}

	// The very next tick holds off: a plan that fails every time must not
	// re-run all day.
	run.fail = nil
	reports, err = e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 0 {
		t.Fatalf("tick right after a failure = %+v, want nothing until RetryAfter", reports)
	}

	// Past RetryAfter (default 1h): retried (drill already succeeded, only
	// the backup is re-attempted).
	e.Now = func() time.Time { return time.Now().Add(61 * time.Minute) }
	reports, err = e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Kind != runtimedb.BackupRunBackup || reports[0].Err != "" {
		t.Fatalf("retry reports = %+v, want one clean backup run", reports)
	}
}

// TestExistingRepositoryIsNotInitialized: a probe that exits 0 means the
// repository exists, so no init pass runs at all.
func TestExistingRepositoryIsNotInitialized(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	seedPlan(t, state, backupPlanResource("photos"))

	if err := e.RunNow(context.Background(), "photos"); err != nil {
		t.Fatal(err)
	}
	for _, n := range run.names() {
		if n == "backup-photos-init" {
			t.Fatalf("runs = %v: init ran against an existing repository", run.names())
		}
	}
	probe := run.runs[0]
	if probe.name != "backup-photos-probe" || strings.Join(probe.spec.Command, " ") != "cat config" {
		t.Fatalf("first pass = %s %v, want the read-only probe", probe.name, probe.spec.Command)
	}
	for _, m := range probe.spec.Mounts {
		if strings.HasPrefix(m.Target, "/backup/") {
			t.Fatalf("probe mounted a source volume: %+v", m)
		}
	}
}

// TestMissingRepositoryIsInitialized: probe exit 10 ("repository does not
// exist") runs init, then the backup.
func TestMissingRepositoryIsInitialized(t *testing.T) {
	run := &fakeRunner{exit: map[string]int32{"backup-photos-probe": 10}}
	e, _, state := newEngine(t, run)
	seedPlan(t, state, backupPlanResource("photos"))

	if err := e.RunNow(context.Background(), "photos"); err != nil {
		t.Fatal(err)
	}
	want := "backup-photos-probe,backup-photos-init,backup-photos,backup-photos-forget"
	if got := strings.Join(run.names(), ","); got != want {
		t.Fatalf("runs = %s, want %s", got, want)
	}
}

// TestInitFailureFailsRun: when the repository is missing and init fails,
// the run fails and no backup pass is attempted.
func TestInitFailureFailsRun(t *testing.T) {
	run := &fakeRunner{exit: map[string]int32{"backup-photos-probe": 10, "backup-photos-init": 1}}
	e, db, state := newEngine(t, run)
	seedPlan(t, state, backupPlanResource("photos"))

	err := e.RunNow(context.Background(), "photos")
	if err == nil || !strings.Contains(err.Error(), "repository init: restic exited 1") {
		t.Fatalf("err = %v, want a failed init", err)
	}
	for _, n := range run.names() {
		if n == "backup-photos" {
			t.Fatal("backup pass ran after a failed init")
		}
	}
	if _, err := db.LastBackupRun("photos", runtimedb.BackupRunBackup, runtimedb.BackupRunFailed); err != nil {
		t.Fatalf("no failed ledger row: %v", err)
	}
}

// TestWrongPasswordNeverInits: probe exit 12 is a wrong password on an
// existing repository. The run fails with that reason, and init is never
// tried — it could not help, and the operator must fix the secret.
func TestWrongPasswordNeverInits(t *testing.T) {
	run := &fakeRunner{exit: map[string]int32{"backup-photos-probe": 12}}
	e, db, state := newEngine(t, run)
	seedPlan(t, state, backupPlanResource("photos"))

	if err := e.RunNow(context.Background(), "photos"); err == nil {
		t.Fatal("a wrong password must fail the run")
	}
	if got := strings.Join(run.names(), ","); got != "backup-photos-probe" {
		t.Fatalf("runs = %s, want only the probe", got)
	}
	last, err := db.LastBackupRun("photos", runtimedb.BackupRunBackup, runtimedb.BackupRunFailed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(last.Detail, "wrong repository password") {
		t.Fatalf("ledger detail = %q, want the exit code explained", last.Detail)
	}
}

// TestNonZeroBackupExitFails: restic exit 3 (snapshot saved but some
// files unreadable) is a failed run, not a success, and says why.
func TestNonZeroBackupExitFails(t *testing.T) {
	run := &fakeRunner{exit: map[string]int32{"backup-photos": 3}}
	e, db, state := newEngine(t, run)
	seedPlan(t, state, backupPlanResource("photos"))

	if err := e.RunNow(context.Background(), "photos"); err == nil {
		t.Fatal("exit 3 must fail the run")
	}
	last, err := db.LastBackupRun("photos", runtimedb.BackupRunBackup, runtimedb.BackupRunFailed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(last.Detail, "backup pass: restic exited 3") || !strings.Contains(last.Detail, "incomplete") {
		t.Fatalf("ledger detail = %q", last.Detail)
	}
	if !strings.Contains(last.Detail, "storage-permission failure") {
		t.Fatalf("ledger detail = %q, want the permission diagnosis", last.Detail)
	}
	if _, err := db.LastBackupRun("photos", runtimedb.BackupRunBackup, runtimedb.BackupRunSucceeded); err == nil {
		t.Fatal("a non-zero exit left a succeeded ledger row")
	}
}

// TestFailedRestoreFails: a restore whose container exits non-zero is a
// failed restore in the ledger.
func TestFailedRestoreFails(t *testing.T) {
	run := &fakeRunner{exit: map[string]int32{"restore-photos": 10}}
	e, db, state := newEngine(t, run)
	seedPlan(t, state, backupPlanResource("photos"))

	err := e.RestoreAlongside(context.Background(), "photos")
	if err == nil || !strings.Contains(err.Error(), "repository does not exist") {
		t.Fatalf("err = %v, want a failed restore naming the missing repository", err)
	}
	if _, err := db.LastBackupRun("photos", runtimedb.BackupRunRestore, runtimedb.BackupRunFailed); err != nil {
		t.Fatalf("no failed restore ledger row: %v", err)
	}
}

// TestConcurrentRunRefused: a second run of a plan that is already
// running is refused rather than sharing (or removing) its container.
func TestConcurrentRunRefused(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	seedPlan(t, state, backupPlanResource("photos"))

	e.running.Store("photos", struct{}{})
	err := e.RunNow(context.Background(), "photos")
	if err == nil || !strings.Contains(err.Error(), "already has a run in progress") {
		t.Fatalf("err = %v, want the busy refusal", err)
	}
	if len(run.names()) != 0 {
		t.Fatalf("runs = %v, want none while busy", run.names())
	}
	e.running.Delete("photos")
	if err := e.RunNow(context.Background(), "photos"); err != nil {
		t.Fatalf("run after the first finished: %v", err)
	}
}

// TestNoRetentionSkipsForget: an all-zero retention runs no forget pass.
func TestNoRetentionSkipsForget(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	r := backupPlanResource("photos")
	r.BackupPlan.Retention = resource.BackupRetention{}
	seedPlan(t, state, r)

	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, n := range run.names() {
		if strings.Contains(n, "forget") {
			t.Fatalf("forget pass ran with zero retention: %v", run.names())
		}
	}
}

// TestRestoreAlongside: restores latest into <plan>-restore with full
// provenance; an unknown plan errors.
func TestRestoreAlongside(t *testing.T) {
	run := &fakeRunner{}
	e, db, state := newEngine(t, run)
	seedPlan(t, state, backupPlanResource("photos"))

	if err := e.RestoreAlongside(context.Background(), "photos"); err != nil {
		t.Fatal(err)
	}
	last := run.runs[len(run.runs)-1]
	if last.name != "restore-photos" {
		t.Fatalf("restore container = %s", last.name)
	}
	found := false
	for _, m := range last.spec.Mounts {
		if m.Source == "photos-restore" && m.Target == "/restore" {
			found = true
		}
	}
	if !found {
		t.Fatalf("restore mounts = %+v, want photos-restore at /restore", last.spec.Mounts)
	}
	// An operator restore records as "restore", not "drill" — the ledger
	// says what happened, and it must not reset the drill schedule.
	if _, err := db.LastBackupRun("photos", runtimedb.BackupRunRestore, runtimedb.BackupRunSucceeded); err != nil {
		t.Fatalf("restore left no ledger row: %v", err)
	}
	if _, err := db.LastBackupRun("photos", runtimedb.BackupRunDrill, runtimedb.BackupRunSucceeded); err == nil {
		t.Fatal("an operator restore must not be recorded as a scheduled drill")
	}

	if err := e.RestoreAlongside(context.Background(), "ghost"); err == nil {
		t.Fatal("RestoreAlongside of an undeclared plan must fail")
	}
}

// TestRunSpecsValidate: every container spec the engine composes passes
// resource validation — the same bar any desired-state container meets.
func TestRunSpecsValidate(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	seedPlan(t, state, backupPlanResource("photos"))
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, rr := range run.runs {
		res := resource.Resource{
			Kind:      resource.KindContainer,
			Metadata:  resource.Metadata{Name: rr.name, Owner: Principal, Node: rr.node},
			Container: &rr.spec,
		}
		if err := res.Validate(); err != nil {
			t.Errorf("run %d (%s): composed spec fails validation: %v", i, rr.name, err)
		}
	}
}

// TestTickIsolatesPlans: one plan's failure doesn't stop another's run.
func TestTickIsolatesPlans(t *testing.T) {
	run := &fakeRunner{fail: map[string]bool{"backup-alpha": true}}
	e, _, state := newEngine(t, run)
	a := backupPlanResource("alpha")
	b := backupPlanResource("beta")
	if err := state.Put(a); err != nil {
		t.Fatal(err)
	}
	seedPlan(t, state, b)

	reports, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var alphaErr, betaOK bool
	for _, rep := range reports {
		if rep.Plan == "alpha" && rep.Kind == runtimedb.BackupRunBackup && rep.Err != "" {
			alphaErr = true
		}
		if rep.Plan == "beta" && rep.Kind == runtimedb.BackupRunBackup && rep.Err == "" {
			betaOK = true
		}
	}
	if !alphaErr || !betaOK {
		t.Fatalf("reports = %+v, want alpha failed and beta clean", reports)
	}
}

// TestRunNow: an on-demand run executes immediately, resets the schedule
// clock, and an unknown plan errors.
func TestRunNow(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	seedPlan(t, state, backupPlanResource("photos"))

	if err := e.RunNow(context.Background(), "photos"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(run.names(), ","), "backup-photos") {
		t.Fatalf("runs = %v, want an immediate backup", run.names())
	}

	// The on-demand success gates the schedule: the next tick runs only
	// the (still-unrun) drill, not another backup.
	reports, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, rep := range reports {
		if rep.Kind == runtimedb.BackupRunBackup {
			t.Fatalf("tick re-ran the backup right after RunNow: %+v", reports)
		}
	}

	if err := e.RunNow(context.Background(), "ghost"); err == nil {
		t.Fatal("RunNow of an undeclared plan must fail")
	}
}
