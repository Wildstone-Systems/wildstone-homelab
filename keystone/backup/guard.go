package backup

// Disk-space guard and NAS mount safety (ADR-W045 A3.1, A3.2).
//
// A local repository must never fill the disk it lives on, and a backup
// of a NAS folder must never run while the share is not mounted — that
// would back up the empty directory beneath it as "nothing changed", and
// retention would eventually prune the real snapshots away. Both are
// checked through the agent's read-only DiskSpace and MeasureUsage ops
// (protocol minor 19):
//
//  1. at proposal time (Preflight): a plan whose local copy cannot fit
//     is refused, with the two ways out;
//  2. before every local run: if the reserve would be crossed, the local
//     copy is skipped — and a plan with an off-site copy backs up
//     straight to it for this run, so the data stays protected;
//  3. during the local backup pass: the runner re-checks free space as it
//     polls, and stops restic at the reserve.
//
// An agent too old to answer DiskSpace cannot be checked. A plan that
// only uses volumes then runs as before, with a warning in the log; a
// plan with a NAS path does not run at all, because "cannot tell whether
// the share is mounted" is exactly the case the check exists for.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// Applier dispatches one op to a node: reconcile.Applier's shape, restated
// so tests can fake it without a gateway.
type Applier interface {
	Apply(ctx context.Context, nodeID string, op *wsv1.Operation) (*wsv1.OpResult, error)
}

// errCannotCheck: the node's agent predates DiskSpace (protocol minor 19).
var errCannotCheck = errors.New("the node agent cannot report disk space (needs protocol minor 19; upgrade wardstoned)")

// errNotThere: the path or volume does not exist on the node.
var errNotThere = errors.New("not found on the node")

// LowDiskError is a local backup skipped because it would cross its
// disk's reserve.
type LowDiskError struct{ Detail string }

func (e *LowDiskError) Error() string { return "skipped the local copy: " + e.Detail }

const (
	gib = uint64(1) << 30
	// defaultReserveFloor is the absolute part of the default reserve.
	defaultReserveFloor = 20 * gib
	// estimateMargin is how much larger than its sources a repository is
	// assumed to grow, for the older versions retention keeps.
	estimateMargin = 1.2
	// guardEvery30 is how often a running local backup re-checks free
	// space, unless ApplierRunner.GuardEvery says otherwise.
	guardEvery30 = 30 * time.Second
	// preflightTimeout bounds the proposal-time check. Past it the
	// proposal goes ahead unchecked; the run-time checks still hold.
	preflightTimeout = 30 * time.Second
)

type diskInfo struct {
	total, free uint64
	mountPoint  string
	fstype      string
	exists      bool
}

// location is where a repository or source lives on a node: a volume
// name, or an absolute path.
func location(nameOrPath string) (path, volume string) {
	if strings.HasPrefix(nameOrPath, "/") {
		return nameOrPath, ""
	}
	return "", nameOrPath
}

func (e *Engine) diskSpace(ctx context.Context, node, nameOrPath string) (diskInfo, error) {
	if e.Agent == nil {
		return diskInfo{}, errCannotCheck
	}
	path, volume := location(nameOrPath)
	res, err := e.Agent.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_DiskSpace{
		DiskSpace: &wsv1.DiskSpace{Path: path, Volume: volume},
	}})
	if err != nil {
		return diskInfo{}, err
	}
	if oe := res.GetError(); oe != nil {
		switch oe.GetCode() {
		case wsv1.ErrorCode_ERROR_CODE_INVALID:
			return diskInfo{}, errCannotCheck
		case wsv1.ErrorCode_ERROR_CODE_NOT_FOUND:
			return diskInfo{}, errNotThere
		}
		return diskInfo{}, fmt.Errorf("disk space of %s: %s: %s", nameOrPath, oe.GetCode(), oe.GetMessage())
	}
	d := res.GetDiskSpace()
	return diskInfo{total: d.GetTotalBytes(), free: d.GetFreeBytes(), mountPoint: d.GetMountPoint(), fstype: d.GetFstype(), exists: d.GetExists()}, nil
}

