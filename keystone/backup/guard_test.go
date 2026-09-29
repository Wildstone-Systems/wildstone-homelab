package backup

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
	"google.golang.org/protobuf/proto"
)

// fakeAgent answers DiskSpace and MeasureUsage from tables keyed by path
// or volume name. tooOld answers every op as an unknown operation, the
// way an agent below protocol minor 19 does.
type fakeAgent struct {
	mu     sync.Mutex
	disks  map[string]*wsv1.DiskSpaceResult
	usage  map[string]uint64
	tooOld bool
	// tooOldUsage fails MeasureUsage only, as a walk past 1M entries does.
	tooOldUsage bool
	asked       []string
}

func (f *fakeAgent) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tooOld {
		return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{Code: wsv1.ErrorCode_ERROR_CODE_INVALID, Message: "unknown operation"}}}, nil
	}
	notFound := &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{Code: wsv1.ErrorCode_ERROR_CODE_NOT_FOUND}}}
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_DiskSpace:
		key := o.DiskSpace.GetPath() + o.DiskSpace.GetVolume()
		f.asked = append(f.asked, "disk:"+key)
		d, ok := f.disks[key]
		if !ok {
			return notFound, nil
		}
		// A copy, so setFree never races a reader.
		c := proto.Clone(d).(*wsv1.DiskSpaceResult)
		return &wsv1.OpResult{Result: &wsv1.OpResult_DiskSpace{DiskSpace: c}}, nil
	case *wsv1.Operation_MeasureUsage:
		if f.tooOldUsage {
			return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{Code: wsv1.ErrorCode_ERROR_CODE_RUNTIME, Message: "usage walk exceeded 1M entries"}}}, nil
		}
		key := o.MeasureUsage.GetPath() + o.MeasureUsage.GetVolume()
		f.asked = append(f.asked, "usage:"+key)
		n, ok := f.usage[key]
		if !ok {
			return notFound, nil
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_MeasureUsage{MeasureUsage: &wsv1.MeasureUsageResult{TotalBytes: n}}}, nil
	}
	return &wsv1.OpResult{}, nil
}

func (f *fakeAgent) setFree(key string, free uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disks[key].FreeBytes = free
}

func disk(total, free uint64, mp, fstype string) *wsv1.DiskSpaceResult {
	d := &wsv1.DiskSpaceResult{TotalBytes: total, FreeBytes: free, MountPoint: mp, Fstype: fstype, Exists: true}
	if mp == nasTarget {
		d.Source = "nas.example.internal:/media"
	}
	return d
}

const nasTarget = "/srv/wildstone/nas/media"

func nasMount() resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindNetworkMount,
		Metadata:     resource.Metadata{Name: "media", Owner: "owner", Node: "n1"},
		NetworkMount: &resource.NetworkMountSpec{Type: "nfs4", Source: "nas.example.internal:/media", Target: nasTarget},
	}
}

// nasPlan backs up a movie folder on the NAS into a repository that is
// also on the NAS.
func nasPlan(name string) resource.Resource {
	r := backupPlanResource(name)
	r.BackupPlan.Volumes = nil
	r.BackupPlan.Paths = []string{nasTarget + "/movies"}
	r.BackupPlan.Repository = nasTarget + "/backups/" + name
	return r
}

// TestNASPathsAreSourcesAndRepository: a NAS folder is mounted read-only
// at /backup<path>, and a repository path read-write at /repo.
func TestNASPathsAreSourcesAndRepository(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	e.Agent = &fakeAgent{
		disks: map[string]*wsv1.DiskSpaceResult{
			nasTarget + "/movies":         disk(8<<40, 4<<40, nasTarget, "nfs4"),
			nasTarget + "/backups/movies": disk(8<<40, 4<<40, nasTarget, "nfs4"),
		},
		usage: map[string]uint64{nasTarget + "/movies": 1 << 40, nasTarget + "/backups/movies": 1 << 40},
	}
	seedAll(t, state, nasMount(), nasPlan("movies"))

	if err := e.RunNow(context.Background(), "movies"); err != nil {
		t.Fatal(err)
	}
	b, _ := run.run("backup-movies")
	if got := strings.Join(b.spec.Command, " "); got != "backup /backup/srv/wildstone/nas/media/movies --tag "+configTag(nasPlan("movies").BackupPlan) {
		t.Fatalf("backup command = %q", got)
	}
	var repoOK, srcOK bool
	for _, m := range b.spec.Mounts {
		repoOK = repoOK || (m.Source == nasTarget+"/backups/movies" && m.Target == "/repo" && !m.ReadOnly)
		srcOK = srcOK || (m.Source == nasTarget+"/movies" && m.Target == "/backup"+nasTarget+"/movies" && m.ReadOnly)
	}
	if !repoOK || !srcOK {
		t.Fatalf("mounts = %+v", b.spec.Mounts)
	}
}

