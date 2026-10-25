package leader

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/clusterauthority"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// TestAgainstARealAPIServer runs election, Keystone's epoch and the
// barrier against a real, disposable Kubernetes API server that already
// has the engine add-ons applied (deploy/ansible/engine-addons.yml). It
// is skipped unless WILDSTONE_KUBE_TEST_SERVER and an administrator token
// in WILDSTONE_KUBE_TEST_TOKEN are set; it acts as each identity by
// impersonation, like scripts/engine-fence-check.sh. Never point it at a
// cluster you care about.
//
//	WILDSTONE_KUBE_TEST_SERVER=https://127.0.0.1:6443 \
//	WILDSTONE_KUBE_TEST_TOKEN=... WILDSTONE_KUBE_TEST_INSECURE=1 \
//	go test -run TestAgainstARealAPIServer ./internal/leader
func TestAgainstARealAPIServer(t *testing.T) {
	server, token := os.Getenv("WILDSTONE_KUBE_TEST_SERVER"), os.Getenv("WILDSTONE_KUBE_TEST_TOKEN")
	if server == "" || token == "" {
		t.Skip("set WILDSTONE_KUBE_TEST_SERVER and WILDSTONE_KUBE_TEST_TOKEN to run against a disposable API server")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: os.Getenv("WILDSTONE_KUBE_TEST_INSECURE") == "1"}} //nolint:gosec // disposable test server only
	as := func(user string) *kubeapi.Client {
		hc := &http.Client{Timeout: 5 * time.Second, Transport: impersonate{tr, user}}
		c, err := kubeapi.NewWithHTTP(server, func() (string, error) { return token, nil }, hc)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	admin := func(method, path, body string) {
		t.Helper()
		req, _ := http.NewRequest(method, server+path, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// node-b exists only for this test: an account and both bindings.
	const sa = "system:serviceaccount:wildstone-system:"
	idA, idB := sa+"wardstone-node-a", sa+"wardstone-leader-test"
	for _, rb := range []struct{ ns, role string }{{"wildstone-apps", "wardstone-runtime"}, {"wildstone-system", "wardstone-coordination"}} {
		admin(http.MethodPost, "/apis/rbac.authorization.k8s.io/v1/namespaces/"+rb.ns+"/rolebindings", fmt.Sprintf(
			`{"metadata":{"name":"wardstone-leader-test"},"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"Role","name":%q},"subjects":[{"kind":"ServiceAccount","name":"wardstone-leader-test","namespace":"wildstone-system"}]}`, rb.role))
	}
	keystone := as(sa + "keystone")
	before, err := keystone.GetConfigMap(context.Background(), clusterauthority.Namespace, clusterauthority.Name)
	if err != nil {
		t.Fatalf("reading the authority record (are the add-ons applied?): %v", err)
	}
	t.Cleanup(func() {
		admin(http.MethodDelete, "/apis/rbac.authorization.k8s.io/v1/namespaces/wildstone-apps/rolebindings/wardstone-leader-test", "")
		admin(http.MethodDelete, "/apis/rbac.authorization.k8s.io/v1/namespaces/wildstone-system/rolebindings/wardstone-leader-test", "")
		admin(http.MethodDelete, "/apis/coordination.k8s.io/v1/namespaces/wildstone-system/leases/"+LeaseName, "")
		cur, err := keystone.GetConfigMap(context.Background(), clusterauthority.Namespace, clusterauthority.Name)
		if err == nil {
			cur.Data = before.Data
			_, _ = keystone.UpdateConfigMap(context.Background(), cur)
		}
	})

	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	authority := clusterauthority.Authority{Store: db, Records: keystone, Leases: keystone}

	run := func(id string) (context.CancelFunc, <-chan Term, chan struct{}) {
		c := as(id)
		terms := make(chan Term, 1)
		stopped := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		l := Leader{
			Elector: &Elector{Client: c, Identity: id,
				LeaseDuration: 3 * time.Second, RenewDeadline: 2 * time.Second, RetryPeriod: 200 * time.Millisecond},
			Issuer:   authority,
			Barrier:  Barrier{Servers: []Prober{c}, Timeout: 10 * time.Second},
			Serve:    func(ctx context.Context, term Term) error { terms <- term; <-ctx.Done(); return nil },
			Incident: func(err error) { t.Errorf("%s: incident: %v", id, err) },
		}
		go func() { defer close(stopped); _ = l.Run(ctx) }()
		return cancel, terms, stopped
	}
	probe := func(id string, epoch uint64) error {
		return as(id).DryRunCreateConfigMap(context.Background(), &kubeapi.ConfigMap{Metadata: kubeapi.ObjectMeta{
			Namespace: ProbeNamespace, GenerateName: "leader-test-",
			Annotations: map[string]string{epochAnnotation: strconv.FormatUint(epoch, 10)},
		}})
	}
	wait := func(terms <-chan Term) Term {
		select {
		case term := <-terms:
			return term
		case <-time.After(20 * time.Second):
			t.Fatal("no term started")
			return Term{}
		}
	}

	stopA, termsA, stoppedA := run(idA)
	ta := wait(termsA)
	if err := probe(idA, ta.Epoch); err != nil {
		t.Fatalf("A in its own term %d: %v", ta.Epoch, err)
	}
	stopA()
	<-stoppedA

	stopB, termsB, stoppedB := run(idB)
	defer func() { stopB(); <-stoppedB }()
	tb := wait(termsB)
	if tb.Epoch <= ta.Epoch {
		t.Fatalf("B's epoch %d is not after A's %d", tb.Epoch, ta.Epoch)
	}
	if err := probe(idB, tb.Epoch); err != nil {
		t.Fatalf("B in its own term: %v", err)
	}
	for _, e := range []uint64{ta.Epoch, tb.Epoch} {
		if err := probe(idA, e); !kubeapi.IsForbidden(err) {
			t.Fatalf("old leader A writing in epoch %d: %v, want refused", e, err)
		}
	}
	if err := probe(idB, ta.Epoch); !kubeapi.IsForbidden(err) {
		t.Fatalf("B writing in A's old epoch: %v, want refused", err)
	}
}

type impersonate struct {
	rt   http.RoundTripper
	user string
}

func (i impersonate) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Impersonate-User", i.user)
	return i.rt.RoundTrip(r)
}
