package loop

import (
	"testing"

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
