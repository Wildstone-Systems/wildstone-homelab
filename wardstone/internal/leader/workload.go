package leader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

const (
	managedByLabel          = "app.kubernetes.io/managed-by"
	managedByValue          = "wildstone"
	workloadLabel           = "wildstone.dev/workload"
	packageLabel            = "wildstone.dev/package"
	resourceLabel           = "wildstone.dev/resource"
	revisionAnnotation      = "wildstone.dev/revision"
	planAnnotation          = "wildstone.dev/plan"
	stepAnnotation          = "wildstone.dev/step-digest"
	deleteEpochAnnotation   = "wildstone.dev/delete-epoch"
	deleteRequestAnnotation = "wildstone.dev/delete-request"
	deleteStateAnnotation   = "wildstone.dev/delete-state"
	maxWorkloadLogBytes     = 1 << 20
)

var (
	dnsLabel             = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	digestImage          = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./_:-]*@sha256:[a-f0-9]{64}$`)
	dockerMemoryQuantity = regexp.MustCompile(`^[0-9]+[bBkKmMgG]?$`)
	decimalCPUQuantity   = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)
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
	CreateDeployment(context.Context, *kubeapi.Deployment) (*kubeapi.Deployment, error)
	ApplyDeployment(context.Context, *kubeapi.Deployment) (*kubeapi.Deployment, error)
	UpdateDeployment(context.Context, *kubeapi.Deployment) (*kubeapi.Deployment, error)
	DeleteDeployment(context.Context, string, string, string) error
	GetService(context.Context, string) (*kubeapi.Service, error)
	CreateService(context.Context, *kubeapi.Service) (*kubeapi.Service, error)
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
	if r := w.GetResources(); r != nil {
		if _, err := kubernetesMemoryQuantity(r.GetMemory()); err != nil {
			return &Invalid{Reason: err.Error()}
		}
		if _, err := kubernetesCPUQuantity(r.GetCpus()); err != nil {
			return &Invalid{Reason: err.Error()}
		}
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
			limits["memory"], _ = kubernetesMemoryQuantity(r.GetMemory())
		}
		if r.GetCpus() != "" {
			limits["cpu"], _ = kubernetesCPUQuantity(r.GetCpus())
		}
	}
	if len(limits) == 0 {
		limits = nil
	}
	sec := w.GetSecurity()
	ro, npe, nonRoot := sec.GetReadOnlyRootfs(), !sec.GetNoNewPrivileges(), true
	sc := &kubeapi.SecurityContext{ReadOnlyRootFilesystem: &ro, AllowPrivilegeEscalation: &npe, RunAsNonRoot: &nonRoot, SeccompProfile: &kubeapi.SeccompProfile{Type: "RuntimeDefault"}, Capabilities: &kubeapi.Capabilities{Add: append([]string(nil), sec.GetCapAdd()...), Drop: append([]string(nil), sec.GetCapDrop()...)}}
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
	service, serviceErr := x.Client.GetService(ctx, w.GetName())
	serviceExists := serviceErr == nil
	expectedServiceRevision := req.GetExpectedServiceRevision()
	if !req.GetSeparateRevisionWitnesses() {
		// Preserve the original single-witness contract for pre-minor-32
		// callers. The explicit marker distinguishes them from a new caller that
		// observed no Service and therefore carries an authoritative empty
		// Service witness.
		expectedServiceRevision = req.GetExpectedRevision()
	}
	if serviceErr == nil {
		if err := owned(service.Metadata, w.GetName()); err != nil {
			return nil, err
		}
		stored := service.Metadata.Annotations[revisionAnnotation]
		if stored != expectedServiceRevision && stored != req.GetAuthority().GetAcceptedRevision() {
			return nil, &Refusal{Reason: fmt.Sprintf("Service observed revision %q, expected %q", stored, expectedServiceRevision)}
		}
	} else if !kubeapi.IsNotFound(serviceErr) {
		return nil, serviceErr
	}
	cur, err := x.Client.GetDeployment(ctx, w.GetName())
	deploymentExists := err == nil
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
		// A surviving owned Service is the compare-and-set witness for a
		// Service-only observation. It permits the reviewed repair to recreate
		// the missing Deployment without turning a stale update into a create.
		if req.GetExpectedRevision() != "" && (!serviceExists || service.Metadata.Annotations[revisionAnnotation] != expectedServiceRevision) {
			return nil, &Refusal{Reason: "workload disappeared after the Plan was made"}
		}
	default:
		return nil, err
	}
	d, s := render(w, req.GetAuthority())
	var applied *kubeapi.Deployment
	if deploymentExists {
		d.Metadata.ResourceVersion = cur.Metadata.ResourceVersion
		applied, err = x.Client.ApplyDeployment(ctx, d)
	} else {
		applied, err = x.Client.CreateDeployment(ctx, d)
	}
	if err != nil {
		return nil, err
	}
	var appliedService *kubeapi.Service
	if len(s.Spec.Ports) > 0 {
		if serviceExists {
			s.Metadata.ResourceVersion = service.Metadata.ResourceVersion
			appliedService, err = x.Client.ApplyService(ctx, s)
		} else {
			appliedService, err = x.Client.CreateService(ctx, s)
		}
		if err != nil {
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

func deleteRequest(req *wsv1.DeleteWorkload) string {
	a := req.GetAuthority()
	b := []byte(strings.Join([]string{a.GetAcceptedRevision(), a.GetPlanId(), a.GetStepDigest(), strconv.FormatUint(a.GetLeaderEpoch(), 10), req.GetName(), req.GetUid(), req.GetResourceVersion(), req.GetServiceUid(), req.GetServiceResourceVersion()}, "\x00"))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func deleteState(meta kubeapi.ObjectMeta, spec any) string {
	annotations := make(map[string]string, len(meta.Annotations))
	for key, value := range meta.Annotations {
		switch key {
		case deleteEpochAnnotation, deleteRequestAnnotation, deleteStateAnnotation, epochAnnotation:
			continue
		}
		annotations[key] = value
	}
	b, _ := json.Marshal(struct {
		UID         string            `json:"uid"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		Spec        any               `json:"spec"`
	}{meta.UID, meta.Labels, annotations, spec})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func deleteTarget(meta kubeapi.ObjectMeta, uid, rv, marker, state string) error {
	if uid == "" || rv == "" || meta.UID != uid {
		return &Refusal{Reason: "object UID/resourceVersion changed after the Plan was made"}
	}
	if meta.ResourceVersion == rv {
		return nil
	}
	if meta.Annotations[deleteRequestAnnotation] != marker || meta.Annotations[deleteStateAnnotation] != state {
		return &Refusal{Reason: "object changed after the Plan was made or delete stamp"}
	}
	return nil
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
	marker := deleteRequest(req)
	if d != nil {
		if err := owned(d.Metadata, req.GetName()); err != nil {
			return nil, err
		}
		if err := deleteTarget(d.Metadata, req.GetUid(), req.GetResourceVersion(), marker, deleteState(d.Metadata, d.Spec)); err != nil {
			return nil, fmt.Errorf("Deployment: %w", err)
		}
	}
	if serviceErr == nil {
		if err := owned(service.Metadata, req.GetName()); err != nil {
			return nil, err
		}
		if err := deleteTarget(service.Metadata, req.GetServiceUid(), req.GetServiceResourceVersion(), marker, deleteState(service.Metadata, service.Spec)); err != nil {
			return nil, fmt.Errorf("Service: %w", err)
		}
	}
	var stamped *kubeapi.Deployment
	if d != nil {
		if d.Metadata.Annotations == nil {
			d.Metadata.Annotations = map[string]string{}
		}
		d.Metadata.Annotations[deleteEpochAnnotation] = strconv.FormatUint(x.Epoch, 10)
		d.Metadata.Annotations[epochAnnotation] = strconv.FormatUint(x.Epoch, 10)
		d.Metadata.Annotations[deleteRequestAnnotation] = marker
		d.Metadata.Annotations[deleteStateAnnotation] = deleteState(d.Metadata, d.Spec)
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
		service.Metadata.Annotations[deleteRequestAnnotation] = marker
		service.Metadata.Annotations[deleteStateAnnotation] = deleteState(service.Metadata, service.Spec)
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
		return nil, deploymentErr
	}
	r := &wsv1.ObserveWorkloadResult{}
	var pods *kubeapi.PodList
	if d != nil {
		if err := owned(d.Metadata, req.GetName()); err != nil {
			return nil, err
		}
		pods, deploymentErr = x.Client.ListPods(ctx, req.GetName())
		if deploymentErr != nil {
			return nil, deploymentErr
		}
		r.Uid, r.ResourceVersion = d.Metadata.UID, d.Metadata.ResourceVersion
		r.Revision = d.Metadata.Annotations[revisionAnnotation]
		r.Generation, r.ObservedGeneration = d.Metadata.Generation, d.Status.ObservedGeneration
		r.Replicas, r.UpdatedReplicas = uint32(d.Status.Replicas), uint32(d.Status.UpdatedReplicas)
		r.ReadyReplicas, r.AvailableReplicas = uint32(d.Status.ReadyReplicas), uint32(d.Status.AvailableReplicas)
	}
	if service != nil {
		if err := owned(service.Metadata, req.GetName()); err != nil {
			return nil, err
		}
		r.ServiceUid, r.ServiceResourceVersion = service.Metadata.UID, service.Metadata.ResourceVersion
		r.ServiceRevision = service.Metadata.Annotations[revisionAnnotation]
		if r.Revision == "" {
			r.Revision = r.ServiceRevision
		}
	}
	if desired := req.GetDesired(); desired != nil {
		if err := validateWorkload(desired); err != nil {
			return nil, err
		}
		if desired.GetName() != req.GetName() {
			return nil, &Invalid{Reason: "desired workload name differs from observed name"}
		}
		wantDeployment, wantService := render(desired, &wsv1.WorkloadAuthority{})
		r.MatchesDesired = d != nil && service != nil && d.Spec.Replicas == wantDeployment.Spec.Replicas &&
			sameManagedFields(d.Spec.Selector, wantDeployment.Spec.Selector) &&
			sameManagedFields(d.Spec.Strategy, wantDeployment.Spec.Strategy) &&
			sameManagedFields(d.Spec.Template.Metadata.Labels, wantDeployment.Spec.Template.Metadata.Labels) &&
			sameManagedPodSpec(d.Spec.Template.Spec, wantDeployment.Spec.Template.Spec) &&
			sameManagedFields(service.Spec, wantService.Spec)
	}
	if d != nil {
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
	}
	return r, nil
}

