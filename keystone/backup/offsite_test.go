package backup

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
	"github.com/chickenrat17/wildstone-homelab/proto/recoveryseal"
)

func TestReservedWildstonePlanPreparesRecoverySetBeforeUpload(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	publicKey, _, err := recoveryseal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	plan := resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindBackupPlan,
		Metadata: resource.Metadata{Name: "wildstone", Owner: "system", Node: "n1"},
		BackupPlan: &resource.BackupPlanSpec{
			Paths: []string{resource.WholeSiteRecoveryPath}, Class: "critical", RecoveryPublicKey: publicKey,
			Offsite: &resource.BackupOffsite{
				Target: "offsite", Route: resource.BackupRouteOffsiteOnly,
				PasswordSecret: "secret/restic-wildstone-offsite", Schedule: "daily",
			},
		},
	}
	seedAll(t, state, targetResource(false), plan)
	prepared := false
	e.PrepareRecovery = func(_ context.Context, got resource.Resource) error {
		prepared = got.Metadata.Name == "wildstone"
		return nil
	}
	if err := e.RunNow(context.Background(), "wildstone"); err != nil {
		t.Fatal(err)
	}
	if !prepared {
		t.Fatal("whole-site source set was not prepared")
	}
	if _, ok := run.run("offsite-wildstone"); !ok {
		t.Fatal("whole-site off-site upload did not run")
	}
}

func targetResource(pathStyle bool) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindBackupTarget,
		Metadata:   resource.Metadata{Name: "offsite", Owner: "owner", Node: "n1"},
		BackupTarget: &resource.BackupTargetSpec{
			Type: "s3", Endpoint: "https://s3.example.com/", Bucket: "wildstone-backups",
			Prefix: "home", Region: "auto", PathStyle: pathStyle,
			AccessKeySecret: "secret/offsite-access", SecretKeySecret: "secret/offsite-secret",
		},
	}
}

func TestTargetConnectionTestIsReadOnly(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	seedAll(t, state, targetResource(true))
	if err := e.TestTarget(context.Background(), "offsite"); err != nil {
		t.Fatal(err)
	}
	got, ok := run.run("test-target-offsite")
	if !ok {
		t.Fatal("target test did not run")
	}
	if got.node != "n1" || got.spec.Image != defaultS3ProbeImage {
		t.Fatalf("probe placement/image = %s %s", got.node, got.spec.Image)
	}
	command := strings.Join(got.spec.Command, " ")
	if !strings.Contains(command, "s3api head-bucket --bucket wildstone-backups") {
		t.Fatalf("probe command = %q, want read-only HeadBucket", command)
	}
	for _, mutating := range []string{" put-", " cp ", " mb ", " create-bucket", " delete"} {
		if strings.Contains(command, mutating) {
			t.Fatalf("probe command mutates the target: %q", command)
		}
	}
	if got.spec.Env["AWS_ACCESS_KEY_ID"] != "secret/offsite-access" || got.spec.Env["AWS_SECRET_ACCESS_KEY"] != "secret/offsite-secret" {
		t.Fatalf("probe did not preserve secret references: %+v", got.spec.Env)
	}
}

func offsitePlan(name, route string) resource.Resource {
	r := backupPlanResource(name)
	r.BackupPlan.Offsite = &resource.BackupOffsite{
		Target: "offsite", Route: route, PasswordSecret: "secret/restic-" + name + "-offsite",
		Schedule: "weekly", Retention: resource.BackupRetention{KeepWeekly: 8},
	}
	if route == resource.BackupRouteOffsiteOnly {
		r.BackupPlan.Repository, r.BackupPlan.PasswordSecret, r.BackupPlan.Schedule = "", "", ""
		r.BackupPlan.Retention = resource.BackupRetention{}
	}
	return r
}

