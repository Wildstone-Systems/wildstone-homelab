package reconcile

import (
	"context"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// seedApplier records seed/stat ops and answers stat with a scripted
// existence answer.
type seedApplier struct {
	seeded  []*wsv1.SeedVolumeFile
	statted []*wsv1.StatVolumeFile
	exists  bool
	// statErr, when set, is returned as an OpError from StatVolumeFile.
	statErr wsv1.ErrorCode
}

func (a *seedApplier) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_SeedVolumeFile:
		a.seeded = append(a.seeded, o.SeedVolumeFile)
		return &wsv1.OpResult{Result: &wsv1.OpResult_SeedVolumeFile{
			SeedVolumeFile: &wsv1.SeedVolumeFileResult{Written: true}}}, nil
	case *wsv1.Operation_StatVolumeFile:
		a.statted = append(a.statted, o.StatVolumeFile)
		if a.statErr != wsv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
			return &wsv1.OpResult{Result: &wsv1.OpResult_Error{
				Error: &wsv1.OpError{Code: a.statErr, Message: "scripted"}}}, nil
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_StatVolumeFile{
			StatVolumeFile: &wsv1.StatVolumeFileResult{Exists: a.exists}}}, nil
	}
	return &wsv1.OpResult{}, nil
}

func seedResource() resource.Resource {
	return resource.Resource{
		Kind:     resource.KindSeedFile,
		Metadata: resource.Metadata{Name: "ha-config", Owner: "capability/home-assistant", Node: "n1"},
		SeedFile: &resource.SeedFileSpec{Volume: "ha-config", Path: "configuration.yaml", Content: "a: b\n"},
	}
}

func seedReconciler(a Applier) *Reconciler { return &Reconciler{Applier: a} }

func TestSeedFileCreateSendsTheSeed(t *testing.T) {
	a := &seedApplier{}
	r := seedReconciler(a)
	want := seedResource()
	sr := r.applySeedFileStep(context.Background(), "n1",
		plan.PlanStep{Kind: resource.KindSeedFile, Name: "ha-config", Action: plan.ActionCreate},
		map[resKey]resource.Resource{{resource.KindSeedFile, "ha-config"}: want})

	if !sr.Applied || !sr.HealthOK {
		t.Fatalf("create should apply cleanly, got %+v", sr)
	}
	if len(a.seeded) != 1 {
		t.Fatalf("want one seed op, got %d", len(a.seeded))
	}
	if a.seeded[0].GetVolume() != "ha-config" || a.seeded[0].GetPath() != "configuration.yaml" {
		t.Errorf("seed addressed the wrong file: %v", a.seeded[0])
	}
}

// A package shipping different seed content must NOT rewrite the file:
// it belongs to the household by then. The step still succeeds, because
// failing it would set the resource aside forever over a difference we
// have deliberately chosen not to act on.
func TestSeedFileUpdateNeverRewritesTheAppsFile(t *testing.T) {
	a := &seedApplier{}
	r := seedReconciler(a)
	want := seedResource()
	want.SeedFile.Content = "something the package changed later\n"
	sr := r.applySeedFileStep(context.Background(), "n1",
		plan.PlanStep{Kind: resource.KindSeedFile, Name: "ha-config", Action: plan.ActionUpdate},
		map[resKey]resource.Resource{{resource.KindSeedFile, "ha-config"}: want})

	if !sr.Applied || !sr.HealthOK {
		t.Fatalf("update should be a clean no-op, got %+v", sr)
	}
	if len(a.seeded) != 0 {
		t.Fatalf("update must send NO seed op; the app owns the file now (sent %d)", len(a.seeded))
	}
}

// Uninstalling leaves seeded data alone, the same as a volume.
func TestSeedFileDeleteRemovesNothing(t *testing.T) {
	a := &seedApplier{}
	r := seedReconciler(a)
	sr := r.applySeedFileStep(context.Background(), "n1",
		plan.PlanStep{Kind: resource.KindSeedFile, Name: "ha-config", Action: plan.ActionDelete},
		map[resKey]resource.Resource{})

	if !sr.Applied || !sr.HealthOK {
		t.Fatalf("delete should be a clean no-op, got %+v", sr)
	}
	if len(a.seeded) != 0 {
		t.Fatalf("delete must send nothing, sent %d", len(a.seeded))
	}
}

// Observation is existence-only: a household that edits its own config
// must not read as drift, or "converged" comes to mean "nobody has
// touched their settings".
func TestSeedFileObservedByExistenceNotContent(t *testing.T) {
	a := &seedApplier{exists: true}
	r := seedReconciler(a)
	want := seedResource()
	// What is on disk is nothing like the seed any more.
	obs, present, err := r.observeSeedFile(context.Background(), "n1",
		resKey{resource.KindSeedFile, "ha-config"}, want, true)
	if err != nil || !present {
		t.Fatalf("an existing seed should be observed present, got present=%v err=%v", present, err)
	}
	if obs.SeedFile == nil || obs.SeedFile.Content != want.SeedFile.Content {
		t.Error("observe should report the DESIRED resource, so the diff sees no drift")
	}
}

// The volume does not exist on the first tick of an install: the Volume
// create is ordered ahead of the seed in the very same plan. That is
// "not seeded yet", not a fault.
func TestSeedFileMissingVolumeIsAbsentNotAnError(t *testing.T) {
	a := &seedApplier{statErr: wsv1.ErrorCode_ERROR_CODE_NOT_FOUND}
	r := seedReconciler(a)
	_, present, err := r.observeSeedFile(context.Background(), "n1",
		resKey{resource.KindSeedFile, "ha-config"}, seedResource(), true)
	if err != nil {
		t.Fatalf("a missing volume must not fail the observe pass: %v", err)
	}
	if present {
		t.Error("a seed whose volume does not exist yet is absent")
	}
}

// An undesired seed reports absent, which is what stops a Delete step
// from ever being diffed.
func TestSeedFileUndesiredNeverDiffsADelete(t *testing.T) {
	a := &seedApplier{exists: true}
	r := seedReconciler(a)
	_, present, err := r.observeSeedFile(context.Background(), "n1",
		resKey{resource.KindSeedFile, "ha-config"}, resource.Resource{}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if present {
		t.Error("an undesired seed must report absent so no Delete is ever planned")
	}
	if len(a.statted) != 0 {
		t.Error("an undesired seed should not even be statted: its spec is gone")
	}
}
