package plan

import (
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
		Metadata:   resource.Metadata{Name: name, Owner: "system"},
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
		Metadata:   resource.Metadata{Name: name, Owner: "capability/media"},
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
