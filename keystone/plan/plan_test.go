package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func digest(b byte) string {
	return "ghcr.io/example/app@sha256:" + strings.Repeat(string(rune(b)), 64)
}

func vol(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindVolume,
		Metadata:   resource.Metadata{Name: name, Owner: "system", Node: "node-a"},
		Volume:     &resource.VolumeSpec{Name: name},
	}
}

func cont(name, image string, mounts []resource.Mount) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/media", Node: "node-a"},
		Container:  &resource.ContainerSpec{Image: image, Mounts: mounts},
	}
}

func route(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindRoute,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/media", Node: "node-a"},
		Route: &resource.RouteSpec{
			Host:           name + ".wildstone.dev",
			Upstream:       "container/" + name + ":80",
			Auth:           "none",
			Classification: "family_public",
		},
	}
}

func dns(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindDnsRecord,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/media"},
		DnsRecord: &resource.DnsRecordSpec{
			Zone: "wildstone.dev", Name: name, Type: "A", Content: "10.0.0.1",
		},
	}
}

func imgA() string { return digest('a') }
func imgB() string { return digest('b') }

func TestDiffEmptyToThreeCreatesLowInOrder(t *testing.T) {
	desired := []resource.Resource{
		dns("d1"),
		route("r1"),
		cont("c1", imgA(), nil),
		vol("v1"),
	}
	p := Diff(desired, nil)
	if len(p.Steps) != 4 {
		t.Fatalf("expected 4 steps, got %d: %+v", len(p.Steps), p.Steps)
	}
	wantOrder := []resource.Kind{resource.KindVolume, resource.KindContainer, resource.KindRoute, resource.KindDnsRecord}
	for i, step := range p.Steps {
		if step.Kind != wantOrder[i] {
			t.Errorf("step %d: got kind %s, want %s", i, step.Kind, wantOrder[i])
		}
		if step.Action != ActionCreate {
			t.Errorf("step %d: got action %s, want Create", i, step.Action)
		}
		if step.Risk != RiskLow {
			t.Errorf("step %d (%s): got risk %s, want Low", i, step.Kind, step.Risk)
		}
	}
	if p.OverallRisk != RiskLow {
		t.Errorf("OverallRisk: got %s, want Low", p.OverallRisk)
	}
}

func TestDiffIdenticalIsNoSteps(t *testing.T) {
	resources := []resource.Resource{vol("v1"), cont("c1", imgA(), nil), route("r1"), dns("d1")}
	p := Diff(resources, resources)
	if len(p.Steps) != 0 {
		t.Fatalf("expected no steps for identical desired/observed, got %+v", p.Steps)
	}
	if p.OverallRisk != RiskLow {
		t.Errorf("OverallRisk on empty plan: got %s, want Low", p.OverallRisk)
	}
}

func TestDiffContainerImageChangeIsMediumUpdate(t *testing.T) {
	desired := []resource.Resource{cont("c1", imgB(), nil)}
	observed := []resource.Resource{cont("c1", imgA(), nil)}
	p := Diff(desired, observed)
	if len(p.Steps) != 1 {
		t.Fatalf("expected 1 step, got %+v", p.Steps)
	}
	step := p.Steps[0]
	if step.Action != ActionUpdate {
		t.Errorf("Action: got %s, want Update", step.Action)
	}
	if step.Risk != RiskMedium {
		t.Errorf("Risk: got %s, want Medium", step.Risk)
	}
	if p.OverallRisk != RiskMedium {
		t.Errorf("OverallRisk: got %s, want Medium", p.OverallRisk)
	}
}

func TestDiffVolumeDeleteIsHigh(t *testing.T) {
	observed := []resource.Resource{vol("v1")}
	p := Diff(nil, observed)
	if len(p.Steps) != 1 {
		t.Fatalf("expected 1 step, got %+v", p.Steps)
	}
	step := p.Steps[0]
	if step.Action != ActionDelete {
		t.Errorf("Action: got %s, want Delete", step.Action)
	}
	if step.Risk != RiskHigh {
		t.Errorf("Risk: got %s, want High", step.Risk)
	}
	if p.OverallRisk != RiskHigh {
		t.Errorf("OverallRisk: got %s, want High", p.OverallRisk)
	}
}

