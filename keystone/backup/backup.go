// Package backup is the E6 backup engine (ADR-W011 A4). It acts on the
// BackupPlan declarations in the Statefile: on its own timer duty (not
// cron on any host) it runs restic as an owner-labeled one-shot CONTAINER
// with declared mounts — never a host binary — snapshots the plan's
// volumes into the plan's repository volume, applies the retention
// policy, and runs the monthly restore drill. Every run and every drill
// is recorded three ways: a plans-table record driven through the normal
// state machine by the system/backup principal, events on the operational
// stream, and a row in the backup_runs ledger the scheduler reads.
//
// Approval model: committing a BackupPlan through an approved plan IS the
// owner's standing approval for its runs and its restore drills — both
// are restore-alongside-safe (sources mounted read-only; drills restore
// into a scratch volume, never over live data). An IN-PLACE restore is
// deliberately not implemented here: per ADR-W011 A4 it only ever happens
// via an explicit High-risk plan a human approves, which is a later work
// package. RestoreAlongside is the implemented recovery path: latest
// snapshot into a fresh "<plan>-restore" volume, from which the owner (or
// a future guided flow) moves data deliberately.
//
// The engine never parses restic output. restic's own exit code is the
// verdict (InspectContainerResult.exit_code, protocol minor 11): a pass
// succeeds only on exit 0, and the ledger detail names what a non-zero
// code means (see resticExit). Exit codes 10 and 12 need restic 0.17 or
// later, which the pinned image is.
package backup

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"context"
	"encoding/json"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

// Principal marks every plan record the backup engine drives. The
// reconcile loop skips records with this principal (see loop.Tick): they
// are container runs the engine executes itself, not Statefile intents.
const Principal = "system/backup"

// Runner runs one one-shot container to completion on a node. The
// production implementation is ApplierRunner (EnsureContainer, poll until
// exited, RemoveContainer); tests substitute a fake. A nil error means
// the container ran and finished; the run's effect on the repository is
// taken on that basis (see the package doc on exit codes).
type Runner interface {
	RunContainer(ctx context.Context, node, name string, spec resource.ContainerSpec) error
}

// Engine is the backup service. All fields except Log are required.
type Engine struct {
	State *statefile.Store
	DB    *runtimedb.DB
	Run   Runner
	// Agent answers the read-only DiskSpace and MeasureUsage ops the
	// disk-space guard and NAS mount checks use (see guard.go). nil runs
	// without them, as against an agent too old to answer.
	Agent Applier
	// ResticImage is the digest-pinned restic image every run uses —
	// engine-level configuration, not per-plan, so one upgrade covers the
	// fleet and a plan cannot smuggle in an arbitrary image.
	ResticImage string
	// Now is the clock seam for tests. nil = time.Now.
	Now func() time.Time
	// DrillEvery is the restore-drill cadence. <= 0 uses 30 days
	// (ADR-W011 A4: "monthly restore-drill").
	DrillEvery time.Duration
	// RetryAfter is how long a failed scheduled run waits before the
	// schedule tries it again. <= 0 uses 1 hour. Without it a plan that
	// fails every time (a wrong password, a full disk) would re-run on
	// every tick, all day. RunNow and RestoreAlongside ignore it.
	RetryAfter time.Duration
	// OffsiteTimeout bounds one off-site run or restore. <= 0 uses 72
	// hours: a first upload of a large library over a home connection
	// can take days, and restic resumes from what it already uploaded.
	OffsiteTimeout time.Duration
	Log            *slog.Logger

	// running holds one entry per plan with a run in progress, so a
	// scheduled run and an on-demand one never share a container name.
	running sync.Map
	// workers holds one entry per plan whose scheduled work is in
	// flight, so a slow plan (a days-long first upload) is not started
	// twice and never holds up the others.
	workers sync.Map
}

