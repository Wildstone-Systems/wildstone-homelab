package leader

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type workloadFake struct {
	d                                       *kubeapi.Deployment
	s                                       *kubeapi.Service
	pods                                    kubeapi.PodList
	logs                                    map[string][]byte
	rv                                      int
	deleteDeploymentUID, deleteDeploymentRV string
	deleteServiceUID, deleteServiceRV       string
	applyStarted                            chan struct{}
	failServiceUpdate                       bool
	failDeploymentDelete                    bool
	beforeDeploymentApply                   func()
	beforeDeploymentCreate                  func()
	beforeServiceCreate                     func()
}

func notFound() error {
	return &kubeapi.StatusError{Code: 404, Reason: "NotFound", Message: "not found"}
}
func (f *workloadFake) next(m *kubeapi.ObjectMeta, uid string) {
	f.rv++
	m.ResourceVersion = string(rune('0' + f.rv))
	if m.UID == "" {
		m.UID = uid
	}
	m.Generation++
}
func clone[T any](v *T) *T {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var c T
	if err := json.Unmarshal(b, &c); err != nil {
		panic(err)
	}
	return &c
}
func (f *workloadFake) GetDeployment(context.Context, string) (*kubeapi.Deployment, error) {
	if f.d == nil {
		return nil, notFound()
	}
	return clone(f.d), nil
}
func (f *workloadFake) waitForWrite(ctx context.Context) error {
	if f.applyStarted == nil {
		return nil
	}
	close(f.applyStarted)
	<-ctx.Done()
	return ctx.Err()
}
func (f *workloadFake) CreateDeployment(ctx context.Context, d *kubeapi.Deployment) (*kubeapi.Deployment, error) {
	if err := f.waitForWrite(ctx); err != nil {
		return nil, err
	}
	if f.beforeDeploymentCreate != nil {
		f.beforeDeploymentCreate()
		f.beforeDeploymentCreate = nil
	}
	if f.d != nil {
		return nil, &kubeapi.StatusError{Code: 409, Reason: "AlreadyExists"}
	}
	d.APIVersion, d.Kind, d.Metadata.Namespace = "apps/v1", "Deployment", kubeapi.WorkloadNamespace
	f.next(&d.Metadata, "deployment-uid")
	f.d = clone(d)
	return clone(d), nil
}
func (f *workloadFake) ApplyDeployment(ctx context.Context, d *kubeapi.Deployment) (*kubeapi.Deployment, error) {
	if err := f.waitForWrite(ctx); err != nil {
		return nil, err
	}
	if f.beforeDeploymentApply != nil {
		f.beforeDeploymentApply()
		f.beforeDeploymentApply = nil
	}
	if f.d == nil || d.Metadata.ResourceVersion == "" || d.Metadata.ResourceVersion != f.d.Metadata.ResourceVersion {
		return nil, &kubeapi.StatusError{Code: 409, Reason: "Conflict"}
	}
	d.APIVersion, d.Kind, d.Metadata.Namespace = "apps/v1", "Deployment", kubeapi.WorkloadNamespace
	f.next(&d.Metadata, "deployment-uid")
	f.d = clone(d)
	return clone(d), nil
}
func (f *workloadFake) UpdateDeployment(_ context.Context, d *kubeapi.Deployment) (*kubeapi.Deployment, error) {
	f.next(&d.Metadata, "deployment-uid")
	f.d = clone(d)
	return clone(d), nil
}
func (f *workloadFake) DeleteDeployment(_ context.Context, _ string, uid, rv string) error {
	f.deleteDeploymentUID, f.deleteDeploymentRV = uid, rv
	if f.d == nil || f.d.Metadata.UID != uid || f.d.Metadata.ResourceVersion != rv {
		return errors.New("bad deployment precondition")
	}
	if f.failDeploymentDelete {
		f.failDeploymentDelete = false
		return errors.New("transient Deployment delete")
	}
	f.d = nil
	return nil
}
func (f *workloadFake) GetService(context.Context, string) (*kubeapi.Service, error) {
	if f.s == nil {
		return nil, notFound()
	}
	return clone(f.s), nil
}
func (f *workloadFake) CreateService(_ context.Context, s *kubeapi.Service) (*kubeapi.Service, error) {
	if f.beforeServiceCreate != nil {
		f.beforeServiceCreate()
		f.beforeServiceCreate = nil
	}
	if f.s != nil {
		return nil, &kubeapi.StatusError{Code: 409, Reason: "AlreadyExists"}
	}
	s.APIVersion, s.Kind, s.Metadata.Namespace = "v1", "Service", kubeapi.WorkloadNamespace
	f.next(&s.Metadata, "service-uid")
	f.s = clone(s)
	return clone(s), nil
}
func (f *workloadFake) ApplyService(_ context.Context, s *kubeapi.Service) (*kubeapi.Service, error) {
	if f.s == nil || s.Metadata.ResourceVersion == "" || s.Metadata.ResourceVersion != f.s.Metadata.ResourceVersion {
		return nil, &kubeapi.StatusError{Code: 409, Reason: "Conflict"}
	}
	s.APIVersion, s.Kind, s.Metadata.Namespace = "v1", "Service", kubeapi.WorkloadNamespace
	f.next(&s.Metadata, "service-uid")
	f.s = clone(s)
	return clone(s), nil
}
func (f *workloadFake) UpdateService(_ context.Context, s *kubeapi.Service) (*kubeapi.Service, error) {
	if f.failServiceUpdate {
		f.failServiceUpdate = false
		return nil, errors.New("transient Service update")
	}
	f.next(&s.Metadata, "service-uid")
	f.s = clone(s)
	return clone(s), nil
}
func (f *workloadFake) DeleteService(_ context.Context, _ string, uid, rv string) error {
	f.deleteServiceUID, f.deleteServiceRV = uid, rv
	if f.s == nil || f.s.Metadata.UID != uid || f.s.Metadata.ResourceVersion != rv {
		return errors.New("bad service precondition")
	}
	f.s = nil
	return nil
}
func (f *workloadFake) ListPods(context.Context, string) (*kubeapi.PodList, error) {
	return &f.pods, nil
}
func (f *workloadFake) PodLogs(_ context.Context, pod string, _ uint32, _ int64, limit int64) ([]byte, bool, error) {
	b := f.logs[pod]
	if int64(len(b)) > limit {
		return b[len(b)-int(limit):], true, nil
	}
	return b, false, nil
}