func TestDiffContainerMountChangeIsHighUpdate(t *testing.T) {
	desired := []resource.Resource{cont("c1", imgA(), []resource.Mount{{Source: "/data", Target: "/data"}})}
	observed := []resource.Resource{cont("c1", imgA(), nil)}
	p := Diff(desired, observed)
	if len(p.Steps) != 1 {
		t.Fatalf("expected 1 step, got %+v", p.Steps)
	}
	if p.Steps[0].Risk != RiskHigh {
		t.Errorf("Risk: got %s, want High", p.Steps[0].Risk)
	}
}

func TestDiffContainerDeleteWithMountIsHigh(t *testing.T) {
	observed := []resource.Resource{cont("c1", imgA(), []resource.Mount{{Source: "/data", Target: "/data"}})}
	p := Diff(nil, observed)
	if len(p.Steps) != 1 {
		t.Fatalf("expected 1 step, got %+v", p.Steps)
	}
	if p.Steps[0].Risk != RiskHigh {
		t.Errorf("Risk: got %s, want High", p.Steps[0].Risk)
	}
}

func TestDiffDeletesOrderedReverse(t *testing.T) {
	observed := []resource.Resource{vol("v1"), cont("c1", imgA(), nil), route("r1"), dns("d1")}
	p := Diff(nil, observed)
	if len(p.Steps) != 4 {
		t.Fatalf("expected 4 steps, got %+v", p.Steps)
	}
	wantOrder := []resource.Kind{resource.KindDnsRecord, resource.KindRoute, resource.KindContainer, resource.KindVolume}
	for i, step := range p.Steps {
		if step.Kind != wantOrder[i] {
			t.Errorf("step %d: got kind %s, want %s", i, step.Kind, wantOrder[i])
		}
		if step.Action != ActionDelete {
			t.Errorf("step %d: got action %s, want Delete", i, step.Action)
		}
	}
	if p.OverallRisk != RiskHigh { // volume delete present
		t.Errorf("OverallRisk: got %s, want High", p.OverallRisk)
	}
}

func TestDiffMixedCreatesUpdatesDeletesOverallRiskIsMax(t *testing.T) {
	desired := []resource.Resource{
		vol("v1"),               // unchanged -> no step
		cont("c1", imgB(), nil), // image change -> Medium update
		dns("d2"),               // new -> Low create
	}
	observed := []resource.Resource{
		vol("v1"),
		cont("c1", imgA(), nil),
		vol("v2"), // deleted -> High
	}
	p := Diff(desired, observed)
	if p.OverallRisk != RiskHigh {
		t.Fatalf("OverallRisk: got %s, want High; steps=%+v", p.OverallRisk, p.Steps)
	}
	// Expect: create dns d2 (Low), update c1 (Medium), delete v2 (High).
	if len(p.Steps) != 3 {
		t.Fatalf("expected 3 steps, got %d: %+v", len(p.Steps), p.Steps)
	}
}

func oidcClient(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindOidcClient,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/identity", Node: "node-a"},
		OidcClient: &resource.OidcClientSpec{
			ClientId:     name,
			ClientName:   name,
			RedirectUris: []string{"https://" + name + ".wildstone.dev/auth/login"},
			Scopes:       []string{"openid", "profile", "email", "groups"},
			AuthPolicy:   "two_factor",
			SecretRef:    "secret/" + name + "-oidc",
		},
	}
}

func ldapGroup(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindLdapGroup,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/identity", Node: "node-a"},
		LdapGroup:  &resource.LdapGroupSpec{Name: name},
	}
}

// TestDiffOidcClientAndLdapGroupOrderBeforeContainer locks in the ADR-W005
// S3a ordering choice (see kindOrder's doc comment): LdapGroup and
// OidcClient — identity wiring an app container may assume already exists
// — are ordered right after Volume and before Container, ahead of where
// Route/DnsRecord already sat. This is a planner-review-flagged choice, not
// a proven dependency; this test exists to make an accidental reordering
// visible, not to re-litigate it.
func TestDiffOidcClientAndLdapGroupOrderBeforeContainer(t *testing.T) {
	desired := []resource.Resource{
		dns("d1"),
		route("r1"),
		cont("c1", imgA(), nil),
		oidcClient("immich"),
		ldapGroup("photos-users"),
		vol("v1"),
	}
	p := Diff(desired, nil)
	wantOrder := []resource.Kind{
		resource.KindVolume,
		resource.KindLdapGroup,
		resource.KindOidcClient,
		resource.KindContainer,
		resource.KindRoute,
		resource.KindDnsRecord,
	}
	if len(p.Steps) != len(wantOrder) {
		t.Fatalf("expected %d steps, got %d: %+v", len(wantOrder), len(p.Steps), p.Steps)
	}
	for i, step := range p.Steps {
		if step.Kind != wantOrder[i] {
			t.Errorf("step %d: got kind %s, want %s (full order: %+v)", i, step.Kind, wantOrder[i], p.Steps)
		}
	}
}