func seedAll(t *testing.T, state *statefile.Store, rs ...resource.Resource) {
	t.Helper()
	for _, r := range rs {
		if err := r.Validate(); err != nil {
			t.Fatalf("fixture %s/%s invalid: %v", r.Kind, r.Metadata.Name, err)
		}
		if err := state.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.Commit("test", "seed", "seed"); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeRunner) run(name string) (recordedRun, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.runs {
		if r.name == name {
			return r, true
		}
	}
	return recordedRun{}, false
}

// TestLocalAndOffsiteRun: the local backup runs, then its snapshots are
// copied to S3 under the separate off-site password, with the S3 keys
// and both passwords as secret references only.
func TestLocalAndOffsiteRun(t *testing.T) {
	run := &fakeRunner{}
	e, db, state := newEngine(t, run)
	seedAll(t, state, targetResource(false), offsitePlan("photos", resource.BackupRouteLocalAndOffsite))

	reports, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, rep := range reports {
		if rep.Err != "" {
			t.Fatalf("report %+v", rep)
		}
	}
	want := "backup-photos-probe,backup-photos,backup-photos-forget,check-photos,drill-photos,offsite-photos-probe,offsite-photos,offsite-photos-forget,check-offsite-photos,drill-offsite-photos"
	if got := strings.Join(run.names(), ","); got != want {
		t.Fatalf("runs = %s\nwant   %s", got, want)
	}

	cp, _ := run.run("offsite-photos")
	if got := strings.Join(cp.spec.Command, " "); got != "copy --tag "+configTag(offsitePlan("photos", resource.BackupRouteLocalAndOffsite).BackupPlan) {
		t.Fatalf("copy command = %q", got)
	}
	for k, v := range map[string]string{
		"RESTIC_REPOSITORY":      "s3:https://s3.example.com/wildstone-backups/home/photos",
		"RESTIC_PASSWORD":        "secret/restic-photos-offsite",
		"RESTIC_FROM_REPOSITORY": "/repo",
		"RESTIC_FROM_PASSWORD":   "secret/restic-photos",
		"AWS_ACCESS_KEY_ID":      "secret/offsite-access",
		"AWS_SECRET_ACCESS_KEY":  "secret/offsite-secret",
		"AWS_DEFAULT_REGION":     "auto",
	} {
		if cp.spec.Env[k] != v {
			t.Errorf("copy env %s = %q, want %q", k, cp.spec.Env[k], v)
		}
	}
	// restic copy locks the source repository, so it must be writable.
	if len(cp.spec.Mounts) != 1 || cp.spec.Mounts[0].Source != "backup-repo" || cp.spec.Mounts[0].ReadOnly {
		t.Fatalf("copy mounts = %+v, want the local repo read-write and no sources", cp.spec.Mounts)
	}
	forget, _ := run.run("offsite-photos-forget")
	if got := strings.Join(forget.spec.Command, " "); got != "forget --prune --keep-weekly 8" {
		t.Fatalf("off-site forget = %q, want the off-site retention", got)
	}
	if _, err := db.LastBackupRun("photos", runtimedb.BackupRunOffsite, runtimedb.BackupRunSucceeded); err != nil {
		t.Fatalf("no off-site ledger row: %v", err)
	}
}

// TestAppendOnlyTargetNeverDeletes: the node key can add and read
// repository data but cannot maintain it. Every restic pass avoids lock
// deletion, and retention never turns into forget/prune with that key.
func TestAppendOnlyTargetNeverDeletes(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	target := targetResource(false)
	target.BackupTarget.AppendOnly = true
	seedAll(t, state, target, offsitePlan("photos", resource.BackupRouteLocalAndOffsite))

	if err := e.RunNow(context.Background(), "photos"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"offsite-photos-probe", "offsite-photos"} {
		r, ok := run.run(name)
		if !ok || !containsArg(r.spec.Command, "--no-lock") {
			t.Fatalf("%s command = %v, want --no-lock", name, r.spec.Command)
		}
	}
	if _, ok := run.run("offsite-photos-forget"); ok {
		t.Fatalf("append-only target ran forget/prune: %v", run.names())
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// TestOffsiteInitCopiesChunkerParams: a missing off-site repository is
// created with the local repo's chunker parameters, so copies dedupe.
func TestOffsiteInitCopiesChunkerParams(t *testing.T) {
	run := &fakeRunner{exit: map[string]int32{"offsite-photos-probe": 10}}
	e, _, state := newEngine(t, run)
	seedAll(t, state, targetResource(true), offsitePlan("photos", resource.BackupRouteLocalAndOffsite))

	if err := e.RunNow(context.Background(), "photos"); err != nil {
		t.Fatal(err)
	}
	init, ok := run.run("offsite-photos-init")
	if !ok {
		t.Fatalf("runs = %v, want an off-site init", run.names())
	}
	if got := strings.Join(init.spec.Command, " "); got != "init --copy-chunker-params -o s3.bucket-lookup=path" {
		t.Fatalf("init command = %q", got)
	}
	if init.spec.Env["RESTIC_FROM_REPOSITORY"] != "/repo" {
		t.Fatal("init with --copy-chunker-params needs the local repo as its source")
	}
}

// TestOffsiteOnlyRun: sources go straight to S3; nothing local runs and
// nothing mounts a local repository.
func TestOffsiteOnlyRun(t *testing.T) {
	run := &fakeRunner{exit: map[string]int32{"offsite-movies-probe": 10}}
	e, db, state := newEngine(t, run)
	e.Now = func() time.Time { return time.Unix(0, 0) }
	seedAll(t, state, targetResource(false), offsitePlan("movies", resource.BackupRouteOffsiteOnly))

	reports, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 3 {
		t.Fatalf("reports = %+v, want off-site backup + check + drill and no local work", reports)
	}
	if got := strings.Join(run.names(), ","); got != "offsite-movies-probe,offsite-movies-init,offsite-movies,offsite-movies-forget,check-offsite-movies,drill-offsite-movies" {
		t.Fatalf("runs = %s", got)
	}
	init, _ := run.run("offsite-movies-init")
	if got := strings.Join(init.spec.Command, " "); got != "init" {
		t.Fatalf("offsite-only init = %q, want plain init (no local repo to copy parameters from)", got)
	}
	b, _ := run.run("offsite-movies")
	if got := strings.Join(b.spec.Command, " "); got != "backup /backup/immich-db /backup/immich-library --tag "+configTag(offsitePlan("movies", resource.BackupRouteOffsiteOnly).BackupPlan) {
		t.Fatalf("backup command = %q", got)
	}
	for _, m := range b.spec.Mounts {
		if m.Target == "/repo" {
			t.Fatalf("offsite-only run mounted a local repository: %+v", m)
		}
		if !m.ReadOnly {
			t.Fatalf("source mount %+v must be read-only", m)
		}
	}
	if _, ok := b.spec.Env["RESTIC_FROM_REPOSITORY"]; ok {
		t.Fatal("offsite-only backup carries a copy source")
	}
	check, ok := run.run("check-offsite-movies")
	if !ok || strings.Join(check.spec.Command, " ") != "check --read-data-subset=1/20" {
		t.Fatalf("off-site check = %+v", check)
	}
	drill, ok := run.run("drill-offsite-movies")
	if !ok || strings.Join(drill.spec.Command, " ") != "restore latest --target /restore" {
		t.Fatalf("off-site drill = %+v", drill)
	}
	foundScratch := false
	for _, m := range drill.spec.Mounts {
		foundScratch = foundScratch || (m.Source == "movies-offsite-drill" && m.Target == "/restore")
	}
	if !foundScratch {
		t.Fatalf("off-site drill mounts = %+v", drill.spec.Mounts)
	}
	for _, kind := range []runtimedb.BackupRunKind{runtimedb.BackupRunOffsiteCheck, runtimedb.BackupRunOffsiteDrill} {
		if _, err := db.LastBackupRun("movies", kind, runtimedb.BackupRunSucceeded); err != nil {
			t.Fatalf("no successful %s ledger row: %v", kind, err)
		}
	}
}

// TestOffsiteFailureLeavesLocalAlone: a broken endpoint fails only the
// off-site run; the local backup still succeeds and is recorded so.
func TestOffsiteFailureLeavesLocalAlone(t *testing.T) {
	run := &fakeRunner{exit: map[string]int32{"offsite-photos-probe": 1}}
	e, db, state := newEngine(t, run)
	seedAll(t, state, targetResource(false), offsitePlan("photos", resource.BackupRouteLocalAndOffsite))

	reports, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var localOK, offsiteFailed bool
	for _, rep := range reports {
		localOK = localOK || (rep.Kind == runtimedb.BackupRunBackup && rep.Err == "")
		offsiteFailed = offsiteFailed || (rep.Kind == runtimedb.BackupRunOffsite && strings.Contains(rep.Err, "off-site repository check: restic exited 1"))
	}
	if !localOK || !offsiteFailed {
		t.Fatalf("reports = %+v, want local ok and off-site failed", reports)
	}
	if _, ok := run.run("offsite-photos-init"); ok {
		t.Fatal("a failed probe (not exit 10) must never init: a wrong key is not a missing repository")
	}
	if _, err := db.LastBackupRun("photos", runtimedb.BackupRunBackup, runtimedb.BackupRunSucceeded); err != nil {
		t.Fatal("local backup not recorded as succeeded")
	}
}

// TestOffsiteHasItsOwnSchedule: a daily local plan with a weekly off-site
// copy runs the copy weekly.
func TestOffsiteHasItsOwnSchedule(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	seedAll(t, state, targetResource(false), offsitePlan("photos", resource.BackupRouteLocalAndOffsite))
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	e.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	reports, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Kind != runtimedb.BackupRunBackup {
		t.Fatalf("day-2 reports = %+v, want only the daily local backup", reports)
	}

	e.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	reports, err = e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rep := range reports {
		found = found || rep.Kind == runtimedb.BackupRunOffsite
	}
	if !found {
		t.Fatalf("day-8 reports = %+v, want the weekly off-site run", reports)
	}
}

// TestMissingTargetFailsOffsiteRun: a plan whose target is gone fails its
// off-site run with a reason, instead of running without credentials.
func TestMissingTargetFailsOffsiteRun(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	seedAll(t, state, offsitePlan("photos", resource.BackupRouteLocalAndOffsite))

	err := e.RunNow(context.Background(), "photos")
	if err == nil || !strings.Contains(err.Error(), `no BackupTarget "offsite"`) {
		t.Fatalf("err = %v, want the missing target named", err)
	}
	for _, n := range run.names() {
		if strings.HasPrefix(n, "offsite-") {
			t.Fatalf("an off-site container ran with no target: %v", run.names())
		}
	}
}

// TestRestoreFromOffsite: an offsite-only plan restores from S3 by
// default; a local-and-offsite plan does when asked; a plan with no
// off-site copy refuses.
func TestRestoreFromOffsite(t *testing.T) {
	run := &fakeRunner{}
	e, db, state := newEngine(t, run)
	seedAll(t, state, targetResource(false),
		offsitePlan("movies", resource.BackupRouteOffsiteOnly),
		offsitePlan("photos", resource.BackupRouteLocalAndOffsite),
		backupPlanResource("docs"))

	if err := e.RestoreAlongside(context.Background(), "movies"); err != nil {
		t.Fatal(err)
	}
	rs, ok := run.run("restore-offsite-movies")
	if !ok {
		t.Fatalf("runs = %v, want an off-site restore for the offsite-only plan", run.names())
	}
	if rs.spec.Env["RESTIC_REPOSITORY"] != "s3:https://s3.example.com/wildstone-backups/home/movies" {
		t.Fatalf("restore repo = %q", rs.spec.Env["RESTIC_REPOSITORY"])
	}
	scratch := false
	for _, m := range rs.spec.Mounts {
		scratch = scratch || (m.Source == "movies-restore" && m.Target == "/restore" && !m.ReadOnly)
		if m.Source == "immich-library" || m.Source == "immich-db" {
			t.Fatalf("restore mounted a live volume: %+v", m)
		}
	}
	if !scratch {
		t.Fatalf("restore mounts = %+v, want movies-restore at /restore", rs.spec.Mounts)
	}

	if err := e.RestoreOffsite(context.Background(), "photos"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LastBackupRun("photos", runtimedb.BackupRunOffsiteRestore, runtimedb.BackupRunSucceeded); err != nil {
		t.Fatalf("no off-site restore ledger row: %v", err)
	}
	if err := e.RestoreOffsite(context.Background(), "docs"); err == nil {
		t.Fatal("restoring off-site for a plan with no off-site copy must fail")
	}
}

// TestOffsiteSpecsValidate: every off-site container spec passes the
// same validation any desired-state container meets.
func TestOffsiteSpecsValidate(t *testing.T) {
	run := &fakeRunner{exit: map[string]int32{"offsite-photos-probe": 10, "offsite-movies-probe": 10}}
	e, _, state := newEngine(t, run)
	seedAll(t, state, targetResource(true),
		offsitePlan("photos", resource.BackupRouteLocalAndOffsite),
		offsitePlan("movies", resource.BackupRouteOffsiteOnly))
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, rr := range run.runs {
		res := resource.Resource{Kind: resource.KindContainer, Metadata: resource.Metadata{Name: rr.name, Owner: Principal, Node: rr.node}, Container: &rr.spec}
		if err := res.Validate(); err != nil {
			t.Errorf("%s: %v", rr.name, err)
		}
	}
}

// blockingRunner holds every run of one container name until released.
type blockingRunner struct {
	fakeRunner
	hold    string
	release chan struct{}
	started chan struct{}
}

func (b *blockingRunner) RunContainer(ctx context.Context, node, name string, spec resource.ContainerSpec) error {
	if name == b.hold {
		close(b.started)
		<-b.release
	}
	return b.fakeRunner.RunContainer(ctx, node, name, spec)
}

// TestSlowPlanDoesNotBlockOthers: a plan stuck in a days-long upload
// neither holds up another plan's backup nor gets started twice.
func TestSlowPlanDoesNotBlockOthers(t *testing.T) {
	run := &blockingRunner{hold: "offsite-movies", release: make(chan struct{}), started: make(chan struct{})}
	e, db, state := newEngine(t, run)
	seedAll(t, state, targetResource(false), offsitePlan("movies", resource.BackupRouteOffsiteOnly), backupPlanResource("docs"))

	done := make(chan []RunReport, 4)
	wg, err := e.start(context.Background(), func(r []RunReport) { done <- r })
	if err != nil {
		t.Fatal(err)
	}
	<-run.started
	select {
	case reps := <-done:
		if reps[0].Plan != "docs" {
			t.Fatalf("first finished = %+v, want docs", reps)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("docs never finished while movies was uploading")
	}
	if _, err := db.LastBackupRun("docs", runtimedb.BackupRunBackup, runtimedb.BackupRunSucceeded); err != nil {
		t.Fatal("docs backup not recorded")
	}

	// A second tick while movies is still uploading does not start it again.
	wg2, err := e.start(context.Background(), func(r []RunReport) { done <- r })
	if err != nil {
		t.Fatal(err)
	}
	wg2.Wait()
	close(run.release)
	wg.Wait()
	count := 0
	for _, n := range run.names() {
		if n == "offsite-movies" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("offsite-movies ran %d times, want 1", count)
	}
}

// TestRunnerHonorsContextDeadline: an off-site run's own deadline, not
// the runner's 30-minute default, bounds it.
func TestRunnerHonorsContextDeadline(t *testing.T) {
	app := &scriptedApplier{inspects: []*wsv1.OpResult{inspected(true, "running", 0)}}
	r := &ApplierRunner{Applier: app, Poll: time.Millisecond, Timeout: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := r.RunContainer(ctx, "n1", "offsite-x", resource.ContainerSpec{Image: testResticImage}); err == nil {
		t.Fatal("a run past its context deadline must fail")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the runner waited out its own Timeout instead of the context deadline")
	}
}