func authority(epoch uint64, revision string) *wsv1.WorkloadAuthority {
	return &wsv1.WorkloadAuthority{AcceptedRevision: revision, PlanId: "plan-1", StepDigest: "step-1", LeaderEpoch: epoch}
}
func workload() *wsv1.WorkloadSpec {
	return &wsv1.WorkloadSpec{Name: "tools-it-tools", Package: "tools-it-tools", Resource: "web", Image: "registry.example.internal/tools@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Command: []string{"serve", "--port=8080"}, Replicas: 1, Strategy: wsv1.WorkloadStrategy_WORKLOAD_STRATEGY_RECREATE, Env: map[string]string{"B": "2", "A": "1"}, Ports: []*wsv1.WorkloadPort{{Name: "http", ContainerPort: 8080, ServicePort: 8080, Protocol: wsv1.PortProtocol_PORT_PROTOCOL_TCP}}, Security: &wsv1.WorkloadSecurity{ReadOnlyRootfs: true, NoNewPrivileges: true, CapDrop: []string{"ALL"}}}
}

func TestGeneratedWorkloadArmsRoundTripWithoutRawPayload(t *testing.T) {
	operations := []*wsv1.Operation{
		{Op: &wsv1.Operation_ApplyWorkload{ApplyWorkload: &wsv1.ApplyWorkload{Authority: authority(7, "rev-a"), Workload: workload(), ExpectedRevision: "rev-before"}}},
		{Op: &wsv1.Operation_DeleteWorkload{DeleteWorkload: &wsv1.DeleteWorkload{Authority: authority(7, "rev-b"), Name: "tools-it-tools", Uid: "deployment-uid", ResourceVersion: "12", ServiceUid: "service-uid", ServiceResourceVersion: "13"}}},
		{Op: &wsv1.Operation_ObserveWorkload{ObserveWorkload: &wsv1.ObserveWorkload{Name: "tools-it-tools"}}},
		{Op: &wsv1.Operation_WorkloadLogs{WorkloadLogs: &wsv1.WorkloadLogs{Name: "tools-it-tools", Tail: 25, SinceUnixSec: 10, LimitBytes: 4096}}},
	}
	for _, in := range operations {
		b, err := proto.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		var out wsv1.Operation
		if err := proto.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(in, &out) {
			t.Fatalf("%T did not round-trip", in.GetOp())
		}
	}
	fields := workload().ProtoReflect().Descriptor().Fields()
	for _, name := range []string{"manifest", "yaml", "json", "patch", "command_string", "helm_values"} {
		if fields.ByName(protoreflect.Name(name)) != nil {
			t.Fatalf("raw payload field %q exists", name)
		}
	}
}

func TestApplyIsFixedIdempotentAndRevisionFenced(t *testing.T) {
	f := &workloadFake{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x := WorkloadExecutor{Client: f, Epoch: 7, Context: ctx}
	req := &wsv1.ApplyWorkload{Authority: authority(7, "rev-a"), Workload: workload()}
	first, err := x.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.GetUid() == "" || first.GetServiceUid() == "" {
		t.Fatalf("missing object identities: %+v", first)
	}
	if f.d.Kind != "Deployment" || f.d.Spec.Template.Spec.Containers[0].Image != req.Workload.Image {
		t.Fatalf("unexpected fixed deployment: %+v", f.d)
	}
	if len(f.d.Spec.Template.Spec.Containers[0].Command) != 0 || !reflect.DeepEqual(f.d.Spec.Template.Spec.Containers[0].Args, req.Workload.Command) {
		t.Fatalf("container arguments were not preserved: %+v", f.d.Spec.Template.Spec.Containers[0])
	}
	if f.d.Metadata.Labels[managedByLabel] != managedByValue || f.d.Metadata.Annotations[revisionAnnotation] != "rev-a" {
		t.Fatalf("ownership/authority not stamped: %+v", f.d.Metadata)
	}
	if got := []string{f.d.Spec.Template.Spec.Containers[0].Env[0].Name, f.d.Spec.Template.Spec.Containers[0].Env[1].Name}; !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Fatalf("env order %v", got)
	}
	if _, err := x.Apply(context.Background(), req); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	stale := &wsv1.ApplyWorkload{Authority: authority(7, "rev-b"), ExpectedRevision: "rev-wrong", Workload: workload()}
	if _, err := x.Apply(context.Background(), stale); !IsRefusal(err) {
		t.Fatalf("stale revision = %v", err)
	}
	stale.ExpectedRevision = "rev-a"
	if _, err := x.Apply(context.Background(), stale); err != nil {
		t.Fatalf("revision CAS update: %v", err)
	}
	f.beforeDeploymentApply = func() { f.next(&f.d.Metadata, "deployment-uid") }
	if _, err := x.Apply(context.Background(), &wsv1.ApplyWorkload{Authority: authority(7, "rev-c"), ExpectedRevision: "rev-b", Workload: workload()}); !kubeapi.IsConflict(err) {
		t.Fatalf("concurrent write escaped resourceVersion precondition: %v", err)
	}
}

func TestWritesRequireActiveExactEpochAndCancelOnLoss(t *testing.T) {
	f := &workloadFake{}
	ctx, cancel := context.WithCancel(context.Background())
	x := WorkloadExecutor{Client: f, Epoch: 9, Context: ctx}
	req := &wsv1.ApplyWorkload{Authority: authority(8, "rev-a"), Workload: workload()}
	if _, err := x.Apply(context.Background(), req); !IsRefusal(err) {
		t.Fatalf("stale epoch = %v", err)
	}
	req.Authority.LeaderEpoch = 9
	f.applyStarted = make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := x.Apply(context.Background(), req); done <- err }()
	<-f.applyStarted
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight write after leadership loss = %v", err)
	}
}