// TestUnmountedShareIsNeverBackedUp: with the NAS down, the folder is on
// the host's own disk — an empty directory. The run fails and restic
// never starts, so no "nothing changed" snapshot is ever taken.
func TestUnmountedShareIsNeverBackedUp(t *testing.T) {
	run := &fakeRunner{}
	e, db, state := newEngine(t, run)
	e.Agent = &fakeAgent{disks: map[string]*wsv1.DiskSpaceResult{
		nasTarget + "/movies":         disk(100<<30, 50<<30, "/srv/wildstone", "ext4"),
		nasTarget + "/backups/movies": disk(100<<30, 50<<30, "/srv/wildstone", "ext4"),
	}}
	seedAll(t, state, nasMount(), nasPlan("movies"))

	err := e.RunNow(context.Background(), "movies")
	if err == nil || !strings.Contains(err.Error(), "is not mounted at "+nasTarget) {
		t.Fatalf("err = %v, want the unmounted share named", err)
	}
	if n := len(run.names()); n != 0 {
		t.Fatalf("restic ran %v with the share missing", run.names())
	}
	if _, err := db.LastBackupRun("movies", runtimedb.BackupRunBackup, runtimedb.BackupRunFailed); err != nil {
		t.Fatal("no failed ledger row for the unmounted share")
	}
}

// TestOldAgentAndNAS: an agent that cannot answer DiskSpace cannot prove
// a share is mounted, so a NAS plan does not run; a volume-only plan
// still does, as before.
func TestOldAgentAndNAS(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	e.Agent = &fakeAgent{tooOld: true}
	seedAll(t, state, nasMount(), nasPlan("movies"), backupPlanResource("photos"))

	if err := e.RunNow(context.Background(), "movies"); err == nil || !strings.Contains(err.Error(), "cannot confirm NAS share") {
		t.Fatalf("NAS plan on an old agent: err = %v", err)
	}
	if err := e.RunNow(context.Background(), "photos"); err != nil {
		t.Fatalf("volume plan on an old agent: %v", err)
	}
}

// TestMissingSourceFolder: a source folder that is not there fails the
// run rather than backing up nothing.
func TestMissingSourceFolder(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	missing := disk(100<<30, 50<<30, nasTarget, "nfs4")
	missing.Exists = false
	e.Agent = &fakeAgent{disks: map[string]*wsv1.DiskSpaceResult{
		nasTarget + "/movies":         missing,
		nasTarget + "/backups/movies": disk(100<<30, 50<<30, nasTarget, "nfs4"),
	}}
	seedAll(t, state, nasMount(), nasPlan("movies"))
	if err := e.RunNow(context.Background(), "movies"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v", err)
	}
}