// Kubernetes reads pass through JSON. Compare the managed wire fields so
// an omitted empty slice (for example Env) equals the empty slice render
// builds, while a changed image, selector or Service port still drifts.
func sameManagedFields(a, b any) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func sameManagedPodSpec(a, b kubeapi.PodSpec) bool {
	left, right := a, b
	normalizePodResources(&left)
	normalizePodResources(&right)
	return sameManagedFields(left, right)
}

func normalizePodResources(p *kubeapi.PodSpec) {
	for i := range p.Containers {
		for _, resources := range []map[string]string{p.Containers[i].Resources.Limits, p.Containers[i].Resources.Requests} {
			for name, value := range resources {
				if normalized, ok := kubernetesQuantityValue(name, value); ok {
					resources[name] = normalized.RatString()
				}
			}
		}
	}
}

func kubernetesQuantityValue(name, value string) (*big.Rat, bool) {
	var multipliers map[string]*big.Rat
	switch name {
	case "cpu":
		multipliers = map[string]*big.Rat{
			"": big.NewRat(1, 1), "m": big.NewRat(1, 1_000),
			"u": big.NewRat(1, 1_000_000), "n": big.NewRat(1, 1_000_000_000),
		}
	case "memory":
		multipliers = map[string]*big.Rat{
			"": big.NewRat(1, 1), "Ki": big.NewRat(1<<10, 1),
			"Mi": big.NewRat(1<<20, 1), "Gi": big.NewRat(1<<30, 1),
			"Ti": new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 40)),
			"k":  big.NewRat(1_000, 1), "M": big.NewRat(1_000_000, 1),
			"G": big.NewRat(1_000_000_000, 1), "m": big.NewRat(1, 1_000),
		}
	default:
		return nil, false
	}
	for _, suffix := range []string{"Ki", "Mi", "Gi", "Ti", "m", "u", "n", "k", "M", "G", ""} {
		multiplier, allowed := multipliers[suffix]
		if !allowed || !strings.HasSuffix(value, suffix) {
			continue
		}
		number := strings.TrimSuffix(value, suffix)
		if number == "" {
			continue
		}
		quantity, ok := new(big.Rat).SetString(number)
		if !ok {
			return nil, false
		}
		return quantity.Mul(quantity, multiplier), true
	}
	return nil, false
}

