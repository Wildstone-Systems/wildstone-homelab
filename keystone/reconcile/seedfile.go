package reconcile

// SeedFile applier: place a package-rendered file into the application's
// OWN data volume, once, before the application first starts.
//
// The contrast with configfile.go is the whole design. A ConfigFile is
// Wildstone's file: written under the managed prefix, read back, compared,
// reverted on drift, cleared on delete. A SeedFile is the app's file: the
// node writes it only if absent, and after that Wildstone neither reads
// its content, nor rewrites it, nor removes it.
//
// That asymmetry is what makes it safe to point at user data, and it is
// also why observation is existence-only: comparing content would report
// drift the moment a household edited their own configuration, and
// "converged" would come to mean "nobody has touched their config".

import (
	"context"
	"fmt"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// applySeedFileStep writes the seed if it is not already there.
//
// Every action reports success, and each for a different reason:
//
//   - Create: the node writes if absent. written=false is the expected
//     steady state on every tick after the first, not a failure.
//   - Update: a package shipped different seed content. The file belongs
//     to the app by now, so it is NOT rewritten. Reporting failure would
//     set the resource aside forever (F60) over a difference we have
//     deliberately chosen not to act on.
//   - Delete: never happens in practice — observeSeedFile reports an
//     undesired seed as absent, so no Delete step is ever diffed — and if
//     one arrived it would still be a no-op, because a seeded file is
//     household data and ADR-W004 A4 does not delete that.
func (r *Reconciler) applySeedFileStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource) StepResult {
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}

	if step.Action == plan.ActionDelete {
		sr.Applied, sr.HealthOK = true, true
		return sr
	}

	want, ok := desiredByKey[key]
	if !ok || want.SeedFile == nil {
		sr.Error = "internal: no desired SeedFile spec for " + step.Name
		return sr
	}

	if step.Action == plan.ActionUpdate {
		// Deliberate no-op, recorded rather than silent: the operator can
		// see in the plan that the package's seed changed and that the
		// running app kept its own file.
		sr.Applied, sr.HealthOK = true, true
		return sr
	}

	res, err := r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_SeedVolumeFile{
		SeedVolumeFile: &wsv1.SeedVolumeFile{
			Volume:  want.SeedFile.Volume,
			Path:    want.SeedFile.Path,
			Content: []byte(want.SeedFile.Content),
		},
	}})
	if err != nil {
		sr.Error = err.Error()
		return sr
	}
	if oe := res.GetError(); oe != nil {
		sr.Error = fmt.Sprintf("%s: %s", oe.GetCode(), oe.GetMessage())
		return sr
	}
	sr.Applied, sr.HealthOK = true, true
	return sr
}

// observeSeedFile: present iff the path exists in the volume.
//
// Content is never compared, so a seed converges once and stays converged
// however the household edits the file afterwards.
//
// An undesired seed is reported ABSENT even if the file is still on disk.
// That is what stops a Delete step from ever being diffed, which is the
// intended behaviour: uninstalling a capability leaves its seeded config
// exactly where it is, the same way it leaves its volumes.
func (r *Reconciler) observeSeedFile(ctx context.Context, nodeID string, k resKey, want resource.Resource, isDesired bool) (resource.Resource, bool, error) {
	if !isDesired || want.SeedFile == nil {
		return resource.Resource{}, false, nil
	}

	res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_StatVolumeFile{
		StatVolumeFile: &wsv1.StatVolumeFile{Volume: want.SeedFile.Volume, Path: want.SeedFile.Path},
	}})
	if err != nil {
		return resource.Resource{}, false, fmt.Errorf("observe seed file %s: %w", k.Name, err)
	}
	if oe := res.GetError(); oe != nil {
		// NOT_FOUND is the volume itself not existing yet. On the first
		// tick of an install that is the normal case — the Volume create
		// is ordered ahead of this seed in the very same plan — so it
		// means "not seeded yet", not a fault.
		if oe.GetCode() == wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
			return resource.Resource{}, false, nil
		}
		return resource.Resource{}, false, fmt.Errorf("observe seed file %s: %s: %s", k.Name, oe.GetCode(), oe.GetMessage())
	}
	if !res.GetStatVolumeFile().GetExists() {
		return resource.Resource{}, false, nil
	}
	// Return the DESIRED resource, not a placeholder: the file is there,
	// and its content is no longer ours to have an opinion about.
	return want, true, nil
}
