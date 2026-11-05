package vertical

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/backup"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/reconcile"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

const (
	hermesInstance       = "echo"
	hermesNode           = "node-a"
	hermesCapability     = "agent-hermes-" + hermesInstance
	hermesPlan           = hermesCapability
	hermesDataVolume     = hermesCapability + "-data"
	hermesRepoVolume     = hermesCapability + "-backup-repo"
	hermesPasswordSecret = "restic-" + hermesCapability
	hermesResticImage    = "restic/restic@sha256:9940e6c7421ab3a21ee9fdc4e91596d04c792b600d31b102696c75cbf3ac4481"
	hermesDataPath       = "state/synthetic-session.json"
	hermesData           = "synthetic Hermes state"
)

type hermesFixture struct {
	resources []resource.Resource
	state     *statefile.Store
	db        *runtimedb.DB
	agent     *testAgent
	applier   *reconcile.GatewayApplier
}

func newHermesFixture(t *testing.T, suppressPassword bool) *hermesFixture {
	t.Helper()
	pkg, err := pkgspec.Load(filepath.Join(repoRoot(t), "packages", "agent-hermes", "package.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	resources, err := capability.NewEngine().Expand(pkg, map[string]string{
		"instance":     hermesInstance,
		"node":         hermesNode,
		"keystone_url": "https://keystone.example.internal",
		"model_name":   "synthetic-model",
	}, capability.NewCatalog(pkg))
	if err != nil {
		t.Fatal(err)
	}

	state, err := statefile.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resources {
		if err := state.Put(r); err != nil {
			t.Fatalf("statefile.Put(%s/%s): %v", r.Kind, r.Metadata.Name, err)
		}
	}
	if err := state.Commit("vertical-test", "install Agent Hermes", "synthetic first-backup regression"); err != nil {
		t.Fatal(err)
	}

	const joinToken = "REPLACED-JOIN-TOKEN"
	h := startGateway(t, []string{joinToken})
	agent := newTestAgent(t, h, hermesNode, joinToken)
	if !h.gw.WaitConnected(hermesNode, 10*time.Second) {
		t.Fatal("Agent Hermes test node never connected")
	}
	applier := &reconcile.GatewayApplier{Gateway: h.gw}

	provided := 0
	for _, r := range resources {
		if r.Kind != resource.KindSecret || r.Secret == nil || !r.Secret.Provided {
			continue
		}
		provided++
		value := make([]byte, 24)
		if _, err := rand.Read(value); err != nil {
			t.Fatal(err)
		}
		res, err := applier.Apply(context.Background(), hermesNode, &wsv1.Operation{Op: &wsv1.Operation_PutSecret{
			PutSecret: &wsv1.PutSecret{Name: r.Secret.Name, Value: base64.RawURLEncoding.EncodeToString(value)},
		}})
		if err != nil || res.GetError() != nil {
			t.Fatalf("put provided secret %s: result=%v err=%v", r.Secret.Name, res, err)
		}
	}
	if provided != 2 {
		t.Fatalf("Agent Hermes declared %d provided secrets, want provider key and runtime token only", provided)
	}
	if suppressPassword {
		agent.suppressSecret(hermesPasswordSecret)
	}

	r := &reconcile.Reconciler{
		Applier:            applier,
		ManagedPrefix:      testManagedPrefix,
		Approve:            approveEverything,
		MaxApplyRetries:    2,
		RetryBackoff:       time.Millisecond,
		HealthGateAttempts: 3,
		HealthGateBackoff:  time.Millisecond,
	}
	outcome, err := r.Reconcile(context.Background(), hermesNode, resources)
	if err != nil {
		t.Fatalf("reconcile Agent Hermes install: %v", err)
	}
	if !outcome.Applied || outcome.PendingApproval || outcome.Rolledback || outcome.Incident != "" {
		t.Fatalf("Agent Hermes install did not apply its approved Plan: %+v", outcome)
	}
	if !agent.writeVolumeFile(hermesDataVolume, hermesDataPath, []byte(hermesData)) {
		t.Fatalf("reconcile did not create %s", hermesDataVolume)
	}

	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &hermesFixture{resources: resources, state: state, db: db, agent: agent, applier: applier}
}