func (e *Engine) usage(ctx context.Context, node, nameOrPath string) (uint64, error) {
	if e.Agent == nil {
		return 0, errCannotCheck
	}
	path, volume := location(nameOrPath)
	res, err := e.Agent.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_MeasureUsage{
		MeasureUsage: &wsv1.MeasureUsage{Path: path, Volume: volume},
	}})
	if err != nil {
		return 0, err
	}
	if oe := res.GetError(); oe != nil {
		switch oe.GetCode() {
		case wsv1.ErrorCode_ERROR_CODE_INVALID:
			return 0, errCannotCheck
		case wsv1.ErrorCode_ERROR_CODE_NOT_FOUND:
			return 0, errNotThere
		}
		return 0, fmt.Errorf("measure %s: %s: %s", nameOrPath, oe.GetCode(), oe.GetMessage())
	}
	return res.GetMeasureUsage().GetTotalBytes(), nil
}

// reserveFor is the free space a repository's disk must keep: the plan's
// own reserve, or the larger of 10% and 20 GiB — capped at a quarter of
// a small disk, so a 40 GiB disk is not declared full before it starts.
func reserveFor(spec *resource.BackupPlanSpec, total uint64) uint64 {
	if spec.Reserve != "" {
		if b, pct, err := resource.ParseReserve(spec.Reserve); err == nil {
			if b > 0 {
				return b
			}
			return uint64(float64(total) * pct / 100)
		}
	}
	floor := defaultReserveFloor
	if q := total / 4; q < floor {
		floor = q
	}
	if tenth := total / 10; tenth > floor {
		return tenth
	}
	return floor
}

func humanBytes(b uint64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1f TiB", float64(b)/(1<<40))
	case b >= gib:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(gib))
	default:
		return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20))
	}
}

// sources is every source a plan backs up: its volumes, then its paths.
func sources(spec *resource.BackupPlanSpec) []string {
	return append(append([]string{}, spec.Volumes...), spec.Paths...)
}

// checkMounts refuses a run whose NAS path is not on its share: every
// source or repository path under a declared NetworkMount must sit on a
// filesystem mounted at that share's target (or deeper), and every
// source path must exist.
func (e *Engine) checkMounts(ctx context.Context, r resource.Resource, desired []resource.Resource, withRepo bool) error {
	spec := r.BackupPlan
	check := func(p string, mustExist bool) error {
		share := shareFor(desired, r.Metadata.Node, p)
		if share == nil && !mustExist {
			return nil
		}
		d, err := e.diskSpace(ctx, r.Metadata.Node, p)
		switch {
		case errors.Is(err, errCannotCheck):
			if share != nil {
				return fmt.Errorf("cannot confirm NAS share %s is mounted at %s: %w", share.Source, share.Target, err)
			}
			return nil // a plain host folder on an old agent: restic will say if it is missing
		case err != nil:
			return fmt.Errorf("checking %s: %w", p, err)
		}
		if share != nil && d.mountPoint != share.Target && !strings.HasPrefix(d.mountPoint, share.Target+"/") {
			return fmt.Errorf("NAS share %s is not mounted at %s (%s is on %s); refusing to back up the empty folder underneath",
				share.Source, share.Target, p, d.mountPoint)
		}
		if mustExist && !d.exists {
			return fmt.Errorf("source folder %s does not exist", p)
		}
		return nil
	}
	for _, p := range spec.Paths {
		if err := check(p, true); err != nil {
			return err
		}
	}
	if withRepo && strings.HasPrefix(spec.Repository, "/") {
		if err := check(spec.Repository, false); err != nil {
			return err
		}
	}
	return nil
}

// shareFor returns the NetworkMount on node whose target holds p, if any.
func shareFor(desired []resource.Resource, node, p string) *resource.NetworkMountSpec {
	var best *resource.NetworkMountSpec
	for _, d := range desired {
		if d.Kind != resource.KindNetworkMount || d.NetworkMount == nil || d.Metadata.Node != node {
			continue
		}
		t := d.NetworkMount.Target
		if (p == t || strings.HasPrefix(p, t+"/")) && (best == nil || len(t) > len(best.Target)) {
			best = d.NetworkMount
		}
	}
	return best
}