func TestDiffOidcClientCreateIsLowUpdateIsMedium(t *testing.T) {
	desired := []resource.Resource{oidcClient("immich")}
	p := Diff(desired, nil)
	if len(p.Steps) != 1 || p.Steps[0].Risk != RiskLow {
		t.Fatalf("expected a single Low-risk create, got: %+v", p.Steps)
	}

	changed := oidcClient("immich")
	changed.OidcClient.AuthPolicy = "one_factor"
	p2 := Diff([]resource.Resource{changed}, desired)
	if len(p2.Steps) != 1 || p2.Steps[0].Action != ActionUpdate || p2.Steps[0].Risk != RiskMedium {
		t.Fatalf("expected a single Medium-risk update, got: %+v", p2.Steps)
	}
}

func TestDiffLdapGroupIsAlwaysLow(t *testing.T) {
	desired := []resource.Resource{ldapGroup("photos-users")}
	p := Diff(desired, nil)
	if len(p.Steps) != 1 || p.Steps[0].Risk != RiskLow {
		t.Fatalf("expected a single Low-risk create, got: %+v", p.Steps)
	}
}

func TestDiffBackupPlan(t *testing.T) {
	bp := func(schedule string) resource.Resource {
		return resource.Resource{
			Kind:     resource.KindBackupPlan,
			Metadata: resource.Metadata{Name: "photos", Owner: "capability/photos", Node: "n1"},
			BackupPlan: &resource.BackupPlanSpec{
				Volumes:        []string{"immich-library"},
				Repository:     "backup-repo",
				PasswordSecret: "secret/restic-photos",
				Schedule:       schedule,
			},
		}
	}
	// Create is Low: the declaration moves no data (ADR-W011 A4).
	p := Diff([]resource.Resource{bp("daily")}, nil)
	if len(p.Steps) != 1 || p.Steps[0].Action != ActionCreate || p.OverallRisk != RiskLow {
		t.Fatalf("create plan = %+v", p)
	}
	// Unchanged spec: no step.
	p = Diff([]resource.Resource{bp("daily")}, []resource.Resource{bp("daily")})
	if len(p.Steps) != 0 {
		t.Fatalf("unchanged BackupPlan produced steps: %+v", p.Steps)
	}
	// Schedule change: an Update, still Low.
	p = Diff([]resource.Resource{bp("weekly")}, []resource.Resource{bp("daily")})
	if len(p.Steps) != 1 || p.Steps[0].Action != ActionUpdate || p.OverallRisk != RiskLow {
		t.Fatalf("update plan = %+v", p)
	}
	// Removal: Medium — protection stops, a conscious confirmation.
	p = Diff(nil, []resource.Resource{bp("daily")})
	if len(p.Steps) != 1 || p.Steps[0].Action != ActionDelete || p.OverallRisk != RiskMedium {
		t.Fatalf("delete plan = %+v", p)
	}
}