// TestLowDiskWithoutOffsiteFails: no room and nowhere else to go — the
// run fails before restic writes a byte, and says why.
func TestLowDiskWithoutOffsiteFails(t *testing.T) {
	run := &fakeRunner{}
	e, db, state := newEngine(t, run)
	e.Agent = &fakeAgent{
		disks: map[string]*wsv1.DiskSpaceResult{"backup-repo": disk(100<<30, 30<<30, "/", "ext4")},
		usage: map[string]uint64{"immich-library": 40 << 30, "immich-db": 1 << 30, "backup-repo": 0},
	}
	seedAll(t, state, backupPlanResource("photos"))

	err := e.RunNow(context.Background(), "photos")
	var low *LowDiskError
	if !errors.As(err, &low) {
		t.Fatalf("err = %v, want a LowDiskError", err)
	}
	for _, n := range run.names() {
		if n == "backup-photos" {
			t.Fatal("the backup pass ran with no room")
		}
	}
	last, _ := db.LastBackupRun("photos", runtimedb.BackupRunBackup, runtimedb.BackupRunFailed)
	if !strings.Contains(last.Detail, "skipped the local copy") || !strings.Contains(last.Detail, "reserve") {
		t.Fatalf("ledger detail = %q", last.Detail)
	}
}

// TestLowDiskFallsBackOffsite: no room for the local copy of a
// local-and-offsite plan — the sources go straight to S3 this run.
func TestLowDiskFallsBackOffsite(t *testing.T) {
	run := &fakeRunner{exit: map[string]int32{"offsite-photos-probe": 10}}
	e, db, state := newEngine(t, run)
	e.Agent = &fakeAgent{
		disks: map[string]*wsv1.DiskSpaceResult{"backup-repo": disk(100<<30, 30<<30, "/", "ext4")},
		usage: map[string]uint64{"immich-library": 40 << 30, "immich-db": 1 << 30},
	}
	seedAll(t, state, targetResource(false), offsitePlan("photos", resource.BackupRouteLocalAndOffsite))

	reports, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var localSkipped, offsiteOK bool
	for _, rep := range reports {
		localSkipped = localSkipped || (rep.Kind == runtimedb.BackupRunBackup && strings.Contains(rep.Err, "skipped the local copy"))
		offsiteOK = offsiteOK || (rep.Kind == runtimedb.BackupRunOffsite && rep.Err == "")
	}
	if !localSkipped || !offsiteOK {
		t.Fatalf("reports = %+v, want local skipped and a clean off-site run", reports)
	}
	send, _ := run.run("offsite-photos")
	if got := strings.Join(send.spec.Command, " "); !strings.HasPrefix(got, "backup /backup/immich-db /backup/immich-library --tag wscfg-") {
		t.Fatalf("fallback command = %q, want a direct backup, not a copy", got)
	}
	for _, m := range send.spec.Mounts {
		if m.Target == "/repo" {
			t.Fatal("the direct fallback mounted the local repository")
		}
	}
	// The local repo had no chunker parameters to copy: a plain init.
	if init, ok := run.run("offsite-photos-init"); !ok || !strings.Contains(strings.Join(init.spec.Command, " "), "init") {
		t.Fatalf("runs = %v", run.names())
	}
	if _, err := db.LastBackupRun("photos", runtimedb.BackupRunOffsite, runtimedb.BackupRunSucceeded); err != nil {
		t.Fatal("fallback off-site run not recorded")
	}
	// Nothing ran into the local repository.
	for _, n := range run.names() {
		if strings.HasPrefix(n, "backup-photos") {
			t.Fatalf("a local pass ran: %v", run.names())
		}
	}
}

// TestRunNowSaysItWentOffsite: an on-demand run that fell back tells the
// person who asked for it, rather than reporting a plain success.
func TestRunNowSaysItWentOffsite(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	e.Agent = &fakeAgent{
		disks: map[string]*wsv1.DiskSpaceResult{"backup-repo": disk(100<<30, 30<<30, "/", "ext4")},
		usage: map[string]uint64{"immich-library": 40 << 30},
	}
	seedAll(t, state, targetResource(false), offsitePlan("photos", resource.BackupRouteLocalAndOffsite))
	err := e.RunNow(context.Background(), "photos")
	if err == nil || !strings.Contains(err.Error(), "backed up straight off-site instead") {
		t.Fatalf("err = %v", err)
	}
}