// resticExit describes restic's documented exit codes (restic 0.17+) for
// the ledger. Anything unlisted is reported by number alone.
var resticExit = map[int32]string{
	1:   "fatal error",
	3:   "snapshot saved but incomplete: some source files could not be read",
	10:  "repository does not exist",
	11:  "repository is locked by another process",
	12:  "wrong repository password",
	130: "interrupted",
}

// resticExitNoRepo is the one exit code that makes an init correct.
const resticExitNoRepo = 10

// describe turns a pass error into ledger text: which pass, and what
// restic's exit code means when it gave one.
func describe(pass string, err error) error {
	if code, ok := exitCode(err); ok {
		if meaning, known := resticExit[code]; known {
			return fmt.Errorf("%s: restic exited %d (%s)", pass, code, meaning)
		}
		return fmt.Errorf("%s: restic exited %d", pass, code)
	}
	return fmt.Errorf("%s: %w", pass, err)
}

// scheduleIntervals maps BackupPlanSpec.Schedule to a due interval.
var scheduleIntervals = map[string]time.Duration{
	"daily":   24 * time.Hour,
	"weekly":  7 * 24 * time.Hour,
	"monthly": 30 * 24 * time.Hour,
}

// RunReport describes one executed run for Tick's outcome.
type RunReport struct {
	Plan string
	Kind runtimedb.BackupRunKind
	Err  string // "" = succeeded
}

