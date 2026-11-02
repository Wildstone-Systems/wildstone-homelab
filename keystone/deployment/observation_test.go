package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

type fakeFlux struct {
	objects map[string]*kubeapi.FluxObservation
	fail    string
	calls   int
	change  bool
}

func (f *fakeFlux) ObserveFlux(_ context.Context, kind, namespace, _ string) (*kubeapi.FluxObservation, error) {
	if kind == "GitRepository" && namespace == "example-app" {
		kind = "ChartSource"
	}
	f.calls++
	if kind == f.fail {
		return nil, fmt.Errorf("private credential and host must never reach API")
	}
	b, _ := json.Marshal(f.objects[kind])
	var out kubeapi.FluxObservation
	_ = json.Unmarshal(b, &out)
	if f.change && f.calls > 5 {
		out.Metadata.ResourceVersion = "changed"
	}
	return &out, nil
}
func observationFixture(t *testing.T) (*Observer, *fakeFlux, *runtimedb.DeploymentReview, runtimedb.DeploymentStatus) {
	t.Helper()
	f := newFixture(t)
	rec := propose(t, f, "UTC")
	doc, err := runtimedb.DecodeDeploymentPlan(rec.PlanJSON)
	if err != nil {
		t.Fatal(err)
	}
	r := doc.Deployment.Review
	sha := strings.Repeat("a", 40)
	out := runtimedb.DeploymentStatus{Phase: "promoted", PromotedRevision: sha}
	makeObject := func() *kubeapi.FluxObservation {
		obj := &kubeapi.FluxObservation{Metadata: kubeapi.ObjectMeta{Name: r.Release, Namespace: r.Namespace, UID: "example-uid", ResourceVersion: "1", Generation: 2}, Spec: map[string]any{}}
		obj.Status.ObservedGeneration = 2
		obj.Status.Conditions = []kubeapi.FluxCondition{{Type: "Ready", Status: "True", ObservedGeneration: 2}}
		return obj
	}
	source, k, h, d, chart := makeObject(), makeObject(), makeObject(), makeObject(), makeObject()
	source.Spec = map[string]any{"ref": map[string]any{"branch": "deploy"}}
	raw := `{"revision":"deploy@sha1:` + sha + `"}`
	_ = json.Unmarshal([]byte(raw), &source.Status.Artifact)
	k.Spec = map[string]any{"path": "./", "sourceRef": map[string]any{"kind": "GitRepository", "name": "example-source", "namespace": "example-flux"}}
	k.Status.LastAppliedRevision = "deploy@sha1:" + sha
	h.Spec = expectedRelease(r)
	chart.Spec = map[string]any{"url": r.ChartSource, "ref": map[string]any{"commit": r.ChartCommit}}
	_ = json.Unmarshal([]byte(`{"revision":"sha1:`+r.ChartCommit+`"}`), &chart.Status.Artifact)
	d.Metadata.Labels = map[string]string{"app.kubernetes.io/instance": r.Release}
	d.Spec = map[string]any{"replicas": float64(1), "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"image": r.Image, "env": []any{map[string]any{"name": "TZ", "value": r.Timezone}}}}}}}
	d.Status.Replicas = 1
	d.Status.UpdatedReplicas = 1
	d.Status.ReadyReplicas = 1
	d.Status.AvailableReplicas = 1
	fake := &fakeFlux{objects: map[string]*kubeapi.FluxObservation{"GitRepository": source, "Kustomization": k, "HelmRelease": h, "Deployment": d, "ChartSource": chart}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/healthcheck" {
			t.Errorf("unexpected health request")
		}
		fmt.Fprint(w, "OK")
	}))
	t.Cleanup(server.Close)
	o := &Observer{reader: fake, config: ObservationConfig{SourceNamespace: "example-flux", SourceName: "example-source", KustomizationNamespace: "example-flux", KustomizationName: "example-app", HealthURL: server.URL + "/healthcheck"}, health: server.Client()}
	return o, fake, r, out
}
func TestObservationExactRevisionAndCurrentGenerations(t *testing.T) {
	cases := []struct {
		name                        string
		mutate                      func(*Observer, *fakeFlux, *runtimedb.DeploymentStatus)
		reconciliation, application string
	}{
		{"ready", func(*Observer, *fakeFlux, *runtimedb.DeploymentStatus) {}, "ready", "ready"},
		{"no-health-probe", func(o *Observer, _ *fakeFlux, _ *runtimedb.DeploymentStatus) { o.config.HealthURL = "" }, "ready", "workload_ready"},
		{"old-source", func(_ *Observer, _ *fakeFlux, s *runtimedb.DeploymentStatus) {
			s.PromotedRevision = strings.Repeat("b", 40)
		}, "reconciling", "unobserved"},
		{"old-applied", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			f.objects["Kustomization"].Status.LastAppliedRevision = "deploy@sha1:" + strings.Repeat("b", 40)
		}, "reconciling", "unobserved"},
		{"old-helm-generation", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			f.objects["HelmRelease"].Status.ObservedGeneration = 1
		}, "reconciling", "unobserved"},
		{"old-ready-condition", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			f.objects["HelmRelease"].Status.Conditions[0].ObservedGeneration = 1
		}, "unknown", "unobserved"},
		{"failed-helm", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			f.objects["HelmRelease"].Status.Conditions[0].Status = "False"
		}, "failed", "unobserved"},
		{"suspended-source", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			f.objects["GitRepository"].Spec["suspend"] = true
		}, "dependency_unavailable", "unobserved"},
		{"source-commit-override", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			f.objects["GitRepository"].Spec["ref"].(map[string]any)["commit"] = "different"
		}, "dependency_unavailable", "unobserved"},
		{"wrong-kustomization-binding", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			f.objects["Kustomization"].Spec["sourceRef"].(map[string]any)["name"] = "unrelated"
		}, "unknown", "unobserved"},
		{"wrong-chart-source", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			f.objects["ChartSource"].Spec["url"] = "https://unrelated.invalid"
		}, "unknown", "unobserved"},
		{"old-chart-revision", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			f.objects["ChartSource"].Status.Artifact.Revision = "sha1:" + strings.Repeat("b", 40)
		}, "reconciling", "unobserved"},
		{"deleting-workload", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			stamp := "2026-01-01T00:00:00Z"
			f.objects["Deployment"].Metadata.DeletionTimestamp = &stamp
		}, "ready", "dependency_unavailable"},
		{"failed-rollout", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			f.objects["Deployment"].Status.Conditions = []kubeapi.FluxCondition{{Type: "Progressing", Status: "False", Reason: "ProgressDeadlineExceeded"}}
		}, "ready", "failed"},
		{"changed-live-values", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			f.objects["HelmRelease"].Spec["values"].(map[string]any)["replicaCount"] = float64(3)
		}, "unknown", "unobserved"},
		{"missing-dependency", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) { f.fail = "GitRepository" }, "dependency_unavailable", "unobserved"},
		{"old-workload-generation", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			f.objects["Deployment"].Status.ObservedGeneration = 1
		}, "ready", "reconciling"},
		{"unready-workload", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			f.objects["Deployment"].Status.AvailableReplicas = 0
		}, "ready", "reconciling"},
		{"changed-workload-image", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) {
			nested(f.objects["Deployment"].Spec, "template", "spec", "containers").([]any)[0].(map[string]any)["image"] = "unrelated"
		}, "ready", "reconciling"},
		{"torn-snapshot", func(_ *Observer, f *fakeFlux, _ *runtimedb.DeploymentStatus) { f.change = true }, "unknown", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, f, r, s := observationFixture(t)
			tc.mutate(o, f, &s)
			o.observe(r, &s)
			if s.Reconciliation != tc.reconciliation || s.Application != tc.application {
				t.Fatalf("got %s/%s: %+v", s.Reconciliation, s.Application, s)
			}
			if s.ControllerObservedAt == nil || s.ControllerExpiresAt == nil || !s.ControllerExpiresAt.After(*s.ControllerObservedAt) {
				t.Fatal("missing freshness bounds")
			}
			b, _ := json.Marshal(s)
			if strings.Contains(string(b), "private credential") {
				t.Fatal("raw diagnostic leak")
			}
		})
	}
}
func TestObservationFailureThenRecoveryAndNoCachedReadiness(t *testing.T) {
	o, f, r, s := observationFixture(t)
	o.observe(r, &s)
	if s.Application != "ready" {
		t.Fatal(s)
	}
	f.fail = "Kustomization"
	o.observe(r, &s)
	if s.Application == "ready" || s.Reconciliation != "dependency_unavailable" {
		t.Fatal("retained success", s)
	}
	f.fail = ""
	o.observe(r, &s)
	if s.Application != "ready" {
		t.Fatal("did not recover", s)
	}
}
func TestObservationHealthFailureCannotBecomeReady(t *testing.T) {
	for _, status := range []int{302, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			o, _, r, s := observationFixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			o.config.HealthURL = server.URL + "/healthcheck"
			o.observe(r, &s)
			if s.Application != "failed" {
				t.Fatal(s)
			}
		})
	}
}
func TestHealthDestinationsAreLocalAndClosed(t *testing.T) {
	for _, raw := range []string{"https://example.invalid/healthcheck", "http://8.8.8.8/healthcheck", "http://127.0.0.1/other", "http://user:pass@localhost/healthcheck", "http://localhost/healthcheck?token=private", "http://localhost/healthcheck#fragment"} {
		if localHealthURL(raw) {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:13378/healthcheck", "http://example-app.example-app.svc:13378/healthcheck"} {
		if !localHealthURL(raw) {
			t.Errorf("refused %s", raw)
		}
	}
}

func TestPromotedPlanStatusDoesNotBorrowPriorApplicationReadiness(t *testing.T) {
	f := newFixture(t)
	rec := propose(t, f, "UTC")
	approve(t, f, rec.ID)
	f.m.Resume()
	local := f.m.Status(rec.ID)
	if local.Phase != "promoted" {
		t.Fatal(local)
	}
	o, reader, _, _ := observationFixture(t)
	reader.objects["GitRepository"].Status.Artifact.Revision = "deploy@sha1:" + local.PromotedRevision
	reader.objects["Kustomization"].Status.LastAppliedRevision = "deploy@sha1:" + local.PromotedRevision
	f.m.observer = o
	current := f.m.Status(rec.ID)
	if current.Application != "ready" || current.PromotedRevision != local.PromotedRevision {
		t.Fatal(current)
	}
	next := propose(t, f, "Etc/UTC")
	approve(t, f, next.ID)
	f.m.Resume()
	pending := f.m.Status(next.ID)
	if pending.Phase != "promoted" || pending.Reconciliation != "reconciling" || pending.Application != "unobserved" {
		t.Fatal("new approval borrowed old readiness", pending)
	}
	old := f.m.Status(rec.ID)
	if old.Phase != "diverged" || old.Application != "unobserved" {
		t.Fatal("old Plan presented as live", old)
	}
}
