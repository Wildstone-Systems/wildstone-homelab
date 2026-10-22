package reconcile

import (
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
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

func tailnetDesired() map[resKey]resource.Resource {
	return map[resKey]resource.Resource{
		{resource.KindIngress, "ingress"}: ingress("tailscale", "node.tail0.ts.net"),
	}
}

// The silent-permanent-failure case, and the reason the version gate
// exists at all.
//
// host_aliases arrived in protocol minor 14. A minor-13 agent IGNORES
// the field but still labels the container with whatever spec hash
// Keystone sent. If the hash counted the aliases, that container would
// carry the alias-aware hash while having no /etc/hosts entry — and
// after the agent was upgraded, observation would find a matching hash,
// call it converged and never recreate it. The entry would never arrive,
// login would stay broken, and every surface would report green.
func TestAliasesAreWithheldFromAnAgentThatCannotWriteThem(t *testing.T) {
	r := &Reconciler{AgentProtocolMinor: func(string) (uint32, bool) { return 13, true }}
	if got := r.hostAliasesForNode("n1", tailnetDesired()); len(got) != 0 {
		t.Fatalf("a minor-13 agent must be given no aliases, got %v", got)
	}
}

func TestAliasesReachAnAgentThatCanWriteThem(t *testing.T) {
	r := &Reconciler{AgentProtocolMinor: func(string) (uint32, bool) { return 14, true }}
	got := r.hostAliasesForNode("n1", tailnetDesired())
	if len(got) != 1 || got[0] != "node.tail0.ts.net" {
		t.Fatalf("a minor-14 agent should get the alias, got %v", got)
	}
}

// Assuming support we cannot see is how the silent version of this bug
// gets in.
func TestAnUnknownAgentVersionIsTreatedAsTooOld(t *testing.T) {
	for name, r := range map[string]*Reconciler{
		"no reporter":  {AgentProtocolMinor: nil},
		"unknown node": {AgentProtocolMinor: func(string) (uint32, bool) { return 0, false }},
	} {
		if got := r.hostAliasesForNode("n1", tailnetDesired()); len(got) != 0 {
			t.Errorf("%s: want no aliases, got %v", name, got)
		}
	}
}

// The whole point of the gate: the hash must be what it was BEFORE this
// feature while the agent is old, and must CHANGE the moment the agent
// can honour it — that difference is what recreates the container and
// delivers the entry, with no operator step.
func TestUpgradingTheAgentChangesTheHashAndSoRecreatesTheContainer(t *testing.T) {
	spec := &resource.ContainerSpec{Image: "docker.io/library/nginx@sha256:" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	desired := tailnetDesired()

	old := &Reconciler{AgentProtocolMinor: func(string) (uint32, bool) { return 13, true }}
	new := &Reconciler{AgentProtocolMinor: func(string) (uint32, bool) { return 14, true }}

	hashOld := specHash(spec, ContainerNetwork, "", old.hostAliasesForNode("n1", desired))
	hashNew := specHash(spec, ContainerNetwork, "", new.hostAliasesForNode("n1", desired))

	if hashOld == hashNew {
		t.Fatal("the hash must change when the agent gains host_aliases, or the container is never recreated and never gets its /etc/hosts entry")
	}
	// And while the agent is old, nothing churns: the hash equals the one
	// a Keystone without this feature would have produced.
	if want := specHash(spec, ContainerNetwork, "", nil); hashOld != want {
		t.Error("a minor-13 node must hash exactly as it did before this feature, or every container is recreated for nothing")
	}
}