// spaceNeeded estimates what the local repository still has to grow by:
// its sources with the retention margin, less what the repository
// already holds. ok is false when a size could not be measured, which
// leaves only the reserve itself to check.
func (e *Engine) spaceNeeded(ctx context.Context, r resource.Resource, repoExists bool) (need uint64, ok bool) {
	var src uint64
	for _, s := range sources(r.BackupPlan) {
		n, err := e.usage(ctx, r.Metadata.Node, s)
		if errors.Is(err, errNotThere) {
			continue // a volume not created yet holds nothing
		}
		if err != nil {
			e.log().Warn("backup: could not size a source; checking the reserve only", "plan", r.Metadata.Name, "source", s, "err", err)
			return 0, false
		}
		src += n
	}
	est := uint64(float64(src) * estimateMargin)
	var have uint64
	if repoExists {
		n, err := e.usage(ctx, r.Metadata.Node, r.BackupPlan.Repository)
		if err != nil && !errors.Is(err, errNotThere) {
			e.log().Warn("backup: could not size the repository; checking the reserve only", "plan", r.Metadata.Name, "err", err)
			return 0, false
		}
		have = n
	}
	if est <= have {
		return 0, true
	}
	return est - have, true
}

// checkSpace decides whether a local backup of r fits: free space at the
// repository must stay above the reserve after the estimated growth. It
// returns the in-run guard for the backup pass, or a *LowDiskError when
// the copy does not fit. The guard is nil when the node cannot be asked.
func (e *Engine) checkSpace(ctx context.Context, r resource.Resource) (guard func(context.Context) error, err error) {
	spec, node := r.BackupPlan, r.Metadata.Node
	if e.Agent == nil {
		return nil, nil // not wired (a test engine): nothing to ask
	}
	d, err := e.diskSpace(ctx, node, spec.Repository)
	switch {
	case errors.Is(err, errCannotCheck):
		e.log().Warn("backup: running without the disk-space guard", "plan", r.Metadata.Name, "why", err.Error())
		return nil, nil
	case errors.Is(err, errNotThere):
		// A repository volume not created yet: nothing to measure until
		// reconcile creates it. The runner creates nothing larger.
		return nil, nil
	case err != nil:
		return nil, err
	}
	reserve := reserveFor(spec, d.total)
	need, measured := e.spaceNeeded(ctx, r, d.exists)
	if d.free < reserve+need {
		detail := fmt.Sprintf("%s has %s free and must keep a %s reserve", spec.Repository, humanBytes(d.free), humanBytes(reserve))
		if measured && need > 0 {
			detail = fmt.Sprintf("about %s more is needed, but %s", humanBytes(need), detail)
		}
		return nil, &LowDiskError{Detail: detail}
	}
	return e.inRunGuard(node, spec.Repository, reserve), nil
}

// inRunGuard is checked by the runner while the backup pass runs.
func (e *Engine) inRunGuard(node, repo string, reserve uint64) func(context.Context) error {
	return func(ctx context.Context) error {
		d, err := e.diskSpace(ctx, node, repo)
		if err != nil {
			return nil // a failed check is not a full disk; the next one may answer
		}
		if d.free < reserve {
			return &LowDiskError{Detail: fmt.Sprintf("stopped the backup: %s reached its %s reserve (%s free)", repo, humanBytes(reserve), humanBytes(d.free))}
		}
		return nil
	}
}

// Preflight checks proposed BackupPlans before they become plans
// (ADR-W045 A3.1 checkpoint 1). Only a plan that can be measured AND
// would not fit is refused; anything that cannot be measured yet — a
// volume not created, an old agent, a check past preflightTimeout —
// goes ahead, and the run-time checks still hold.
func (e *Engine) Preflight(puts []resource.Resource) error {
	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	defer cancel()
	var refusals []string
	for _, r := range puts {
		if r.Kind != resource.KindBackupPlan || r.BackupPlan == nil || !hasLocal(r.BackupPlan) {
			continue
		}
		if _, err := e.checkSpace(ctx, r); err != nil {
			var low *LowDiskError
			if errors.As(err, &low) {
				refusals = append(refusals, fmt.Sprintf("BackupPlan %s: its local copy will not fit (%s). Use route offsite-only, or put the repository on a NAS path, or free space",
					r.Metadata.Name, low.Detail))
			}
		}
	}
	if len(refusals) > 0 {
		return errors.New(strings.Join(refusals, "; "))
	}
	return nil
}

type guardKey struct{}

// withGuard attaches a check the runner calls while the container runs.
func withGuard(ctx context.Context, g func(context.Context) error) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, guardKey{}, g)
}

func guardFrom(ctx context.Context) func(context.Context) error {
	g, _ := ctx.Value(guardKey{}).(func(context.Context) error)
	return g
}
