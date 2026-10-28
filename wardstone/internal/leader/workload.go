package leader

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

const (
	managedByLabel        = "app.kubernetes.io/managed-by"
	managedByValue        = "wildstone"
	workloadLabel         = "wildstone.dev/workload"
	packageLabel          = "wildstone.dev/package"
	resourceLabel         = "wildstone.dev/resource"
	revisionAnnotation    = "wildstone.dev/revision"
	planAnnotation        = "wildstone.dev/plan"
	stepAnnotation        = "wildstone.dev/step-digest"
	deleteEpochAnnotation = "wildstone.dev/delete-epoch"
	maxWorkloadLogBytes   = 1 << 20
)

var (
	dnsLabel    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	digestImage = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./_:-]*@sha256:[a-f0-9]{64}$`)
)

// Refusal is an authority, ownership or stale-state failure. Callers expose
// it as FORBIDDEN rather than as a transient Kubernetes error.
type Refusal struct{ Reason string }

func (e *Refusal) Error() string { return "leader: refused: " + e.Reason }
func IsRefusal(err error) bool   { var r *Refusal; return errors.As(err, &r) }

// Invalid is a malformed typed request, distinct from a runtime API failure.
type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return "leader: invalid workload request: " + e.Reason }
func IsInvalid(err error) bool   { var i *Invalid; return errors.As(err, &i) }

type WorkloadClient interface {
	GetDeployment(context.Context, string) (*kubeapi.Deployment, error)
	ApplyDeployment(context.Context, *kubeapi.Deployment) (*kubeapi.Deployment, error)
	UpdateDeployment(context.Context, *kubeapi.Deployment) (*kubeapi.Deployment, error)
	DeleteDeployment(context.Context, string, string, string) error
	GetService(context.Context, string) (*kubeapi.Service, error)
	ApplyService(context.Context, *kubeapi.Service) (*kubeapi.Service, error)
	UpdateService(context.Context, *kubeapi.Service) (*kubeapi.Service, error)
	DeleteService(context.Context, string, string, string) error
	ListPods(context.Context, string) (*kubeapi.PodList, error)
	PodLogs(context.Context, string, uint32, int64, int64) ([]byte, bool, error)
}

// WorkloadExecutor is the only Kubernetes workload renderer. Its context is
// one barrier-cleared leadership term and is cancelled as soon as that term
// ends, so no in-flight write can continue under local stale authority.
type WorkloadExecutor struct {
	Client  WorkloadClient
	Epoch   uint64
	Context context.Context
}

func (x WorkloadExecutor) writeContext(requestCtx context.Context, a *wsv1.WorkloadAuthority) (context.Context, context.CancelFunc, error) {
	if x.Client == nil || x.Context == nil || x.Epoch == 0 {
		return nil, nil, &Refusal{Reason: "no barrier-cleared leadership term"}
	}
	if err := x.Context.Err(); err != nil {
		return nil, nil, &Refusal{Reason: "leadership term ended"}
	}
	if a == nil || a.GetAcceptedRevision() == "" || a.GetPlanId() == "" || a.GetStepDigest() == "" || a.GetLeaderEpoch() == 0 {
		return nil, nil, &Refusal{Reason: "missing accepted revision, approved Plan step, or leader epoch"}
	}
	if a.GetLeaderEpoch() != x.Epoch {
		return nil, nil, &Refusal{Reason: fmt.Sprintf("epoch %d is not active epoch %d", a.GetLeaderEpoch(), x.Epoch)}
	}
	ctx, cancel := context.WithCancel(requestCtx)
	stop := context.AfterFunc(x.Context, cancel)
	return ctx, func() { stop(); cancel() }, nil
}

func labels(w *wsv1.WorkloadSpec) map[string]string {
	return map[string]string{managedByLabel: managedByValue, workloadLabel: w.GetName(), packageLabel: w.GetPackage(), resourceLabel: w.GetResource()}
}
func annotations(a *wsv1.WorkloadAuthority) map[string]string {
	return map[string]string{epochAnnotation: strconv.FormatUint(a.GetLeaderEpoch(), 10), revisionAnnotation: a.GetAcceptedRevision(), planAnnotation: a.GetPlanId(), stepAnnotation: a.GetStepDigest()}
}
func owned(m kubeapi.ObjectMeta, name string) error {
	if m.Labels[managedByLabel] != managedByValue || m.Labels[workloadLabel] != name {
		return &Refusal{Reason: fmt.Sprintf("%s is not the expected Wildstone-owned workload", name)}
	}
	return nil
}

func validateWorkload(w *wsv1.WorkloadSpec) error {
	if w == nil || !dnsLabel.MatchString(w.GetName()) || !dnsLabel.MatchString(w.GetPackage()) || !dnsLabel.MatchString(w.GetResource()) {
		return &Invalid{Reason: "workload name, package and resource must be DNS labels"}
	}
	if !digestImage.MatchString(w.GetImage()) {
		return &Refusal{Reason: "workload image is not digest-pinned"}
	}
	if w.GetReplicas() == 0 {
		return &Invalid{Reason: "replicas must be at least one"}
	}
	if len(w.GetPorts()) == 0 {
		return &Invalid{Reason: "the first stateless workload path requires a Service port"}
	}
	if w.GetStrategy() != wsv1.WorkloadStrategy_WORKLOAD_STRATEGY_RECREATE && w.GetStrategy() != wsv1.WorkloadStrategy_WORKLOAD_STRATEGY_ROLLING_UPDATE {
		return &Invalid{Reason: "unknown workload strategy"}
	}
	if w.GetReplicas() == 1 && w.GetStrategy() != wsv1.WorkloadStrategy_WORKLOAD_STRATEGY_RECREATE {
		return &Refusal{Reason: "a singleton workload must use Recreate"}
	}
	if r := w.GetResources(); r != nil && r.GetPidsLimit() != 0 {
		return &Invalid{Reason: "pids_limit has no reviewed Kubernetes mapping"}
	}
	portNames := map[string]bool{}
	servicePorts := map[uint32]bool{}
	for _, p := range w.GetPorts() {
		if !dnsLabel.MatchString(p.GetName()) || p.GetContainerPort() == 0 || p.GetContainerPort() > 65535 || p.GetServicePort() == 0 || p.GetServicePort() > 65535 || (p.GetProtocol() != wsv1.PortProtocol_PORT_PROTOCOL_TCP && p.GetProtocol() != wsv1.PortProtocol_PORT_PROTOCOL_UDP) {
			return &Invalid{Reason: "every port needs bounded ports and an explicit protocol"}
		}
		if portNames[p.GetName()] || servicePorts[p.GetServicePort()] {
			return &Invalid{Reason: "port names and Service ports must be unique"}
		}
		portNames[p.GetName()], servicePorts[p.GetServicePort()] = true, true
	}
	for key, value := range w.GetNodeSelector() {
		if value == "" || (key != "kubernetes.io/hostname" && !strings.HasPrefix(key, "wildstone.dev/")) {
			return &Refusal{Reason: "node selector is outside the reviewed placement labels"}
		}
	}
	for _, c := range w.GetSecurity().GetCapAdd() {
		switch c {
		case "NET_ADMIN", "NET_RAW", "SYS_TIME", "IPC_LOCK":
		default:
			return &Refusal{Reason: "capability " + c + " is not allowlisted"}
		}
	}
	return nil
}

func render(w *wsv1.WorkloadSpec, a *wsv1.WorkloadAuthority) (*kubeapi.Deployment, *kubeapi.Service) {
	ls := labels(w)
	an := annotations(a)
	env := make([]kubeapi.EnvVar, 0, len(w.GetEnv()))
	keys := make([]string, 0, len(w.GetEnv()))
	for k := range w.GetEnv() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, kubeapi.EnvVar{Name: k, Value: w.GetEnv()[k]})
	}
	ports := make([]kubeapi.ContainerPort, 0, len(w.GetPorts()))
	servicePorts := make([]kubeapi.ServicePort, 0, len(w.GetPorts()))
	for _, p := range w.GetPorts() {
		protocol := "TCP"
		if p.GetProtocol() == wsv1.PortProtocol_PORT_PROTOCOL_UDP {
			protocol = "UDP"
		}
		ports = append(ports, kubeapi.ContainerPort{Name: p.GetName(), ContainerPort: int32(p.GetContainerPort()), Protocol: protocol})
		servicePorts = append(servicePorts, kubeapi.ServicePort{Name: p.GetName(), Port: int32(p.GetServicePort()), TargetPort: int32(p.GetContainerPort()), Protocol: protocol})
	}
	limits := map[string]string{}
	if r := w.GetResources(); r != nil {
		if r.GetMemory() != "" {
			limits["memory"] = r.GetMemory()
		}
		if r.GetCpus() != "" {
			limits["cpu"] = r.GetCpus()
		}
	}
	if len(limits) == 0 {
		limits = nil
	}
	sec := w.GetSecurity()
	ro, npe := sec.GetReadOnlyRootfs(), !sec.GetNoNewPrivileges()
	sc := &kubeapi.SecurityContext{ReadOnlyRootFilesystem: &ro, AllowPrivilegeEscalation: &npe, Capabilities: &kubeapi.Capabilities{Add: append([]string(nil), sec.GetCapAdd()...), Drop: append([]string(nil), sec.GetCapDrop()...)}}
	if sec.GetRunAsUser() != 0 {
		v := sec.GetRunAsUser()
		sc.RunAsUser = &v
	}
	strategy := "Recreate"
	if w.GetStrategy() == wsv1.WorkloadStrategy_WORKLOAD_STRATEGY_ROLLING_UPDATE {
		strategy = "RollingUpdate"
	}
	d := &kubeapi.Deployment{Metadata: kubeapi.ObjectMeta{Name: w.GetName(), Labels: ls, Annotations: an}, Spec: kubeapi.DeploymentSpec{Replicas: int32(w.GetReplicas()), Selector: kubeapi.LabelSelector{MatchLabels: map[string]string{workloadLabel: w.GetName()}}, Strategy: kubeapi.DeploymentStrategy{Type: strategy}, Template: kubeapi.PodTemplateSpec{Metadata: kubeapi.ObjectMeta{Labels: ls, Annotations: an}, Spec: kubeapi.PodSpec{NodeSelector: w.GetNodeSelector(), Containers: []kubeapi.Container{{Name: w.GetName(), Image: w.GetImage(), Args: w.GetCommand(), Env: env, Ports: ports, Resources: kubeapi.ResourceRequirements{Limits: limits, Requests: limits}, SecurityContext: sc}}}}}}
	s := &kubeapi.Service{Metadata: kubeapi.ObjectMeta{Name: w.GetName(), Labels: ls, Annotations: an}, Spec: kubeapi.ServiceSpec{Selector: map[string]string{workloadLabel: w.GetName()}, Ports: servicePorts}}
	return d, s
}

func (x WorkloadExecutor) Apply(requestCtx context.Context, req *wsv1.ApplyWorkload) (*wsv1.ApplyWorkloadResult, error) {
	ctx, cancel, err := x.writeContext(requestCtx, req.GetAuthority())
	if err != nil {
		return nil, err
	}
	defer cancel()
	w := req.GetWorkload()
	if err := validateWorkload(w); err != nil {
		return nil, err
	}
	cur, err := x.Client.GetDeployment(ctx, w.GetName())
	switch {
	case err == nil:
		if err := owned(cur.Metadata, w.GetName()); err != nil {
			return nil, err
		}
		stored := cur.Metadata.Annotations[revisionAnnotation]
		if stored != req.GetExpectedRevision() && stored != req.GetAuthority().GetAcceptedRevision() {
			return nil, &Refusal{Reason: fmt.Sprintf("observed revision %q, expected %q", stored, req.GetExpectedRevision())}
		}
	case kubeapi.IsNotFound(err):
		if req.GetExpectedRevision() != "" {
			return nil, &Refusal{Reason: "workload disappeared after the Plan was made"}
		}
	default:
		return nil, err
	}
	service, serviceErr := x.Client.GetService(ctx, w.GetName())
	if serviceErr == nil {
		if err := owned(service.Metadata, w.GetName()); err != nil {
			return nil, err
		}
		stored := service.Metadata.Annotations[revisionAnnotation]
		if stored != req.GetExpectedRevision() && stored != req.GetAuthority().GetAcceptedRevision() {
			return nil, &Refusal{Reason: fmt.Sprintf("Service observed revision %q, expected %q", stored, req.GetExpectedRevision())}
		}
	} else if !kubeapi.IsNotFound(serviceErr) {
		return nil, serviceErr
	}
	d, s := render(w, req.GetAuthority())
	applied, err := x.Client.ApplyDeployment(ctx, d)
	if err != nil {
		return nil, err
	}
	var appliedService *kubeapi.Service
	if len(s.Spec.Ports) > 0 {
		if appliedService, err = x.Client.ApplyService(ctx, s); err != nil {
			return nil, err
		}
	}
	result := &wsv1.ApplyWorkloadResult{Uid: applied.Metadata.UID, ResourceVersion: applied.Metadata.ResourceVersion, Generation: applied.Metadata.Generation}
	if appliedService != nil {
		result.ServiceUid = appliedService.Metadata.UID
		result.ServiceResourceVersion = appliedService.Metadata.ResourceVersion
	}
	return result, nil
}

func (x WorkloadExecutor) Delete(requestCtx context.Context, req *wsv1.DeleteWorkload) (*wsv1.DeleteWorkloadResult, error) {
	ctx, cancel, err := x.writeContext(requestCtx, req.GetAuthority())
	if err != nil {
		return nil, err
	}
	defer cancel()
	d, deploymentErr := x.Client.GetDeployment(ctx, req.GetName())
	if deploymentErr != nil && !kubeapi.IsNotFound(deploymentErr) {
		return nil, deploymentErr
	}
	if kubeapi.IsNotFound(deploymentErr) {
		d = nil
	}
	service, serviceErr := x.Client.GetService(ctx, req.GetName())
	if serviceErr != nil && !kubeapi.IsNotFound(serviceErr) {
		return nil, serviceErr
	}
	if kubeapi.IsNotFound(serviceErr) {
		service = nil
	}
	if d == nil && service == nil {
		return &wsv1.DeleteWorkloadResult{Deleted: false}, nil
	}
	if d != nil {
		if err := owned(d.Metadata, req.GetName()); err != nil {
			return nil, err
		}
		if req.GetUid() == "" || req.GetResourceVersion() == "" || d.Metadata.UID != req.GetUid() || d.Metadata.ResourceVersion != req.GetResourceVersion() {
			return nil, &Refusal{Reason: "Deployment UID/resourceVersion changed after the Plan was made"}
		}
	}
	if serviceErr == nil {
		if err := owned(service.Metadata, req.GetName()); err != nil {
			return nil, err
		}
		if req.GetServiceUid() == "" || req.GetServiceResourceVersion() == "" || service.Metadata.UID != req.GetServiceUid() || service.Metadata.ResourceVersion != req.GetServiceResourceVersion() {
			return nil, &Refusal{Reason: "Service UID/resourceVersion changed after the Plan was made"}
		}
	}
	var stamped *kubeapi.Deployment
	if d != nil {
		if d.Metadata.Annotations == nil {
			d.Metadata.Annotations = map[string]string{}
		}
		d.Metadata.Annotations[deleteEpochAnnotation] = strconv.FormatUint(x.Epoch, 10)
		d.Metadata.Annotations[epochAnnotation] = strconv.FormatUint(x.Epoch, 10)
		stamped, err = x.Client.UpdateDeployment(ctx, d)
		if err != nil {
			return nil, err
		}
	}
	if service != nil {
		if service.Metadata.Annotations == nil {
			service.Metadata.Annotations = map[string]string{}
		}
		service.Metadata.Annotations[deleteEpochAnnotation] = strconv.FormatUint(x.Epoch, 10)
		service.Metadata.Annotations[epochAnnotation] = strconv.FormatUint(x.Epoch, 10)
		stampedS, e := x.Client.UpdateService(ctx, service)
		if e != nil {
			return nil, e
		}
		if e = x.Client.DeleteService(ctx, req.GetName(), stampedS.Metadata.UID, stampedS.Metadata.ResourceVersion); e != nil {
			return nil, e
		}
	}
	if stamped != nil {
		if err := x.Client.DeleteDeployment(ctx, req.GetName(), stamped.Metadata.UID, stamped.Metadata.ResourceVersion); err != nil {
			return nil, err
		}
	}
	return &wsv1.DeleteWorkloadResult{Deleted: true}, nil
}

func (x WorkloadExecutor) Observe(ctx context.Context, req *wsv1.ObserveWorkload) (*wsv1.ObserveWorkloadResult, error) {
	if !dnsLabel.MatchString(req.GetName()) {
		return nil, &Invalid{Reason: "invalid workload name"}
	}
	d, err := x.Client.GetDeployment(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	if err := owned(d.Metadata, req.GetName()); err != nil {
		return nil, err
	}
	pods, err := x.Client.ListPods(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	r := &wsv1.ObserveWorkloadResult{Uid: d.Metadata.UID, ResourceVersion: d.Metadata.ResourceVersion, Revision: d.Metadata.Annotations[revisionAnnotation], Generation: d.Metadata.Generation, ObservedGeneration: d.Status.ObservedGeneration, Replicas: uint32(d.Status.Replicas), UpdatedReplicas: uint32(d.Status.UpdatedReplicas), ReadyReplicas: uint32(d.Status.ReadyReplicas), AvailableReplicas: uint32(d.Status.AvailableReplicas)}
	if service, serviceErr := x.Client.GetService(ctx, req.GetName()); serviceErr == nil {
		if err := owned(service.Metadata, req.GetName()); err != nil {
			return nil, err
		}
		r.ServiceUid, r.ServiceResourceVersion = service.Metadata.UID, service.Metadata.ResourceVersion
	} else if !kubeapi.IsNotFound(serviceErr) {
		return nil, serviceErr
	}
	for _, c := range d.Status.Conditions {
		r.Conditions = append(r.Conditions, &wsv1.WorkloadCondition{Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message})
	}
	nodes := map[string]bool{}
	for _, p := range pods.Items {
		if err := owned(p.Metadata, req.GetName()); err != nil {
			return nil, err
		}
		if p.Spec.NodeName != "" {
			nodes[p.Spec.NodeName] = true
		}
		for _, cs := range p.Status.ContainerStatuses {
			r.RestartCount += uint32(cs.RestartCount)
			if r.Image == "" {
				r.Image = cs.ImageID
			}
		}
	}
	for n := range nodes {
		r.PodNodes = append(r.PodNodes, n)
	}
	sort.Strings(r.PodNodes)
	return r, nil
}

func (x WorkloadExecutor) Logs(ctx context.Context, req *wsv1.WorkloadLogs) (*wsv1.WorkloadLogsResult, error) {
	if !dnsLabel.MatchString(req.GetName()) {
		return nil, &Invalid{Reason: "invalid workload name"}
	}
	limit := int64(req.GetLimitBytes())
	if limit == 0 {
		limit = maxWorkloadLogBytes
	}
	if limit > maxWorkloadLogBytes {
		return nil, &Refusal{Reason: "log limit exceeds 1 MiB"}
	}
	pods, err := x.Client.ListPods(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Metadata.Name < pods.Items[j].Metadata.Name })
	var out []byte
	truncated := false
	for _, p := range pods.Items {
		if err := owned(p.Metadata, req.GetName()); err != nil {
			return nil, err
		}
		remaining := limit - int64(len(out))
		if len(out) > 0 {
			remaining--
		}
		if remaining <= 0 {
			truncated = true
			break
		}
		b, t, err := x.Client.PodLogs(ctx, p.Metadata.Name, req.GetTail(), req.GetSinceUnixSec(), remaining)
		if err != nil {
			return nil, err
		}
		if len(out) > 0 {
			out = append(out, '\n')
		}
		out = append(out, b...)
		truncated = truncated || t
	}
	return &wsv1.WorkloadLogsResult{Logs: out, Truncated: truncated}, nil
}
