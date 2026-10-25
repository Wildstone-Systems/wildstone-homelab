package clusterauthority

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi/kubeapitest"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

const (
	nodeA = "system:serviceaccount:wildstone-system:wardstone-node-a"
	nodeB = "system:serviceaccount:wildstone-system:wardstone-node-b"
)

func setup(t *testing.T, epoch string) (*kubeapitest.Server, Authority) {
	t.Helper()
	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := kubeapitest.New()
	t.Cleanup(srv.Close)
	if epoch != "" {
		srv.PutConfigMap(kubeapi.ConfigMap{
			Metadata: kubeapi.ObjectMeta{Namespace: Namespace, Name: Name},
			Data:     map[string]string{"epoch": epoch, "leader": ""},
		})
	}
	c := srv.Client("system:serviceaccount:wildstone-system:keystone")
	return srv, Authority{Store: db, Records: c, Leases: c}
}

// hold makes who the Lease holder, as winning the election does.
func hold(srv *kubeapitest.Server, who string) {
	srv.PutLease(kubeapi.Lease{
		Metadata: kubeapi.ObjectMeta{Namespace: LeaseNamespace, Name: LeaseName},
		Spec:     kubeapi.LeaseSpec{HolderIdentity: who, LeaseDurationSeconds: 15},
	})
}

func record(t *testing.T, srv *kubeapitest.Server) map[string]string {
	t.Helper()
	cm := srv.ConfigMap(Namespace, Name)
	if cm == nil {
		t.Fatal("authority record missing")
	}
	return cm.Data
}

func TestIssueMovesTheRecordForward(t *testing.T) {
	srv, a := setup(t, "0")
	ctx := context.Background()
	for i, who := range []string{nodeA, nodeB, nodeA} {
		hold(srv, who)
		e, err := a.Issue(ctx, who)
		if err != nil || e != uint64(i+1) {
			t.Fatalf("issue %d = %d, %v", i+1, e, err)
		}
		if got := record(t, srv); got["leader"] != who || got["epoch"] != []string{"1", "2", "3"}[i] {
			t.Fatalf("record after issue %d = %v", i+1, got)
		}
	}
}

func TestIssueRefusesAnythingButARuntimeIdentity(t *testing.T) {
	srv, a := setup(t, "0")
	hold(srv, "system:serviceaccount:wildstone-system:keystone")
	for _, who := range []string{"", "system:serviceaccount:wildstone-system:keystone", "admin", "system:serviceaccount:kube-system:wardstone-x", nodeA + "\n"} {
		if _, err := a.Issue(context.Background(), who); err == nil {
			t.Errorf("Issue(%q) was accepted", who)
		}
	}
	if got := record(t, srv); got["epoch"] != "0" {
		t.Fatalf("a refused issue moved the record: %v", got)
	}
}

// A Keystone database restored from an older backup is behind the
// cluster. The next epoch must still be above the record's.
func TestIssueNeverReusesAnEpochTheClusterHolds(t *testing.T) {
	srv, a := setup(t, "41")
	hold(srv, nodeA)
	e, err := a.Issue(context.Background(), nodeA)
	if err != nil || e != 42 {
		t.Fatalf("issue = %d, %v; want 42", e, err)
	}
	if got := record(t, srv); got["epoch"] != "42" {
		t.Fatalf("record = %v", got)
	}
}

func TestIssueRefusesAMissingOrBrokenRecord(t *testing.T) {
	srv, a := setup(t, "")
	hold(srv, nodeA)
	if _, err := a.Issue(context.Background(), nodeA); !errors.Is(err, ErrBadRecord) {
		t.Fatalf("missing record: err = %v, want ErrBadRecord", err)
	}
	srv, a = setup(t, "not-a-number")
	hold(srv, nodeA)
	if _, err := a.Issue(context.Background(), nodeA); !errors.Is(err, ErrBadRecord) {
		t.Fatalf("broken record: err = %v, want ErrBadRecord", err)
	}
	if got := record(t, srv); got["epoch"] != "not-a-number" {
		t.Fatalf("a broken record was overwritten: %v", got)
	}
}

// racing is a Records whose record moves under the first write, as when
// another Keystone or an administrator writes it between read and update.
type racing struct {
	Records
	srv   *kubeapitest.Server
	raced bool
}

func (r *racing) UpdateConfigMap(ctx context.Context, cm *kubeapi.ConfigMap) (*kubeapi.ConfigMap, error) {
	if !r.raced {
		r.raced = true
		r.srv.PutConfigMap(kubeapi.ConfigMap{
			Metadata: kubeapi.ObjectMeta{Namespace: Namespace, Name: Name},
			Data:     map[string]string{"epoch": "7", "leader": nodeB},
		})
	}
	return r.Records.UpdateConfigMap(ctx, cm)
}

func TestIssueRereadsWhenTheRecordMoves(t *testing.T) {
	srv, a := setup(t, "0")
	hold(srv, nodeA)
	a.Records = &racing{Records: a.Records, srv: srv}
	e, err := a.Issue(context.Background(), nodeA)
	if err != nil {
		t.Fatal(err)
	}
	// The first epoch (1) was burnt by the conflict; the retry read epoch
	// 7 and issued above it.
	if e != 8 {
		t.Fatalf("epoch = %d, want 8", e)
	}
	if got := record(t, srv); got["epoch"] != "8" || got["leader"] != nodeA {
		t.Fatalf("record = %v", got)
	}
}

func TestIssueOnlyToTheLeaseHolder(t *testing.T) {
	srv, a := setup(t, "3")
	ctx := context.Background()
	if _, err := a.Issue(ctx, nodeA); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("no Lease yet: err = %v, want ErrNotHolder", err)
	}
	hold(srv, nodeB)
	if _, err := a.Issue(ctx, nodeA); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("A asking while B holds: err = %v, want ErrNotHolder", err)
	}
	if got := record(t, srv); got["epoch"] != "3" {
		t.Fatalf("a refused issue moved the record: %v", got)
	}
	if e, err := a.Issue(ctx, nodeB); err != nil || e != 4 {
		t.Fatalf("the holder's issue = %d, %v", e, err)
	}
}

// stolen is a Records where, between A's read and A's write, B wins the
// Lease and publishes its own epoch (Codex review on #192).
type stolen struct {
	Records
	srv   *kubeapitest.Server
	raced bool
}

func (r *stolen) UpdateConfigMap(ctx context.Context, cm *kubeapi.ConfigMap) (*kubeapi.ConfigMap, error) {
	if !r.raced {
		r.raced = true
		hold(r.srv, nodeB)
		r.srv.PutConfigMap(kubeapi.ConfigMap{
			Metadata: kubeapi.ObjectMeta{Namespace: Namespace, Name: Name},
			Data:     map[string]string{"epoch": "2", "leader": nodeB},
		})
	}
	return r.Records.UpdateConfigMap(ctx, cm)
}

func TestIssueRefusesALeaderThatLostTheLeaseMidWrite(t *testing.T) {
	srv, a := setup(t, "0")
	hold(srv, nodeA)
	a.Records = &stolen{Records: a.Records, srv: srv}
	if e, err := a.Issue(context.Background(), nodeA); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("A after losing the Lease: epoch %d, err %v; want ErrNotHolder", e, err)
	}
	if got := record(t, srv); got["epoch"] != "2" || got["leader"] != nodeB {
		t.Fatalf("the winner was fenced out: record %v", got)
	}
}