// TestSpaceEstimateCountsTheExistingRepository: a repository that already
// holds the data needs little more, so a nearly-full-looking disk with an
// existing copy still runs.
func TestSpaceEstimateCountsTheExistingRepository(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	e.Agent = &fakeAgent{
		disks: map[string]*wsv1.DiskSpaceResult{"backup-repo": disk(1<<40, 150<<30, "/", "ext4")},
		usage: map[string]uint64{"immich-library": 500 << 30, "immich-db": 0, "backup-repo": 600 << 30},
	}
	seedAll(t, state, backupPlanResource("photos"))
	if err := e.RunNow(context.Background(), "photos"); err != nil {
		t.Fatalf("an existing repository was treated as needing the whole library again: %v", err)
	}
}

// TestReserve: the default is the larger of 10% and 20 GiB, capped at a
// quarter of a small disk; an explicit reserve wins.
func TestReserve(t *testing.T) {
	p := &resource.BackupPlanSpec{}
	for _, tc := range []struct{ total, want uint64 }{
		{1 << 40, (1 << 40) / 10}, // 1 TiB: 10%
		{100 << 30, 20 << 30},     // 100 GiB: 20 GiB floor
		{40 << 30, 10 << 30},      // 40 GiB: a quarter, not 20
	} {
		if got := reserveFor(p, tc.total); got != tc.want {
			t.Errorf("reserve(%s) = %s, want %s", humanBytes(tc.total), humanBytes(got), humanBytes(tc.want))
		}
	}
	p.Reserve = "50GiB"
	if got := reserveFor(p, 1<<40); got != 50<<30 {
		t.Errorf("explicit 50GiB = %s", humanBytes(got))
	}
	p.Reserve = "25%"
	if got := reserveFor(p, 100<<30); got != 25<<30 {
		t.Errorf("explicit 25%% = %s", humanBytes(got))
	}
}

// TestPreflight: a proposal whose local copy cannot fit is refused with
// the ways out; one that fits, or cannot be measured yet, goes ahead.
func TestPreflight(t *testing.T) {
	e := &Engine{Agent: &fakeAgent{
		disks: map[string]*wsv1.DiskSpaceResult{"backup-repo": disk(100<<30, 30<<30, "/", "ext4")},
		usage: map[string]uint64{"immich-library": 40 << 30},
	}}
	err := e.Preflight([]resource.Resource{backupPlanResource("photos")})
	if err == nil || !strings.Contains(err.Error(), "offsite-only") || !strings.Contains(err.Error(), "NAS path") {
		t.Fatalf("err = %v, want a refusal naming the ways out", err)
	}
	offsiteOnly := offsitePlan("movies", resource.BackupRouteOffsiteOnly)
	if err := e.Preflight([]resource.Resource{offsiteOnly}); err != nil {
		t.Fatalf("offsite-only needs no local room: %v", err)
	}
	fresh := backupPlanResource("docs")
	fresh.BackupPlan.Repository = "not-created-yet"
	if err := e.Preflight([]resource.Resource{fresh}); err != nil {
		t.Fatalf("an unmeasurable repository must not block the proposal: %v", err)
	}
	if err := (&Engine{Agent: &fakeAgent{tooOld: true}}).Preflight([]resource.Resource{backupPlanResource("photos")}); err != nil {
		t.Fatalf("an old agent must not block the proposal: %v", err)
	}
}

// TestInRunGuardStopsBackup: free space falling to the reserve mid-run
// stops the container, before the disk is full.
func TestInRunGuardStopsBackup(t *testing.T) {
	agent := &fakeAgent{disks: map[string]*wsv1.DiskSpaceResult{"backup-repo": disk(100<<30, 60<<30, "/", "ext4")}}
	e := &Engine{Agent: agent}
	guard := e.inRunGuard("n1", "backup-repo", 20<<30)

	app := &scriptedApplier{inspects: []*wsv1.OpResult{inspected(true, "running", 0)}}
	r := &ApplierRunner{Applier: app, Poll: time.Millisecond, Timeout: 10 * time.Second, GuardEvery: time.Millisecond}
	go func() { time.Sleep(20 * time.Millisecond); agent.setFree("backup-repo", 19<<30) }()
	err := r.RunContainer(withGuard(context.Background(), guard), "n1", "backup-photos", resource.ContainerSpec{Image: testResticImage})
	var low *LowDiskError
	if !errors.As(err, &low) || !strings.Contains(low.Error(), "reached its") {
		t.Fatalf("err = %v, want the guard to stop the run", err)
	}
	if !strings.HasSuffix(strings.Join(app.ops, ","), "stop,remove") {
		t.Fatalf("ops = %v, want the container stopped and removed", app.ops)
	}
}

