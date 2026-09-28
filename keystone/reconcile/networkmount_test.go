package reconcile

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// shareNode fakes a node's mount table: EnsureMount mounts, DiskSpace
// reports what is mounted where. Every other op succeeds empty.
type shareNode struct {
	mu      sync.Mutex
	mounted map[string]liveMount // target -> what is mounted there
	ensured int
	tooOld  bool
	// sticky: EnsureMount of an already-mounted target leaves the old
	// mount live, as the real agent does (it only rewrites fstab).
	sticky bool
}

type liveMount struct {
	fstype, source string
	options        []string
}

func (n *shareNode) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_EnsureMount:
		n.ensured++
		t := o.EnsureMount.GetTarget()
		if _, there := n.mounted[t]; !there || !n.sticky {
			n.mounted[t] = liveMount{fstype: "nfs4", source: o.EnsureMount.GetSource(),
				options: append([]string{"rw", "nosuid", "nodev", "vers=4.2"}, o.EnsureMount.GetOptions()...)}
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_EnsureMount{EnsureMount: &wsv1.EnsureMountResult{Mounted: true}}}, nil
	case *wsv1.Operation_DiskSpace:
		if n.tooOld {
			return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{Code: wsv1.ErrorCode_ERROR_CODE_INVALID}}}, nil
		}
		t := o.DiskSpace.GetPath()
		if m, ok := n.mounted[t]; ok {
			return &wsv1.OpResult{Result: &wsv1.OpResult_DiskSpace{DiskSpace: &wsv1.DiskSpaceResult{
				MountPoint: t, Fstype: m.fstype, Source: m.source, Options: m.options, Exists: true}}}, nil
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_DiskSpace{DiskSpace: &wsv1.DiskSpaceResult{MountPoint: "/", Fstype: "ext4", Exists: true}}}, nil
	}
	return &wsv1.OpResult{}, nil
}

func mediaShare() resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindNetworkMount,
		Metadata:     resource.Metadata{Name: "media", Owner: "owner", Node: "node-a"},
		NetworkMount: &resource.NetworkMountSpec{Type: "nfs4", Source: "nas.example.internal:/media", Target: "/srv/wildstone/nas/media"},
	}
}

// TestNetworkMountMountsAndRemounts: a declared share is mounted; once
// mounted, reconcile is a no-op; when it drops (the NAS rebooted), the
// next reconcile mounts it again without asking — it is Low.
func TestNetworkMountMountsAndRemounts(t *testing.T) {
	node := &shareNode{mounted: map[string]liveMount{}}
	r := newReconciler(node)
	desired := []resource.Resource{mediaShare()}

	out, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Applied || len(out.Steps) != 1 || out.Steps[0].Step.Action != plan.ActionCreate || !out.Steps[0].HealthOK {
		t.Fatalf("first reconcile = %+v", out)
	}
	again, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil || len(again.Plan.Steps) != 0 {
		t.Fatalf("mounted share re-planned: %+v %v", again.Plan.Steps, err)
	}

	node.mu.Lock()
	delete(node.mounted, "/srv/wildstone/nas/media")
	node.mu.Unlock()
	back, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil || !back.Applied || back.PendingApproval || node.ensured != 2 {
		t.Fatalf("dropped share not re-mounted unattended: %+v ensured=%d err=%v", back, node.ensured, err)
	}
}

// TestNetworkMountHealthFailsIfSomethingElseIsThere: EnsureMount says
// "already mounted" for anything at the target; health must check it is
// actually the share.
func TestNetworkMountHealthFailsIfSomethingElseIsThere(t *testing.T) {
	node := &shareNode{mounted: map[string]liveMount{"/srv/wildstone/nas/media": {fstype: "ext4", source: "/dev/sdb1"}}}
	r := newReconciler(node)
	// Pretend EnsureMount left the foreign mount in place.
	node2 := &foreignMount{shareNode: node}
	r.Applier = node2
	out, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{mediaShare()})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Steps) != 1 || out.Steps[0].HealthOK {
		t.Fatalf("a non-NFS filesystem at the target passed health: %+v", out.Steps)
	}
}

type foreignMount struct{ *shareNode }

func (f *foreignMount) Apply(ctx context.Context, n string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	if op.GetEnsureMount() != nil {
		return &wsv1.OpResult{Result: &wsv1.OpResult_EnsureMount{EnsureMount: &wsv1.EnsureMountResult{}}}, nil
	}
	return f.shareNode.Apply(ctx, n, op)
}

// TestNetworkMountOldAgent: without DiskSpace the managed cache stands
// in, so the node still converges instead of re-mounting every tick.
func TestNetworkMountOldAgent(t *testing.T) {
	node := &shareNode{mounted: map[string]liveMount{}, tooOld: true}
	r := newReconciler(node)
	desired := []resource.Resource{mediaShare()}
	if out, err := r.Reconcile(context.Background(), "node-a", desired); err != nil || !out.Applied {
		t.Fatalf("first: %+v %v", out, err)
	}
	again, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil || len(again.Plan.Steps) != 0 {
		t.Fatalf("old agent re-planned the mount: %+v %v", again.Plan.Steps, err)
	}
}

// TestNetworkMountChangeIsPlanned: an approved change of source or of an
// option is observed as drift and planned as an Update — not taken as
// done because some NFS share is mounted there.
func TestNetworkMountChangeIsPlanned(t *testing.T) {
	for name, change := range map[string]func(*resource.NetworkMountSpec){
		"source": func(m *resource.NetworkMountSpec) { m.Source = "nas.example.internal:/media2" },
		"option": func(m *resource.NetworkMountSpec) { m.Options = []string{"ro"} },
	} {
		node := &shareNode{mounted: map[string]liveMount{}, sticky: true}
		r := newReconciler(node)
		r.Approve = func(*plan.Plan) bool { return true }
		if out, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{mediaShare()}); err != nil || !out.Applied {
			t.Fatalf("%s: first mount: %+v %v", name, out, err)
		}
		changed := mediaShare()
		change(changed.NetworkMount)
		out, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{changed})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Plan.Steps) != 1 || out.Plan.Steps[0].Action != plan.ActionUpdate {
			t.Fatalf("%s change: steps = %+v, want one Update", name, out.Plan.Steps)
		}
		// The real agent leaves the old mount live, and health says so.
		if len(out.Steps) != 1 || out.Steps[0].HealthOK || !strings.Contains(out.Steps[0].Error, "next reboot or remount") {
			t.Fatalf("%s change: step = %+v, want the honest still-mounted error", name, out.Steps)
		}
	}
}

// TestHasOptions: flags must be live as written; a key=value matches the
// value the kernel negotiated from it.
func TestHasOptions(t *testing.T) {
	live := []string{"rw", "nosuid", "vers=4.2", "soft"}
	for _, tc := range []struct {
		declared []string
		want     bool
	}{
		{nil, true},
		{[]string{"soft"}, true},
		{[]string{"vers=4"}, true},
		{[]string{"vers=3"}, false},
		{[]string{"ro"}, false},
		{[]string{"soft", "ro"}, false},
	} {
		if got := hasOptions(live, tc.declared); got != tc.want {
			t.Errorf("hasOptions(%v) = %v, want %v", tc.declared, got, tc.want)
		}
	}
}