type hermesBackupRunner struct {
	mu          sync.Mutex
	agent       *testAgent
	repository  bool
	snapshot    map[string][]byte
	commands    []string
	failOnce    map[string]bool
	backupCount int
}

// RunContainer is the backup package's existing fake-runner seam at
// repository fidelity: it models Restic's repository/snapshot effects from
// the closed command and mount specs, but never invokes Docker or handles a
// password value. The node fake above remains the authority for secret
// metadata and the application volumes created by the real Reconciler.
func (r *hermesBackupRunner) RunContainer(_ context.Context, _ string, name string, spec resource.ContainerSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(spec.Command) == 0 {
		return fmt.Errorf("%s: empty restic command", name)
	}
	verb := spec.Command[0]
	r.commands = append(r.commands, strings.Join(spec.Command, " "))
	if r.failOnce[verb] {
		delete(r.failOnce, verb)
		return fmt.Errorf("simulated cancellation at %s", verb)
	}
	switch verb {
	case "cat":
		if !r.repository {
			return &backup.ExitError{Name: name, Code: 10}
		}
	case "init":
		r.repository = true
	case "backup":
		if !r.repository {
			return &backup.ExitError{Name: name, Code: 10}
		}
		r.snapshot = map[string][]byte{}
		r.agent.mu.Lock()
		for _, mount := range spec.Mounts {
			if !strings.HasPrefix(mount.Target, "/backup/") || !mount.ReadOnly {
				continue
			}
			volume := r.agent.volumes[mount.Source]
			if volume == nil {
				continue
			}
			for path, data := range volume.files {
				key := strings.TrimPrefix(mount.Target, "/") + "/" + path
				r.snapshot[key] = append([]byte(nil), data...)
			}
		}
		r.agent.mu.Unlock()
		r.backupCount++
	case "check":
		if !r.repository || len(r.snapshot) == 0 {
			return fmt.Errorf("repository has no snapshot")
		}
	case "restore":
		if !r.repository || len(r.snapshot) == 0 {
			return fmt.Errorf("repository has no snapshot")
		}
		var target string
		for _, mount := range spec.Mounts {
			if mount.Target == "/restore" {
				target = mount.Source
				break
			}
		}
		if target == "" {
			return fmt.Errorf("restore has no isolated target")
		}
		r.agent.mu.Lock()
		volume := r.agent.volumes[target]
		if volume == nil {
			volume = &fakeVolume{owned: true, files: map[string][]byte{}}
			r.agent.volumes[target] = volume
		}
		for path, data := range r.snapshot {
			volume.files[path] = append([]byte(nil), data...)
		}
		r.agent.mu.Unlock()
	case "forget":
		if !r.repository {
			return fmt.Errorf("repository is missing")
		}
	default:
		return fmt.Errorf("unexpected restic command %q", verb)
	}
	return nil
}

func (f *hermesFixture) engine(run backup.Runner) *backup.Engine {
	return &backup.Engine{
		State:       f.state,
		DB:          f.db,
		Run:         run,
		Agent:       f.applier,
		ResticImage: hermesResticImage,
		RetryAfter:  time.Nanosecond,
	}
}

