package loop

import (
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

func TestGroupByNodeKeepsDockerAndClusterSeparate(t *testing.T) {
	docker := resource.Resource{Kind: resource.KindContainer, Metadata: resource.Metadata{Name: "docker", Node: "node-a"}, Container: &resource.ContainerSpec{}}
	cluster := resource.Resource{Kind: resource.KindContainer, Metadata: resource.Metadata{Name: "cluster", Node: "node-a"}, Container: &resource.ContainerSpec{Backend: "k3s"}}
	route := resource.Resource{Kind: resource.KindRoute, Metadata: resource.Metadata{Name: "route", Node: "node-a"}}
	groups := groupByNode([]resource.Resource{docker, cluster, route})
	if len(groups) != 2 || groups[0].node != "@cluster" || len(groups[0].resources) != 1 || groups[0].resources[0].Metadata.Name != "cluster" || groups[1].node != "node-a" || len(groups[1].resources) != 2 {
		t.Fatalf("backend groups = %+v", groups)
	}
}

func TestAuthorityTreatsDefaultAndExplicitDockerAsEquivalent(t *testing.T) {
	for _, tc := range []struct {
		name              string
		reviewed, planned string
	}{
		{name: "explicit approval and default observation", reviewed: "docker"},
		{name: "default approval and explicit observation", planned: "docker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &authority{plans: []queuedPlan{{id: "plan-docker-delete", entries: []entry{{
				kind: resource.KindContainer, name: "web", action: plan.ActionDelete, backend: tc.reviewed,
			}}}}, accepted: map[string]string{}}
			step := plan.PlanStep{Kind: resource.KindContainer, Name: "web", Action: plan.ActionDelete, Backend: tc.planned}
			if !a.covers(step) {
				t.Fatal("equivalent Docker spellings did not match")
			}
			planID, digest, ok := a.grant(step)
			if !ok || planID != "plan-docker-delete" || !strings.HasPrefix(digest, "sha256:") {
				t.Fatalf("Docker grant = %q, %q, %t", planID, digest, ok)
			}

			step.Backend = "k3s"
			if a.covers(step) {
				t.Fatal("docker approval covered a k3s delete")
			}
		})
	}
}