// TestPlanJSONShape pins the wire form the approval screen reads: named
// risk and action, camelCase keys, no JSON nested in a JSON string. The
// ordinals `"Risk":0` / `"Action":1` were unreadable to the human the
// plan exists for.
func TestPlanJSONShape(t *testing.T) {
	p := Plan{
		Summary:     "add a volume",
		OverallRisk: RiskMedium,
		Steps: []PlanStep{{
			Description: "Create Volume/relvol2",
			Kind:        resource.KindVolume,
			Name:        "relvol2",
			Action:      ActionCreate,
			Risk:        RiskLow,
			BlastRadius: "new volume, no existing service affected",
		}},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"overallRisk":"Medium"`, `"risk":"Low"`, `"action":"Create"`,
		`"blastRadius":"new volume, no existing service affected"`,
	} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("plan JSON missing %s: %s", want, b)
		}
	}
}

// TestPlanJSONAcceptsStoredOrdinals: plan records written before the
// named form are still in the runtime DB, and approval re-reads exactly
// that stored JSON — it must not start failing on them.
func TestPlanJSONAcceptsStoredOrdinals(t *testing.T) {
	const stored = `{"Summary":"old","Steps":[{"Description":"d","Kind":"Volume",` +
		`"Name":"v","Action":1,"Risk":2,"BlastRadius":"b"}],"OverallRisk":2}`
	var p Plan
	if err := json.Unmarshal([]byte(stored), &p); err != nil {
		t.Fatal(err)
	}
	if p.OverallRisk != RiskHigh || len(p.Steps) != 1 ||
		p.Steps[0].Action != ActionCreate || p.Steps[0].Risk != RiskHigh {
		t.Fatalf("stored ordinals lost: %+v", p)
	}
}

// TestEveryKindHasADeclaredOrder closes the hole that let ConfigFile
// ship with no entry in kindOrder. The registry is closed, so this can
// enumerate it: adding a kind now forces its author to place it in the
// dependency order deliberately, rather than inheriting whatever a
// missing map key happens to mean that day.
//
// It is worth more than it looks. A kind's position decides whether the
// file a container mounts is written before the container starts; the
// bug this replaces was invisible precisely because the accidental
// answer was the correct one.
func TestEveryKindHasADeclaredOrder(t *testing.T) {
	for _, k := range resource.Kinds() {
		if _, ok := kindOrder[k]; !ok {
			t.Errorf("kind %s has no entry in kindOrder: place it deliberately in the "+
				"dependency order (before the kinds that consume it, after the kinds it consumes)", k)
		}
	}
}

// TestConfigFileIsOrderedBeforeContainer states the one ordering this
// release actually depends on, so a future renumbering that breaks it
// fails here rather than on a node, as an app reading the config it had
// before the write landed.
func TestConfigFileIsOrderedBeforeContainer(t *testing.T) {
	if KindOrder(resource.KindConfigFile) >= KindOrder(resource.KindContainer) {
		t.Errorf("ConfigFile (%d) must sort before Container (%d): a container mounts its config, "+
			"so the file has to be on disk before the container that reads it starts",
			KindOrder(resource.KindConfigFile), KindOrder(resource.KindContainer))
	}
}

// TestDiffAutomation pins the grading of a household rule (ADR-W024 A1):
// create and change are Medium — a rule acts on its own once approved,
// so it must always pass a human, never drift repair — and removal is
// Low. An unchanged rule, compared deeply, is no step.
func TestDiffAutomation(t *testing.T) {
	rule := func(window string) resource.Resource {
		return resource.Resource{
			Kind:     resource.KindAutomation,
			Metadata: resource.Metadata{Name: "breakfast-check", Owner: "household/mika"},
			Automation: &resource.AutomationSpec{
				Description: "breakfast",
				Trigger:     resource.AutomationTrigger{EventType: "person.left_home"},
				Conditions:  []resource.AutomationCondition{{Kind: "cooldown", Window: window}},
				Action:      resource.AutomationAction{Tool: "notifications.notify", Tier: "domestic", Params: map[string]string{"message": "hi"}},
			},
		}
	}
	p := Diff([]resource.Resource{rule("20h")}, nil)
	if len(p.Steps) != 1 || p.Steps[0].Action != ActionCreate || p.OverallRisk != RiskMedium {
		t.Fatalf("create plan = %+v", p)
	}
	p = Diff([]resource.Resource{rule("20h")}, []resource.Resource{rule("20h")})
	if len(p.Steps) != 0 {
		t.Fatalf("unchanged Automation produced steps: %+v", p.Steps)
	}
	p = Diff([]resource.Resource{rule("6h")}, []resource.Resource{rule("20h")})
	if len(p.Steps) != 1 || p.Steps[0].Action != ActionUpdate || p.OverallRisk != RiskMedium {
		t.Fatalf("update plan = %+v", p)
	}
	p = Diff(nil, []resource.Resource{rule("20h")})
	if len(p.Steps) != 1 || p.Steps[0].Action != ActionDelete || p.OverallRisk != RiskLow {
		t.Fatalf("delete plan = %+v", p)
	}
	if KindOrder(resource.KindAutomation) <= KindOrder(resource.KindContainer) {
		t.Fatalf("Automation (%d) must sort after Container (%d): a rule names a capability's tool",
			KindOrder(resource.KindAutomation), KindOrder(resource.KindContainer))
	}
}

func backupTargetRes(bucket string) resource.Resource {
	return resource.Resource{
		Kind:     resource.KindBackupTarget,
		Metadata: resource.Metadata{Name: "offsite", Owner: "owner", Node: "n1"},
		BackupTarget: &resource.BackupTargetSpec{
			Type: "s3", Endpoint: "https://s3.example.com", Bucket: bucket,
			AccessKeySecret: "secret/offsite-access", SecretKeySecret: "secret/offsite-secret",
		},
	}
}

// TestDiffBackupTarget: naming storage outside the house is High on
// create and change (ADR-W045 A7); removing it is Medium.
func TestDiffBackupTarget(t *testing.T) {
	p := Diff([]resource.Resource{backupTargetRes("b-one")}, nil)
	if len(p.Steps) != 1 || p.Steps[0].Action != ActionCreate || p.OverallRisk != RiskHigh {
		t.Fatalf("create plan = %+v", p)
	}
	p = Diff([]resource.Resource{backupTargetRes("b-one")}, []resource.Resource{backupTargetRes("b-one")})
	if len(p.Steps) != 0 {
		t.Fatalf("unchanged BackupTarget produced steps: %+v", p.Steps)
	}
	p = Diff([]resource.Resource{backupTargetRes("b-two")}, []resource.Resource{backupTargetRes("b-one")})
	if len(p.Steps) != 1 || p.Steps[0].Action != ActionUpdate || p.OverallRisk != RiskHigh {
		t.Fatalf("update plan = %+v", p)
	}
	p = Diff(nil, []resource.Resource{backupTargetRes("b-one")})
	if len(p.Steps) != 1 || p.Steps[0].Action != ActionDelete || p.OverallRisk != RiskMedium {
		t.Fatalf("delete plan = %+v", p)
	}
}

// TestBackupTargetOrderedBeforeBackupPlan: a plan's off-site copy names
// its target, so the target is declared first.
func TestBackupTargetOrderedBeforeBackupPlan(t *testing.T) {
	if KindOrder(resource.KindBackupTarget) >= KindOrder(resource.KindBackupPlan) {
		t.Fatalf("BackupTarget order %d is not before BackupPlan %d",
			KindOrder(resource.KindBackupTarget), KindOrder(resource.KindBackupPlan))
	}
}

func offsiteBP(off *resource.BackupOffsite) resource.Resource {
	return resource.Resource{
		Kind:     resource.KindBackupPlan,
		Metadata: resource.Metadata{Name: "photos", Owner: "capability/photos", Node: "n1"},
		BackupPlan: &resource.BackupPlanSpec{
			Volumes: []string{"immich-library"}, Repository: "backup-repo",
			PasswordSecret: "secret/restic-photos", Schedule: "daily", Offsite: off,
		},
	}
}

// TestDiffBackupPlanOffsite: sending data off-site, or changing where it
// goes, is High; turning it off is Medium; tuning its schedule is Low.
func TestDiffBackupPlanOffsite(t *testing.T) {
	off := func(target, route, schedule string) *resource.BackupOffsite {
		return &resource.BackupOffsite{Target: target, Route: route, PasswordSecret: "secret/restic-photos-offsite", Schedule: schedule}
	}
	base := off("offsite", resource.BackupRouteLocalAndOffsite, "weekly")
	for _, tc := range []struct {
		name       string
		want, have *resource.BackupOffsite
		observed   bool
		risk       RiskGrade
	}{
		{"create with offsite", base, nil, false, RiskHigh},
		{"offsite added", base, nil, true, RiskHigh},
		{"target changed", off("other", resource.BackupRouteLocalAndOffsite, "weekly"), base, true, RiskHigh},
		{"route changed", off("offsite", resource.BackupRouteOffsiteOnly, "weekly"), base, true, RiskHigh},
		{"schedule changed", off("offsite", resource.BackupRouteLocalAndOffsite, "monthly"), base, true, RiskLow},
		{"offsite removed", nil, base, true, RiskMedium},
	} {
		var observed []resource.Resource
		if tc.observed {
			observed = []resource.Resource{offsiteBP(tc.have)}
		}
		p := Diff([]resource.Resource{offsiteBP(tc.want)}, observed)
		if len(p.Steps) != 1 || p.OverallRisk != tc.risk {
			t.Errorf("%s: plan = %+v, want one step at %s", tc.name, p.Steps, tc.risk)
		}
	}
}

// TestDiffNetworkMount: mounting a declared share is Low (so a dropped
// share is re-mounted unattended); changing one is Medium.
func TestDiffNetworkMount(t *testing.T) {
	m := func(src string) resource.Resource {
		return resource.Resource{Kind: resource.KindNetworkMount, Metadata: resource.Metadata{Name: "media", Owner: "owner", Node: "n1"},
			NetworkMount: &resource.NetworkMountSpec{Type: "nfs4", Source: src, Target: "/srv/wildstone/nas/media"}}
	}
	if p := Diff([]resource.Resource{m("nas:/a")}, nil); p.OverallRisk != RiskLow {
		t.Fatalf("create = %s", p.OverallRisk)
	}
	if p := Diff([]resource.Resource{m("nas:/b")}, []resource.Resource{m("nas:/a")}); p.OverallRisk != RiskMedium {
		t.Fatalf("update = %s", p.OverallRisk)
	}
	if KindOrder(resource.KindNetworkMount) >= KindOrder(resource.KindBackupPlan) || KindOrder(resource.KindNetworkMount) >= KindOrder(resource.KindContainer) {
		t.Fatal("a share must be mounted before what reads it")
	}
}

// TestDiffOffsiteSourcesAdded: adding a folder to a plan that already
// goes off-site sends more data out of the house, so it is High.
func TestDiffOffsiteSourcesAdded(t *testing.T) {
	off := &resource.BackupOffsite{Target: "offsite", Route: resource.BackupRouteLocalAndOffsite, PasswordSecret: "secret/o", Schedule: "weekly"}
	have := offsiteBP(off)
	want := offsiteBP(off)
	want.BackupPlan.Paths = []string{"/srv/wildstone/nas/media/movies"}
	if p := Diff([]resource.Resource{want}, []resource.Resource{have}); p.OverallRisk != RiskHigh {
		t.Fatalf("sources added to an off-site plan = %s, want High", p.OverallRisk)
	}
	// Removing a source is not High: less leaves the house.
	fewer := offsiteBP(off)
	fewer.BackupPlan.Volumes = nil
	fewer.BackupPlan.Paths = []string{"/srv/x"}
	have.BackupPlan.Paths = []string{"/srv/x"}
	if p := Diff([]resource.Resource{fewer}, []resource.Resource{have}); p.OverallRisk == RiskHigh {
		t.Fatal("removing a source graded High")
	}
}

// TestDiffOffsiteExcludeRemoved: dropping an exclude from a plan that
// goes off-site sends more data out, so it is High; adding one is not.
func TestDiffOffsiteExcludeRemoved(t *testing.T) {
	off := &resource.BackupOffsite{Target: "offsite", Route: resource.BackupRouteLocalAndOffsite, PasswordSecret: "secret/o", Schedule: "weekly"}
	with := offsiteBP(off)
	with.BackupPlan.Exclude = []string{"*.iso"}
	without := offsiteBP(off)
	if p := Diff([]resource.Resource{without}, []resource.Resource{with}); p.OverallRisk != RiskHigh {
		t.Fatalf("exclude removed = %s, want High", p.OverallRisk)
	}
	if p := Diff([]resource.Resource{with}, []resource.Resource{without}); p.OverallRisk == RiskHigh {
		t.Fatal("adding an exclude graded High")
	}
}

// TestDiffContainerNetworkModeIsDescribed: a move to host networking is a
// Medium update, and both the description and the blast radius say so,
// because sharing the node's interfaces is what the approver is actually
// being asked to allow.
func TestDiffContainerNetworkModeIsDescribed(t *testing.T) {
	host := cont("hub", imgA(), nil)
	host.Container.NetworkMode = resource.NetworkModeHost

	p := Diff([]resource.Resource{host}, []resource.Resource{cont("hub", imgA(), nil)})
	if len(p.Steps) != 1 || p.Steps[0].Action != ActionUpdate || p.Steps[0].Risk != RiskMedium {
		t.Fatalf("bridge -> host: got %+v, want one Medium update", p.Steps)
	}
	if s := p.Steps[0]; !strings.Contains(s.Description, "host network") || !strings.Contains(s.BlastRadius, "host's network namespace") {
		t.Errorf("bridge -> host step does not say host networking: %+v", s)
	}

	p = Diff([]resource.Resource{cont("hub", imgA(), nil)}, []resource.Resource{host})
	if len(p.Steps) != 1 || p.Steps[0].Action != ActionUpdate {
		t.Fatalf("host -> bridge: got %+v, want one update", p.Steps)
	}
	if strings.Contains(p.Steps[0].Description, "host network") {
		t.Errorf("host -> bridge step still says host network: %q", p.Steps[0].Description)
	}

	p = Diff([]resource.Resource{host}, nil)
	if len(p.Steps) != 1 || p.Steps[0].Risk != RiskLow || !strings.Contains(p.Steps[0].Description, "(host network)") {
		t.Errorf("host create: got %+v, want a Low create described as host network", p.Steps)
	}
}