func countRuns(t *testing.T, db *runtimedb.DB, kind runtimedb.BackupRunKind, status runtimedb.BackupRunStatus) int {
	t.Helper()
	runs, err := db.ListBackupRuns(100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, run := range runs {
		if run.PlanName == hermesPlan && run.RunKind == kind && run.Status == status {
			n++
		}
	}
	return n
}

func assertHermesData(t *testing.T, agent *testAgent, volume, path string) {
	t.Helper()
	got, ok := agent.readVolumeFile(volume, path)
	if !ok || string(got) != hermesData {
		t.Fatalf("%s/%s = %q, present=%v; want intact synthetic Hermes data", volume, path, got, ok)
	}
}

func TestAgentHermesFirstBackupRestoreLifecycle(t *testing.T) {
	t.Run("fresh install backs up, checks, drills, and restores alongside", func(t *testing.T) {
		f := newHermesFixture(t, false)
		if version, ok := f.agent.secretVersion(hermesPasswordSecret); !ok || version != 1 {
			t.Fatalf("generated backup password metadata = version %d, present=%v; want version 1", version, ok)
		}
		runner := &hermesBackupRunner{agent: f.agent, failOnce: map[string]bool{}}
		reports, err := f.engine(runner).Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(reports) != 3 {
			t.Fatalf("first tick reports = %+v, want backup + check + drill", reports)
		}
		for _, report := range reports {
			if report.Err != "" {
				t.Fatalf("first tick report = %+v", report)
			}
		}
		if err := f.engine(runner).RestoreAlongside(context.Background(), hermesPlan); err != nil {
			t.Fatal(err)
		}
		assertHermesData(t, f.agent, hermesDataVolume, hermesDataPath)
		assertHermesData(t, f.agent, hermesPlan+"-drill", "backup/"+hermesDataVolume+"/"+hermesDataPath)
		assertHermesData(t, f.agent, hermesPlan+"-restore", "backup/"+hermesDataVolume+"/"+hermesDataPath)
	})

	t.Run("restart before first backup reuses password and repository", func(t *testing.T) {
		f := newHermesFixture(t, false)
		runner := &hermesBackupRunner{agent: f.agent, failOnce: map[string]bool{"backup": true}}
		if err := f.engine(runner).RunNow(context.Background(), hermesPlan); err == nil || !strings.Contains(err.Error(), "simulated cancellation") {
			t.Fatalf("first run error = %v, want synthetic cancellation", err)
		}
		if !runner.repository {
			t.Fatal("repository initialized before cancellation was lost")
		}
		if err := f.engine(runner).RunNow(context.Background(), hermesPlan); err != nil {
			t.Fatal(err)
		}
		if runner.backupCount != 1 {
			t.Fatalf("successful backup passes = %d, want exactly 1", runner.backupCount)
		}
		if version, _ := f.agent.secretVersion(hermesPasswordSecret); version != 1 {
			t.Fatalf("password rotated across retry: version %d", version)
		}
		if got := countRuns(t, f.db, runtimedb.BackupRunBackup, runtimedb.BackupRunSucceeded); got != 1 {
			t.Fatalf("successful backup ledger rows = %d, want 1", got)
		}
		assertHermesData(t, f.agent, hermesDataVolume, hermesDataPath)
	})

	t.Run("restart after first backup retries check without duplicate backup", func(t *testing.T) {
		f := newHermesFixture(t, false)
		runner := &hermesBackupRunner{agent: f.agent, failOnce: map[string]bool{"check": true}}
		first := f.engine(runner)
		if _, err := first.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if runner.backupCount != 1 || !runner.repository || len(runner.snapshot) == 0 {
			t.Fatalf("first backup did not survive check cancellation: backups=%d repository=%v snapshot=%d", runner.backupCount, runner.repository, len(runner.snapshot))
		}
		restarted := f.engine(runner)
		restarted.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
		if _, err := restarted.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if runner.backupCount != 1 {
			t.Fatalf("restart duplicated the successful backup: %d passes", runner.backupCount)
		}
		if got := countRuns(t, f.db, runtimedb.BackupRunBackup, runtimedb.BackupRunSucceeded); got != 1 {
			t.Fatalf("successful backup ledger rows after restart = %d, want 1", got)
		}
		if got := countRuns(t, f.db, runtimedb.BackupRunCheck, runtimedb.BackupRunSucceeded); got != 1 {
			t.Fatalf("successful check rows after restart = %d, want 1", got)
		}
		if version, _ := f.agent.secretVersion(hermesPasswordSecret); version != 1 {
			t.Fatalf("password rotated after successful backup: version %d", version)
		}
		assertHermesData(t, f.agent, hermesDataVolume, hermesDataPath)
	})

	t.Run("suppressed generation is held before Restic", func(t *testing.T) {
		f := newHermesFixture(t, true)
		runner := &hermesBackupRunner{agent: f.agent, failOnce: map[string]bool{}}
		reports, err := f.engine(runner).Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(reports) != 3 {
			t.Fatalf("held first tick reports = %+v", reports)
		}
		for _, report := range reports {
			if !strings.Contains(report.Err, "dependency unavailable") {
				t.Fatalf("held report = %+v", report)
			}
		}
		if len(runner.commands) != 0 {
			t.Fatalf("missing password launched Restic commands: %v", runner.commands)
		}
		if got := countRuns(t, f.db, runtimedb.BackupRunBackup, runtimedb.BackupRunDependencyUnavailable); got != 1 {
			t.Fatalf("dependency ledger rows = %d, want one durable held result", got)
		}
	})
}
