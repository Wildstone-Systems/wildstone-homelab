package reconcile

import (
	"context"
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
	mounted map[string]string // target -> fstype
	ensured int
	tooOld  bool
}

func (n *shareNode) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_EnsureMount:
		n.ensured++
		n.mounted[o.EnsureMount.GetTarget()] = "nfs4"
		return &wsv1.OpResult{Result: &wsv1.OpResult_EnsureMount{EnsureMount: &wsv1.EnsureMountResult{Mounted: true}}}, nil
	case *wsv1.Operation_DiskSpace:
		if n.tooOld {
			return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{Code: wsv1.ErrorCode_ERROR_CODE_INVALID}}}, nil
		}
		t := o.DiskSpace.GetPath()
		if fs, ok := n.mounted[t]; ok {
			return &wsv1.OpResult{Result: &wsv1.OpResult_DiskSpace{DiskSpace: &wsv1.DiskSpaceResult{MountPoint: t, Fstype: fs, Exists: true}}}, nil
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
	node := &shareNode{mounted: map[string]string{}}
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
	node := &shareNode{mounted: map[string]string{"/srv/wildstone/nas/media": "ext4"}}
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
	node := &shareNode{mounted: map[string]string{}, tooOld: true}
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