func TestApplyDoesNotOverwriteInterveningCreates(t *testing.T) {
	ctx := context.Background()
	f := &workloadFake{}
	x := WorkloadExecutor{Client: f, Epoch: 7, Context: ctx}
	f.beforeDeploymentCreate = func() {
		f.d = &kubeapi.Deployment{Metadata: kubeapi.ObjectMeta{Name: "tools-it-tools", UID: "other", ResourceVersion: "other"}}
	}
	if _, err := x.Apply(ctx, &wsv1.ApplyWorkload{Authority: authority(7, "rev-a"), Workload: workload()}); !kubeapi.IsConflict(err) {
		t.Fatalf("intervening Deployment create was overwritten: %v", err)
	}

	f = &workloadFake{}
	x.Client = f
	f.beforeServiceCreate = func() {
		f.s = &kubeapi.Service{Metadata: kubeapi.ObjectMeta{Name: "tools-it-tools", UID: "other", ResourceVersion: "other"}}
	}
	if _, err := x.Apply(ctx, &wsv1.ApplyWorkload{Authority: authority(7, "rev-a"), Workload: workload()}); !kubeapi.IsConflict(err) {
		t.Fatalf("intervening Service create was overwritten: %v", err)
	}
}

func TestDeleteStampsThenUsesBothPreconditions(t *testing.T) {
	f := &workloadFake{}
	x := WorkloadExecutor{Client: f, Epoch: 7, Context: context.Background()}
	applied, err := x.Apply(context.Background(), &wsv1.ApplyWorkload{Authority: authority(7, "rev-a"), Workload: workload()})
	if err != nil {
		t.Fatal(err)
	}
	bad := &wsv1.DeleteWorkload{Authority: authority(7, "rev-b"), Name: "tools-it-tools", Uid: applied.Uid, ResourceVersion: applied.ResourceVersion, ServiceUid: applied.ServiceUid, ServiceResourceVersion: "stale"}
	if _, err := x.Delete(context.Background(), bad); !IsRefusal(err) {
		t.Fatalf("stale service delete = %v", err)
	}
	if f.d == nil || f.s == nil {
		t.Fatal("stale delete changed objects")
	}
	bad.ServiceResourceVersion = applied.ServiceResourceVersion
	res, err := x.Delete(context.Background(), bad)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Deleted || f.d != nil || f.s != nil {
		t.Fatalf("delete result %+v", res)
	}
	if f.deleteDeploymentRV == applied.ResourceVersion || f.deleteServiceRV == applied.ServiceResourceVersion {
		t.Fatal("delete did not use stamped resourceVersions")
	}
}