// Tick evaluates every BackupPlan in the Statefile once and waits for
// the work it starts: any plan whose last successful backup, drill or
// off-site run is older than its cadence runs now. Plans run in
// parallel, one worker each; within a plan the order is backup, drill,
// off-site, so an off-site copy carries the snapshot just taken.
// Failures are reported per plan and never stop the others; a failed run
// leaves no successful ledger row, so it is retried once RetryAfter has
// passed rather than waiting out a full interval.
func (e *Engine) Tick(ctx context.Context) ([]RunReport, error) {
	var mu sync.Mutex
	var reports []RunReport
	wg, err := e.start(ctx, func(reps []RunReport) {
		mu.Lock()
		reports = append(reports, reps...)
		mu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	wg.Wait()
	sort.SliceStable(reports, func(i, j int) bool { return reports[i].Plan < reports[j].Plan })
	return reports, nil
}

// start launches one worker per BackupPlan that has none running and
// returns without waiting; each worker hands its reports to done.
func (e *Engine) start(ctx context.Context, done func([]RunReport)) (*sync.WaitGroup, error) {
	desired, err := e.State.Load()
	if err != nil {
		return nil, fmt.Errorf("backup: loading statefile: %w", err)
	}
	var wg sync.WaitGroup
	for _, r := range desired {
		if r.Kind != resource.KindBackupPlan || r.BackupPlan == nil {
			continue
		}
		if _, busy := e.workers.LoadOrStore(r.Metadata.Name, struct{}{}); busy {
			continue
		}
		wg.Add(1)
		go func(r resource.Resource) {
			defer wg.Done()
			defer e.workers.Delete(r.Metadata.Name)
			done(e.planWork(ctx, r, desired))
		}(r)
	}
	return &wg, nil
}

// planWork runs whatever of one plan's duties is due, in order.
func (e *Engine) planWork(ctx context.Context, r resource.Resource, desired []resource.Resource) []RunReport {
	spec, name := r.BackupPlan, r.Metadata.Name
	var reports []RunReport
	direct := false
	if hasLocal(spec) {
		if e.due(name, runtimedb.BackupRunBackup, scheduleIntervals[spec.Schedule]) {
			err := e.runBackup(ctx, r, desired)
			reports = append(reports, report(name, runtimedb.BackupRunBackup, err))
			// No room for the local copy: a plan with an off-site copy
			// backs up straight to it this time, so the data is still
			// protected while the owner frees space (ADR-W045 A3.1).
			var low *LowDiskError
			direct = errors.As(err, &low) && spec.Offsite != nil
		}
		if e.due(name, runtimedb.BackupRunDrill, e.drillEvery()) {
			reports = append(reports, report(name, runtimedb.BackupRunDrill, e.runDrill(ctx, r)))
		}
	}
	if o := spec.Offsite; o != nil && (direct || e.due(name, runtimedb.BackupRunOffsite, scheduleIntervals[o.Schedule])) {
		reports = append(reports, report(name, runtimedb.BackupRunOffsite, e.runOffsite(ctx, r, desired, direct)))
	}
	return reports
}

func report(plan string, kind runtimedb.BackupRunKind, err error) RunReport {
	rep := RunReport{Plan: plan, Kind: kind}
	if err != nil {
		rep.Err = err.Error()
	}
	return rep
}

func (e *Engine) drillEvery() time.Duration {
	if e.DrillEvery > 0 {
		return e.DrillEvery
	}
	return 30 * 24 * time.Hour
}

func (e *Engine) retryAfter() time.Duration {
	if e.RetryAfter > 0 {
		return e.RetryAfter
	}
	return time.Hour
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// due reports whether (plan, kind) has no successful ledger row within
// interval and no failed one within RetryAfter. An unknown schedule value
// maps to interval 0 and is never due — resource validation makes that
// unreachable for committed plans.
func (e *Engine) due(plan string, kind runtimedb.BackupRunKind, interval time.Duration) bool {
	if interval <= 0 {
		return false
	}
	if recent, ok := e.within(plan, kind, runtimedb.BackupRunFailed, e.retryAfter()); !ok || recent {
		return false
	}
	recent, ok := e.within(plan, kind, runtimedb.BackupRunSucceeded, interval)
	return ok && !recent
}

// within reports whether (plan, kind) has a ledger row with status that
// started less than d ago. ok is false when the ledger could not be read:
// fail safe, don't run on a broken ledger read.
func (e *Engine) within(plan string, kind runtimedb.BackupRunKind, status runtimedb.BackupRunStatus, d time.Duration) (recent, ok bool) {
	last, err := e.DB.LastBackupRun(plan, kind, status)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return false, true
	}
	if err != nil {
		e.log().Error("backup: reading run ledger", "plan", plan, "err", err)
		return false, false
	}
	return e.now().Sub(last.StartedAt) < d, true
}

// runBackup executes one backup run: the NAS mount and disk-space checks
// (guard.go), a probe pass (`restic cat config`, read-only) that tells an
// existing repository from a missing one, an init pass only when it is
// missing, a backup pass watched by the disk-space guard, and — when the
// plan declares any retention — a forget --prune pass. Every pass must
// exit 0. A run with no room returns a *LowDiskError.
func (e *Engine) runBackup(ctx context.Context, r resource.Resource, desired []resource.Resource) error {
	spec := r.BackupPlan
	name := r.Metadata.Name
	return e.withProvenance(ctx, r, runtimedb.BackupRunBackup,
		fmt.Sprintf("backup run: %s (%d source(s) -> %s)", name, len(sources(spec)), spec.Repository),
		func(ctx context.Context) error {
			if err := e.checkMounts(ctx, r, desired, true); err != nil {
				return err
			}
			guard, err := e.checkSpace(ctx, r)
			if err != nil {
				return err
			}
			if err := e.ensureRepository(ctx, r); err != nil {
				return err
			}
			backupSpec := e.resticSpec(spec, append([]string{"backup"}, sourceTargets(spec)...), sources(spec))
			if err := e.Run.RunContainer(withGuard(ctx, guard), r.Metadata.Node, "backup-"+name, backupSpec); err != nil {
				var low *LowDiskError
				if errors.As(err, &low) {
					return err
				}
				return describe("backup pass", err)
			}
			forgetArgs := retentionArgs(spec.Retention)
			if forgetArgs == nil {
				return nil
			}
			forgetSpec := e.resticSpec(spec, append([]string{"forget", "--prune"}, forgetArgs...), nil)
			if err := e.Run.RunContainer(ctx, r.Metadata.Node, "backup-"+name+"-forget", forgetSpec); err != nil {
				return describe("forget pass", err)
			}
			return nil
		})
}

// ensureRepository initializes the plan's repository only when restic
// says it does not exist (exit 10). Any other probe failure stops the run:
// a wrong password (exit 12) must never be "fixed" by an init, and an
// init that fails is a real failure, not the steady state.
func (e *Engine) ensureRepository(ctx context.Context, r resource.Resource) error {
	spec := r.BackupPlan
	name := r.Metadata.Name
	probe := e.resticSpec(spec, []string{"cat", "config"}, nil)
	err := e.Run.RunContainer(ctx, r.Metadata.Node, "backup-"+name+"-probe", probe)
	if err == nil {
		return nil
	}
	if code, ok := exitCode(err); !ok || code != resticExitNoRepo {
		return describe("repository check", err)
	}
	initSpec := e.resticSpec(spec, []string{"init"}, nil)
	if err := e.Run.RunContainer(ctx, r.Metadata.Node, "backup-"+name+"-init", initSpec); err != nil {
		return describe("repository init", err)
	}
	e.log().Info("backup: initialized a new repository", "plan", name, "repository", spec.Repository)
	return nil
}

// runDrill executes the restore drill: latest snapshot restored alongside
// into the scratch volume "<plan>-drill" — live data is never touched.
func (e *Engine) runDrill(ctx context.Context, r resource.Resource) error {
	name := r.Metadata.Name
	return e.withProvenance(ctx, r, runtimedb.BackupRunDrill,
		fmt.Sprintf("restore drill: %s (latest -> volume %s-drill)", name, name),
		func(ctx context.Context) error {
			return e.restoreInto(ctx, r, name+"-drill", "drill-"+name)
		})
}

// RunNow executes planName's backup immediately, schedule
// notwithstanding, with the same provenance as a scheduled run — the
// on-demand path behind POST /api/v1/backups/{plan}/run (ADR-W015 A2).
// A plan with an off-site copy runs that too, after the local backup
// (or instead of it, for offsite-only). A successful RunNow also resets
// the schedule clock: the ledger row it writes is the "last success" the
// next due() check reads.
//
// The caller going away does not stop the run: a closed browser tab
// must not kill a backup halfway. It finishes, and the ledger says how.
func (e *Engine) RunNow(ctx context.Context, planName string) error {
	ctx = context.WithoutCancel(ctx)
	r, desired, err := e.findPlan(planName)
	if err != nil {
		return err
	}
	if hasLocal(r.BackupPlan) {
		err := e.runBackup(ctx, r, desired)
		var low *LowDiskError
		if errors.As(err, &low) && r.BackupPlan.Offsite != nil {
			if oerr := e.runOffsite(ctx, r, desired, true); oerr != nil {
				return fmt.Errorf("%v; the off-site backup instead also failed: %w", err, oerr)
			}
			return fmt.Errorf("%v; backed up straight off-site instead", err)
		}
		if err != nil {
			return err
		}
	}
	if r.BackupPlan.Offsite != nil {
		return e.runOffsite(ctx, r, desired, false)
	}
	return nil
}

// RestoreAlongside restores planName's latest snapshot into the fresh
// volume "<plan>-restore" — the implemented recovery path (ADR-W011 A4:
// restore-alongside first). It runs immediately with full provenance; the
// owner moves data from the restore volume deliberately.
//
// It records runKind "restore", not "drill": mechanically this is the
// drill, but the ledger answers "what happened here", and an operator
// restoring during an incident is not running a rehearsal. Keeping the
// kinds distinct also means a manual restore no longer resets the
// scheduled drill's clock — due() reads the last successful *drill*.
//
// An offsite-only plan has no local copy, so its restore comes from the
// off-site repository (see RestoreOffsite).
func (e *Engine) RestoreAlongside(ctx context.Context, planName string) error {
	ctx = context.WithoutCancel(ctx)
	r, desired, err := e.findPlan(planName)
	if err != nil {
		return err
	}
	if !hasLocal(r.BackupPlan) {
		return e.restoreOffsite(ctx, r, desired)
	}
	return e.withProvenance(ctx, r, runtimedb.BackupRunRestore,
		fmt.Sprintf("restore alongside: %s (latest -> volume %s-restore)", planName, planName),
		func(ctx context.Context) error {
			return e.restoreInto(ctx, r, planName+"-restore", "restore-"+planName)
		})
}

// findPlan returns the named BackupPlan and the desired state it came
// from, which an off-site run needs to find its BackupTarget.
func (e *Engine) findPlan(planName string) (resource.Resource, []resource.Resource, error) {
	desired, err := e.State.Load()
	if err != nil {
		return resource.Resource{}, nil, fmt.Errorf("backup: loading statefile: %w", err)
	}
	for _, r := range desired {
		if r.Kind == resource.KindBackupPlan && r.Metadata.Name == planName && r.BackupPlan != nil {
			return r, desired, nil
		}
	}
	return resource.Resource{}, nil, fmt.Errorf("backup: no BackupPlan %q in the Statefile", planName)
}

func (e *Engine) restoreInto(ctx context.Context, r resource.Resource, targetVolume, containerName string) error {
	spec := *r.BackupPlan
	cs := e.resticSpec(&spec, []string{"restore", "latest", "--target", "/restore"}, nil)
	cs.Mounts = append(cs.Mounts, resource.Mount{Source: targetVolume, Target: "/restore"})
	if err := e.Run.RunContainer(ctx, r.Metadata.Node, containerName, cs); err != nil {
		return describe("restore pass", err)
	}
	return nil
}

// withProvenance wraps one run in its full record set: a plans-table
// record (proposed -> approved -> applying by the system/backup principal
// — the standing approval carried by the committed BackupPlan itself),
// the backup_runs ledger row, events, and the terminal transition.
func (e *Engine) withProvenance(ctx context.Context, r resource.Resource, kind runtimedb.BackupRunKind, summary string, fn func(context.Context) error) error {
	if _, busy := e.running.LoadOrStore(r.Metadata.Name, struct{}{}); busy {
		return fmt.Errorf("backup: plan %q already has a run in progress", r.Metadata.Name)
	}
	defer e.running.Delete(r.Metadata.Name)

	stored, err := json.Marshal(map[string]any{"summary": summary, "backupPlan": r.Metadata.Name, "kind": string(kind)})
	if err != nil {
		return err
	}
	planID, err := e.DB.InsertPlan(Principal, "", summary, "low", string(stored))
	if err != nil {
		return fmt.Errorf("backup: recording plan: %w", err)
	}
	for _, st := range []runtimedb.PlanState{runtimedb.PlanApproved, runtimedb.PlanApplying} {
		if err := e.DB.TransitionPlan(planID, st, Principal); err != nil {
			return fmt.Errorf("backup: plan %s -> %s: %w", planID, st, err)
		}
	}
	seq, err := e.DB.StartBackupRun(r.Metadata.Name, kind, planID)
	if err != nil {
		return fmt.Errorf("backup: opening run ledger: %w", err)
	}
	e.event("backup.started", r.Metadata.Node, summary, map[string]string{"plan": planID, "backupPlan": r.Metadata.Name, "kind": string(kind)})

	runErr := fn(ctx)

	status, planState, evt := runtimedb.BackupRunSucceeded, runtimedb.PlanApplied, "backup.completed"
	detail := ""
	if runErr != nil {
		status, planState, evt = runtimedb.BackupRunFailed, runtimedb.PlanFailed, "backup.failed"
		detail = runErr.Error()
	}
	if err := e.DB.FinishBackupRun(seq, status, detail); err != nil {
		e.log().Error("backup: closing run ledger", "seq", seq, "err", err)
	}
	if err := e.DB.TransitionPlan(planID, planState, Principal); err != nil {
		e.log().Error("backup: terminal plan transition", "plan", planID, "err", err)
	}
	e.event(evt, r.Metadata.Node, summary, map[string]string{"plan": planID, "backupPlan": r.Metadata.Name, "kind": string(kind), "detail": detail})
	return runErr
}

// resticSpec composes the one-shot restic container for one pass: every
// source volume read-only under /backup/<vol>, the repository volume
// read-write under /repo, and the repository password as a secret
// REFERENCE only (Wardstone materializes it, E8 — the cloudflared
// TUNNEL_TOKEN posture). withSources nil mounts no sources (init/forget/
// restore passes don't need them).
func (e *Engine) resticSpec(spec *resource.BackupPlanSpec, command []string, withSources []string) resource.ContainerSpec {
	mounts := []resource.Mount{{Source: spec.Repository, Target: "/repo"}}
	for _, v := range withSources {
		mounts = append(mounts, resource.Mount{Source: v, Target: sourceTarget(v), ReadOnly: true})
	}
	return resource.ContainerSpec{
		Image: e.ResticImage,
		Env: map[string]string{
			"RESTIC_REPOSITORY": "/repo",
			"RESTIC_PASSWORD":   spec.PasswordSecret,
		},
		Mounts:  mounts,
		Command: command,
		Restart: "no",
	}
}

// sourceTarget is where a source appears inside the run container: a
// volume at /backup/<name>, a node path at /backup<path> — so a snapshot
// of /srv/wildstone/nas/movies restores to backup/srv/wildstone/nas/movies,
// the same path it came from.
func sourceTarget(src string) string {
	if strings.HasPrefix(src, "/") {
		return "/backup" + src
	}
	return "/backup/" + src
}

// sourceTargets returns the in-container backup paths for the plan's
// sources, sorted so the restic command line — and with it the container
// spec hash — is deterministic for a given source set.
func sourceTargets(spec *resource.BackupPlanSpec) []string {
	srcs := sources(spec)
	out := make([]string, 0, len(srcs))
	for _, v := range srcs {
		out = append(out, sourceTarget(v))
	}
	sort.Strings(out)
	return out
}

// retentionArgs maps a BackupRetention to restic forget flags; nil when
// every keep count is zero (no forget pass runs at all).
func retentionArgs(ret resource.BackupRetention) []string {
	var args []string
	if ret.KeepDaily > 0 {
		args = append(args, "--keep-daily", strconv.Itoa(ret.KeepDaily))
	}
	if ret.KeepWeekly > 0 {
		args = append(args, "--keep-weekly", strconv.Itoa(ret.KeepWeekly))
	}
	if ret.KeepMonthly > 0 {
		args = append(args, "--keep-monthly", strconv.Itoa(ret.KeepMonthly))
	}
	return args
}

func (e *Engine) logReports(reports []RunReport) {
	for _, rep := range reports {
		if rep.Err != "" {
			e.log().Warn("backup run failed", "plan", rep.Plan, "kind", string(rep.Kind), "err", rep.Err)
		} else {
			e.log().Info("backup run completed", "plan", rep.Plan, "kind", string(rep.Kind))
		}
	}
}

func (e *Engine) event(typ, node, msg string, attrs map[string]string) {
	if _, err := e.DB.AppendEvent(runtimedb.Event{Type: typ, Node: node, Message: msg, Attrs: attrs}); err != nil {
		e.log().Error("backup: event append failed", "type", typ, "err", err)
	}
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// RunLoop ticks the engine every interval until ctx is done — the timer
// duty ADR-W011 A4 places in Keystone instead of cron on a host.
func (e *Engine) RunLoop(ctx context.Context, interval time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		// Not Tick: the loop must not wait for a slow plan. A plan still
		// working from an earlier tick is skipped, not started again.
		if _, err := e.start(ctx, e.logReports); err != nil {
			e.log().Error("backup tick", "err", err)
		}
	}
}
