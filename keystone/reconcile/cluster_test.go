package reconcile

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

var testAuthority = Authority{AcceptedRevision: "rev-1", PlanID: "plan-1", StepDigest: "digest-1", LeaderEpoch: 3}

func clusterContainer() resource.Resource {
	r := containerResource("app", testDigestImage)
	r.Container.Command = []string{"serve"}
	r.Container.Env = map[string]string{"TOKEN": "secret/app-token"}
	r.Container.Ports = []string{"8080:80"}
	r.Container.Mounts = []resource.Mount{{Source: "data", Target: "/data"}}
	r.Container.Resources = &resource.ContainerResources{Memory: "512m"}
	r.Container.ReadOnlyRootfs = true
	r.Container.CapDrop = []string{"ALL"}
	r.Container.ShmSize = "1g"
	r.Container.Restart = "unless-stopped"
	return r
}

// Every ContainerSpec field the cluster backend needs maps across, and the
// defaults are today's behaviour: one copy, Recreate, pinned to its node.
func TestApplyWorkloadMapsTheContainerAndKeepsTodaysDefaults(t *testing.T) {
	r := clusterContainer()
	req, err := cluster.applyWorkload(r, pkgspec.ComponentDecl{Name: "app"}, WorkloadPolicy{}, nil, testAuthority)
	if err != nil {
		t.Fatal(err)
	}
	if req.Op != OpApplyWorkload || req.Authority != testAuthority {
		t.Fatalf("request = %+v", req)
	}
	w := req.Workload
	if w.Image != r.Container.Image || !reflect.DeepEqual(w.Command, r.Container.Command) ||
		!reflect.DeepEqual(w.Env, r.Container.Env) || !reflect.DeepEqual(w.Ports, r.Container.Ports) ||
		!reflect.DeepEqual(w.Mounts, r.Container.Mounts) || w.Resources != r.Container.Resources ||
		!w.Security.ReadOnlyRootfs || !reflect.DeepEqual(w.Security.CapDrop, []string{"ALL"}) || w.ShmSize != "1g" {
		t.Fatalf("workload does not carry the container: %+v", w)
	}
	if w.Env["TOKEN"] != "secret/app-token" {
		t.Fatal("a secret reference must stay a reference")
	}
	if w.Replicas != 1 || w.Strategy != StrategyRecreate {
		t.Fatalf("replicas %d strategy %s, want 1 Recreate", w.Replicas, w.Strategy)
	}
	if w.NodeSelector["kubernetes.io/hostname"] != "node-a" {
		t.Fatalf("a component that may not move is not pinned: %v", w.NodeSelector)
	}
}