func TestDeleteConvergesAnOrphanedService(t *testing.T) {
	f := &workloadFake{}
	x := WorkloadExecutor{Client: f, Epoch: 7, Context: context.Background()}
	applied, err := x.Apply(context.Background(), &wsv1.ApplyWorkload{Authority: authority(7, "rev-a"), Workload: workload()})
	if err != nil {
		t.Fatal(err)
	}
	f.d = nil // a prior partial attempt or external deletion removed only it
	res, err := x.Delete(context.Background(), &wsv1.DeleteWorkload{Authority: authority(7, "rev-b"), Name: "tools-it-tools", ServiceUid: applied.ServiceUid, ServiceResourceVersion: applied.ServiceResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Deleted || f.s != nil {
		t.Fatalf("orphaned Service was not removed: %+v", res)
	}
}

func TestDeleteResumesOnlyItsOwnUnchangedStamp(t *testing.T) {
	f := &workloadFake{}
	x := WorkloadExecutor{Client: f, Epoch: 7, Context: context.Background()}
	applied, err := x.Apply(context.Background(), &wsv1.ApplyWorkload{Authority: authority(7, "rev-a"), Workload: workload()})
	if err != nil {
		t.Fatal(err)
	}
	req := &wsv1.DeleteWorkload{Authority: authority(7, "rev-b"), Name: "tools-it-tools", Uid: applied.Uid, ResourceVersion: applied.ResourceVersion, ServiceUid: applied.ServiceUid, ServiceResourceVersion: applied.ServiceResourceVersion}
	f.failServiceUpdate = true
	if _, err := x.Delete(context.Background(), req); err == nil {
		t.Fatal("partial delete unexpectedly succeeded")
	}
	if _, err := x.Delete(context.Background(), req); err != nil {
		t.Fatalf("retry did not resume its own stamp: %v", err)
	}

	applied, err = x.Apply(context.Background(), &wsv1.ApplyWorkload{Authority: authority(7, "rev-c"), Workload: workload()})
	if err != nil {
		t.Fatal(err)
	}
	req = &wsv1.DeleteWorkload{Authority: authority(7, "rev-d"), Name: "tools-it-tools", Uid: applied.Uid, ResourceVersion: applied.ResourceVersion, ServiceUid: applied.ServiceUid, ServiceResourceVersion: applied.ServiceResourceVersion}
	f.failDeploymentDelete = true
	if _, err := x.Delete(context.Background(), req); err == nil {
		t.Fatal("delete failure was not injected")
	}
	f.d.Spec.Replicas++
	if _, err := x.Delete(context.Background(), req); !IsRefusal(err) {
		t.Fatalf("unrelated post-stamp change was accepted: %v", err)
	}
}

func TestObserveAndLogsAreOwnedAndBounded(t *testing.T) {
	f := &workloadFake{logs: map[string][]byte{"pod-b": []byte("BBBB"), "pod-a": []byte("AAAA")}}
	x := WorkloadExecutor{Client: f, Epoch: 7, Context: context.Background()}
	if _, err := x.Apply(context.Background(), &wsv1.ApplyWorkload{Authority: authority(7, "rev-a"), Workload: workload()}); err != nil {
		t.Fatal(err)
	}
	ls := labels(workload())
	f.pods.Items = []kubeapi.Pod{{Metadata: kubeapi.ObjectMeta{Name: "pod-b", Labels: ls}, Spec: kubeapi.PodSpec{NodeName: "node-b"}, Status: kubeapi.PodStatus{ContainerStatuses: []kubeapi.ContainerStatus{{RestartCount: 2, ImageID: "sha256:running"}}}}, {Metadata: kubeapi.ObjectMeta{Name: "pod-a", Labels: ls}, Spec: kubeapi.PodSpec{NodeName: "node-a"}}}
	obs, err := x.Observe(context.Background(), &wsv1.ObserveWorkload{Name: "tools-it-tools"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(obs.PodNodes, []string{"node-a", "node-b"}) || obs.RestartCount != 2 || obs.ServiceUid == "" {
		t.Fatalf("observe %+v", obs)
	}
	logs, err := x.Logs(context.Background(), &wsv1.WorkloadLogs{Name: "tools-it-tools", LimitBytes: 6})
	if err != nil {
		t.Fatal(err)
	}
	if string(logs.Logs) != "AAAA\nB" || !logs.Truncated {
		t.Fatalf("logs %q truncated=%v", logs.Logs, logs.Truncated)
	}
	if _, err := x.Logs(context.Background(), &wsv1.WorkloadLogs{Name: "tools-it-tools", LimitBytes: maxWorkloadLogBytes + 1}); !IsRefusal(err) {
		t.Fatalf("oversize logs = %v", err)
	}
}
