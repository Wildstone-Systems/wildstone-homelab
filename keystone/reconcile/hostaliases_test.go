package reconcile

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func ingress(tls, tailnet string) resource.Resource {
	return resource.Resource{
		Kind:     resource.KindIngress,
		Metadata: resource.Metadata{Name: "ingress", Owner: "capability/ingress", Node: "n1"},
		Ingress:  &resource.IngressSpec{TLS: tls, TailnetName: tailnet},
	}
}

// The bug this is written from: under the tailnet posture the OIDC
// issuer is the node's MagicDNS name, which Docker's embedded DNS cannot
// resolve. Every OIDC consumer answered 500 to every login attempt —
// open-webui, immich and homepage at once — with nothing in the plan or
// the events to say why.
func TestTailnetPostureGivesContainersTheIssuerName(t *testing.T) {
	desired := map[resKey]resource.Resource{
		{resource.KindIngress, "ingress"}: ingress("tailscale", "node.tail0.ts.net"),
	}
	got := hostAliasesFor(desired)
	if len(got) != 1 || got[0] != "node.tail0.ts.net" {
		t.Fatalf("want the tailnet name as a host alias, got %v", got)
	}
}

// With ordinary DNS the issuer resolves the same inside a container as
// outside. Adding an entry would only create a second answer that can go
// stale.
func TestOtherPosturesAddNothing(t *testing.T) {
	for _, tls := range []string{"acme", "internal", ""} {
		desired := map[resKey]resource.Resource{
			{resource.KindIngress, "ingress"}: ingress(tls, ""),
		}
		if got := hostAliasesFor(desired); len(got) != 0 {
			t.Errorf("tls=%q should add no aliases, got %v", tls, got)
		}
	}
}

func TestNoIngressAddsNothing(t *testing.T) {
	if got := hostAliasesFor(map[resKey]resource.Resource{}); len(got) != 0 {
		t.Errorf("want none, got %v", got)
	}
}

// A tailnet ingress that has not been given its name yet must not
// produce a bare ":ip" entry or an empty alias.
func TestTailnetWithoutANameAddsNothing(t *testing.T) {
	desired := map[resKey]resource.Resource{
		{resource.KindIngress, "ingress"}: ingress("tailscale", ""),
	}
	if got := hostAliasesFor(desired); len(got) != 0 {
		t.Errorf("want none, got %v", got)
	}
}

// The alias is part of a container's effective configuration: one that
// cannot resolve its own issuer is not the same container as one that
// can. If the hash ignored it, a deployment that gained the fix would
// keep every existing container exactly as broken as before.
func TestSpecHashDistinguishesContainersByTheirAliases(t *testing.T) {
	spec := &resource.ContainerSpec{Image: "docker.io/library/nginx@sha256:" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	without := specHash(spec, ContainerNetwork, "", nil)
	with := specHash(spec, ContainerNetwork, "", []string{"node.tail0.ts.net"})
	if without == with {
		t.Fatal("a container that can resolve its issuer must hash differently from one that cannot")
	}
	if again := specHash(spec, ContainerNetwork, "", []string{"node.tail0.ts.net"}); again != with {
		t.Fatal("the same aliases must hash the same, or every tick recreates the container")
	}
}