// The household chooses within the declaration, and a choice beyond it is
// refused rather than quietly reduced (capabilities contract §2, §8).
func TestApplyWorkloadHoldsPolicyToTheDeclaration(t *testing.T) {
	r := clusterContainer()
	many := pkgspec.ComponentDecl{Name: "app", Copies: pkgspec.CopiesMany, Relocate: true}
	req, err := cluster.applyWorkload(r, many, WorkloadPolicy{Copies: 3, KeepRunning: true},
		[]pkgspec.DeviceDecl{{Param: "gpu", Kind: pkgspec.DeviceClass, Class: "gpu"}}, testAuthority)
	if err != nil {
		t.Fatal(err)
	}
	w := req.Workload
	if w.Replicas != 3 || w.Strategy != StrategyRolling {
		t.Fatalf("replicas %d strategy %s, want 3 RollingUpdate", w.Replicas, w.Strategy)
	}
	if _, pinned := w.NodeSelector["kubernetes.io/hostname"]; pinned || w.NodeSelector["wildstone.dev/gpu"] != "true" {
		t.Fatalf("node selector = %v, want only the gpu label", w.NodeSelector)
	}

	one := pkgspec.ComponentDecl{Name: "app"}
	for _, c := range []struct {
		name   string
		policy WorkloadPolicy
		want   string
	}{
		{"copies the package does not allow", WorkloadPolicy{Copies: 2}, "declares one"},
		{"moving a package does not allow", WorkloadPolicy{KeepRunning: true}, "relocate: true"},
	} {
		if _, err := cluster.applyWorkload(r, one, c.policy, nil, testAuthority); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// A write without its authority is refused before it is built; a read
// needs none; a delete carries the UID the Plan saw (safety contract §2-§3).
func TestClusterWritesCarryTheirAuthority(t *testing.T) {
	r := clusterContainer()
	for _, missing := range []Authority{
		{PlanID: "p", StepDigest: "d", LeaderEpoch: 1},
		{AcceptedRevision: "r", StepDigest: "d", LeaderEpoch: 1},
		{AcceptedRevision: "r", PlanID: "p", LeaderEpoch: 1},
		{AcceptedRevision: "r", PlanID: "p", StepDigest: "d"},
	} {
		if _, err := cluster.applyWorkload(r, pkgspec.ComponentDecl{}, WorkloadPolicy{}, nil, missing); err == nil {
			t.Errorf("apply without %+v was built", missing)
		}
		if _, err := cluster.deleteWorkload("app", "uid-1", "", missing); err == nil {
			t.Errorf("delete without %+v was built", missing)
		}
	}
	if _, err := cluster.deleteWorkload("app", "", "", testAuthority); err == nil {
		t.Error("a delete without an object UID the Plan saw was built")
	}
	del, err := cluster.deleteWorkload("app", "uid-1", "", testAuthority)
	if err != nil || del.Op != OpDeleteWorkload || del.UID != "uid-1" || del.Authority != testAuthority {
		t.Fatalf("delete = %+v, %v", del, err)
	}
	serviceOnly, err := cluster.deleteWorkload("app", "", "svc-1", testAuthority)
	if err != nil || serviceOnly.UID != "" || serviceOnly.ServiceUID != "svc-1" {
		t.Fatalf("service-only delete = %+v, %v", serviceOnly, err)
	}
	if st := cluster.workloadStatus("app"); st.Op != OpWorkloadStatus || st.Authority != (Authority{}) {
		t.Fatalf("status = %+v", st)
	}
}

// Legacy Containers and every host resource stay at their node. A reviewed
// k3s choice changes only the Container's target.
func TestBackendTargetChoice(t *testing.T) {
	if got := TargetOf(clusterContainer()); got != (Target{Kind: TargetNode, Node: "node-a"}) {
		t.Fatalf("target = %+v", got)
	}
	k3s := clusterContainer()
	k3s.Container.Backend = "k3s"
	if got := TargetOf(k3s); got != (Target{Kind: TargetCluster}) {
		t.Fatalf("k3s target = %+v", got)
	}
	volume := resource.Resource{Kind: resource.KindVolume, Metadata: resource.Metadata{Node: "node-a"}}
	if got := TargetOf(volume); got != (Target{Kind: TargetNode, Node: "node-a"}) {
		t.Fatalf("host target = %+v", got)
	}
}

// The cluster request types carry typed fields only: no manifest, patch,
// raw bytes or free map of arbitrary values a package could fill.
func TestClusterRequestsHaveNoRawPayload(t *testing.T) {
	var walk func(reflect.Type, string)
	seen := map[reflect.Type]bool{}
	walk = func(tp reflect.Type, path string) {
		for tp.Kind() == reflect.Pointer || tp.Kind() == reflect.Slice {
			if tp.Kind() == reflect.Slice && tp.Elem().Kind() == reflect.Uint8 {
				t.Errorf("%s is raw bytes", path)
				return
			}
			tp = tp.Elem()
		}
		if tp.Kind() == reflect.Interface {
			t.Errorf("%s is an untyped value", path)
		}
		if tp.Kind() != reflect.Struct || seen[tp] {
			return
		}
		seen[tp] = true
		for i := 0; i < tp.NumField(); i++ {
			f := tp.Field(i)
			name := strings.ToLower(f.Name)
			for _, bad := range []string{"manifest", "yaml", "patch", "raw", "script"} {
				if strings.Contains(name, bad) {
					t.Errorf("%s.%s looks like a raw payload", path, f.Name)
				}
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(reflect.TypeOf(ClusterRequest{}), "ClusterRequest")
}

// Every ContainerSpec field is either carried into the WorkloadSpec or
// handled on purpose, so a field added to ContainerSpec later fails here
// until someone decides what the cluster does with it, rather than being
// dropped from cluster requests without anyone noticing.
func TestEveryContainerFieldIsCarriedOrHandled(t *testing.T) {
	carried := map[string]bool{
		"Image": true, "Env": true, "Ports": true, "Mounts": true, "Command": true,
		"Devices": true, "CapAdd": true, "CapDrop": true, "ReadOnlyRootfs": true,
		"NoNewPrivileges": true, "Resources": true, "ShmSize": true, "NetworkMode": true,
	}
	handled := map[string]string{
		"Backend":         "selects the cluster translator before request construction",
		"Expose":          "ClusterIntent turns internal ports into bounded Service ports",
		"Restart":         "a workload always restarts; any other policy is refused",
		"RunNetwork":      "execution-only metadata for Keystone-run one-shots, never serialized",
		"RuntimeIdentity": "refused until the k3s backend implements numeric runAsUser/runAsGroup",
	}
	tp := reflect.TypeOf(resource.ContainerSpec{})
	for i := 0; i < tp.NumField(); i++ {
		name := tp.Field(i).Name
		if !carried[name] && handled[name] == "" {
			t.Errorf("ContainerSpec.%s is neither carried into WorkloadSpec nor handled", name)
		}
	}
}

func TestApplyWorkloadRefusesARestartPolicyItCannotCarry(t *testing.T) {
	r := clusterContainer()
	r.Container.Restart = "no"
	if _, err := cluster.applyWorkload(r, pkgspec.ComponentDecl{}, WorkloadPolicy{}, nil, testAuthority); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("err = %v, want a restart refusal", err)
	}
}

func TestApplyWorkloadRefusesRuntimeIdentityUntilBackendSupportsIt(t *testing.T) {
	r := clusterContainer()
	r.Container.RuntimeIdentity = &resource.ContainerRuntimeIdentity{UID: 1000}
	if _, err := cluster.applyWorkload(r, pkgspec.ComponentDecl{}, WorkloadPolicy{}, nil, testAuthority); err == nil || !strings.Contains(err.Error(), "runtimeIdentity") {
		t.Fatalf("err = %v, want runtimeIdentity refusal", err)
	}
}