// TestDiskAlreadyPastReserve: a host whose disk is already fuller than
// its reserve keeps running backups that add almost nothing — the state
// many hosts are in on upgrade — but a backup that needs real room, or
// cannot be sized, does not start.
func TestDiskAlreadyPastReserve(t *testing.T) {
	agent := &fakeAgent{
		disks: map[string]*wsv1.DiskSpaceResult{"backup-repo": disk(250<<30, 24<<30, "/", "ext4")}, // reserve 25 GiB
		// A few bytes of growth: the real case the exit drill found.
		usage: map[string]uint64{"immich-library": 10<<30 + 100, "immich-db": 0, "backup-repo": 12 << 30},
	}
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	e.Agent = agent
	seedAll(t, state, backupPlanResource("photos"))
	if err := e.RunNow(context.Background(), "photos"); err != nil {
		t.Fatalf("an incremental backup on a disk past its reserve was refused: %v", err)
	}

	agent.usage["immich-library"] = 40 << 30 // 48 GiB estimate against a 12 GiB repo
	var low *LowDiskError
	if err := e.RunNow(context.Background(), "photos"); !errors.As(err, &low) {
		t.Fatalf("a backup needing 36 GiB on 24 GiB free ran: %v", err)
	}

	delete(agent.usage, "immich-library")
	agent.usage["immich-library-broken"] = 0
	agent.tooOldUsage = true
	if err := e.RunNow(context.Background(), "photos"); !errors.As(err, &low) {
		t.Fatalf("an unsizeable backup on a disk past its reserve ran: %v", err)
	}
}

// TestWrongShareIsNeverBackedUp: a different share mounted at the
// declared target is refused, not backed up as if it were the NAS.
func TestWrongShareIsNeverBackedUp(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	other := disk(8<<40, 4<<40, nasTarget, "nfs4")
	other.Source = "old-nas.example.internal:/media"
	e.Agent = &fakeAgent{disks: map[string]*wsv1.DiskSpaceResult{
		nasTarget + "/movies": other, nasTarget + "/backups/movies": other,
	}}
	seedAll(t, state, nasMount(), nasPlan("movies"))
	err := e.RunNow(context.Background(), "movies")
	if err == nil || !strings.Contains(err.Error(), "refusing to back up the wrong share") {
		t.Fatalf("err = %v", err)
	}
	if len(run.names()) != 0 {
		t.Fatalf("restic ran against the wrong share: %v", run.names())
	}
}

// TestCopyRunsWhileNASSourceIsDown: a local-and-offsite copy reads only
// the local repository, so the NAS source being down does not stop the
// last good snapshot reaching S3 — while a backup of the source does.
func TestCopyRunsWhileNASSourceIsDown(t *testing.T) {
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	e.Agent = &fakeAgent{disks: map[string]*wsv1.DiskSpaceResult{
		nasTarget + "/movies": disk(100<<30, 50<<30, "/srv/wildstone", "ext4"), // share down
		"backup-repo":         disk(1<<40, 500<<30, "/", "ext4"),
	}}
	p := offsitePlan("movies", resource.BackupRouteLocalAndOffsite)
	p.BackupPlan.Volumes = nil
	p.BackupPlan.Paths = []string{nasTarget + "/movies"}
	seedAll(t, state, nasMount(), targetResource(false), p)

	reports, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var backupRefused, copyOK bool
	for _, rep := range reports {
		backupRefused = backupRefused || (rep.Kind == runtimedb.BackupRunBackup && strings.Contains(rep.Err, "is not mounted"))
		copyOK = copyOK || (rep.Kind == runtimedb.BackupRunOffsite && rep.Err == "")
	}
	if !backupRefused || !copyOK {
		t.Fatalf("reports = %+v, want the backup refused and the copy run", reports)
	}
}