// Workload resources retain the existing Docker-readable public contract.
// Translate it only at the typed Kubernetes execution boundary so "512m"
// keeps meaning 512 MiB rather than Kubernetes' 0.512-byte milli suffix.
func kubernetesMemoryQuantity(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !dockerMemoryQuantity.MatchString(value) {
		return "", fmt.Errorf("memory %q is not a Docker size string", value)
	}
	multiplier := int64(1)
	number := value
	switch value[len(value)-1] {
	case 'b', 'B':
		number = value[:len(value)-1]
	case 'k', 'K':
		multiplier, number = 1<<10, value[:len(value)-1]
	case 'm', 'M':
		multiplier, number = 1<<20, value[:len(value)-1]
	case 'g', 'G':
		multiplier, number = 1<<30, value[:len(value)-1]
	}
	n, err := strconv.ParseInt(number, 10, 64)
	if err != nil || n <= 0 || n > (1<<63-1)/multiplier {
		return "", fmt.Errorf("memory %q is not a bounded Docker size string", value)
	}
	bytes := n * multiplier
	for _, unit := range []struct {
		bytes int64
		name  string
	}{{1 << 30, "Gi"}, {1 << 20, "Mi"}, {1 << 10, "Ki"}} {
		if bytes%unit.bytes == 0 {
			return strconv.FormatInt(bytes/unit.bytes, 10) + unit.name, nil
		}
	}
	return strconv.FormatInt(bytes, 10), nil
}

func kubernetesCPUQuantity(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !decimalCPUQuantity.MatchString(value) {
		return "", fmt.Errorf("cpu %q is not a decimal core count", value)
	}
	cores, ok := new(big.Rat).SetString(value)
	if !ok || cores.Sign() <= 0 {
		return "", fmt.Errorf("cpu %q is not a positive decimal core count", value)
	}
	milli := new(big.Rat).Mul(cores, big.NewRat(1_000, 1))
	if !milli.IsInt() {
		return "", fmt.Errorf("cpu %q is finer than Kubernetes' 1m precision", value)
	}
	m := milli.Num()
	if new(big.Int).Mod(new(big.Int).Set(m), big.NewInt(1_000)).Sign() == 0 {
		return new(big.Int).Quo(new(big.Int).Set(m), big.NewInt(1_000)).String(), nil
	}
	return m.String() + "m", nil
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