// TestExcludesReachRestic: excludes ride every backup command — local and
// straight off-site — and an absolute one is moved to where the folder
// sits inside the run container.
func TestExcludesReachRestic(t *testing.T) {
	spec := &resource.BackupPlanSpec{
		Paths:   []string{nasTarget + "/movies"},
		Exclude: []string{"*.nfo", "**/Samples", nasTarget + "/movies/Extras"},
	}
	got := strings.Join(backupCommand(spec), " ")
	want := "backup /backup" + nasTarget + "/movies --exclude *.nfo --exclude **/Samples --exclude /backup" + nasTarget + "/movies/Extras --tag " + configTag(spec)
	if got != want {
		t.Fatalf("command = %q\nwant      %q", got, want)
	}

	run := &fakeRunner{exit: map[string]int32{"offsite-movies-probe": 10}}
	e, _, state := newEngine(t, run)
	p := offsitePlan("movies", resource.BackupRouteOffsiteOnly)
	p.BackupPlan.Exclude = []string{"*.nfo"}
	seedAll(t, state, targetResource(false), p)
	if err := e.RunNow(context.Background(), "movies"); err != nil {
		t.Fatal(err)
	}
	b, _ := run.run("offsite-movies")
	if !strings.Contains(strings.Join(b.spec.Command, " "), "--exclude *.nfo") {
		t.Fatalf("off-site backup command lost the exclude: %v", b.spec.Command)
	}
}

// TestConfigTag: the tag names exactly the sources, excludes and consistency
// hooks — the
// same set in any order is the same tag, and adding an exclude or a
// source makes a new one — and the off-site copy asks for the tag the
// local backup just wrote, so older snapshots (made before an exclude)
// never leave.
func TestConfigTag(t *testing.T) {
	a := &resource.BackupPlanSpec{Volumes: []string{"v1", "v2"}, Exclude: []string{"*.iso", "cache"}}
	b := &resource.BackupPlanSpec{Volumes: []string{"v2", "v1"}, Exclude: []string{"cache", "*.iso"}}
	if configTag(a) != configTag(b) {
		t.Fatal("the same sources and excludes in another order gave another tag")
	}
	for name, c := range map[string]*resource.BackupPlanSpec{
		"exclude added":   {Volumes: []string{"v1", "v2"}, Exclude: []string{"*.iso", "cache", "*.tmp"}},
		"exclude gone":    {Volumes: []string{"v1", "v2"}, Exclude: []string{"*.iso"}},
		"source added":    {Volumes: []string{"v1", "v2", "v3"}, Exclude: []string{"*.iso", "cache"}},
		"path not volume": {Volumes: []string{"v1"}, Paths: []string{"/v2"}, Exclude: []string{"*.iso", "cache"}},
		"hook added": {Volumes: []string{"v1", "v2"}, Exclude: []string{"*.iso", "cache"}, PreBackup: []resource.BackupHook{{
			Kind: resource.BackupHookPostgres, Container: "postgres", Database: "app", OutputVolume: "v2",
		}}},
	} {
		if configTag(c) == configTag(a) {
			t.Errorf("%s: tag unchanged", name)
		}
	}
	if !strings.HasPrefix(configTag(a), "wscfg-") || len(configTag(a)) != len("wscfg-")+12 {
		t.Fatalf("tag shape = %q", configTag(a))
	}

	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	p := offsitePlan("photos", resource.BackupRouteLocalAndOffsite)
	p.BackupPlan.Exclude = []string{"*.iso"}
	seedAll(t, state, targetResource(false), p)
	if err := e.RunNow(context.Background(), "photos"); err != nil {
		t.Fatal(err)
	}
	local, _ := run.run("backup-photos")
	cp, _ := run.run("offsite-photos")
	want := configTag(p.BackupPlan)
	if !strings.Contains(strings.Join(local.spec.Command, " "), "--tag "+want) ||
		strings.Join(cp.spec.Command, " ") != "copy --tag "+want {
		t.Fatalf("local %v / copy %v, want both on tag %s", local.spec.Command, cp.spec.Command, want)
	}
}
