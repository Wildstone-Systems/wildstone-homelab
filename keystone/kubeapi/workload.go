package kubeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const WorkloadNamespace = "wildstone-apps"

type Deployment struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	Metadata   ObjectMeta       `json:"metadata"`
	Spec       DeploymentSpec   `json:"spec"`
	Status     DeploymentStatus `json:"status,omitempty"`
}

type DeploymentSpec struct {
	Replicas int32              `json:"replicas"`
	Selector LabelSelector      `json:"selector"`
	Strategy DeploymentStrategy `json:"strategy"`
	Template PodTemplateSpec    `json:"template"`
}

type LabelSelector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}
type DeploymentStrategy struct {
	Type string `json:"type"`
}
type PodTemplateSpec struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     PodSpec    `json:"spec"`
}
type PodSpec struct {
	Containers   []Container       `json:"containers"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	NodeName     string            `json:"nodeName,omitempty"`
}
type Container struct {
	Name            string               `json:"name"`
	Image           string               `json:"image"`
	Command         []string             `json:"command,omitempty"`
	Args            []string             `json:"args,omitempty"`
	Env             []EnvVar             `json:"env,omitempty"`
	Ports           []ContainerPort      `json:"ports,omitempty"`
	Resources       ResourceRequirements `json:"resources,omitempty"`
	SecurityContext *SecurityContext     `json:"securityContext,omitempty"`
}
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type ContainerPort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int32  `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}
type ResourceRequirements struct {
	Limits   map[string]string `json:"limits,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
}
type SecurityContext struct {
	ReadOnlyRootFilesystem   *bool         `json:"readOnlyRootFilesystem,omitempty"`
	AllowPrivilegeEscalation *bool         `json:"allowPrivilegeEscalation,omitempty"`
	RunAsUser                *int64        `json:"runAsUser,omitempty"`
	Capabilities             *Capabilities `json:"capabilities,omitempty"`
}
type Capabilities struct {
	Add  []string `json:"add,omitempty"`
	Drop []string `json:"drop,omitempty"`
}
type DeploymentStatus struct {
	ObservedGeneration int64                 `json:"observedGeneration,omitempty"`
	Replicas           int32                 `json:"replicas,omitempty"`
	UpdatedReplicas    int32                 `json:"updatedReplicas,omitempty"`
	ReadyReplicas      int32                 `json:"readyReplicas,omitempty"`
	AvailableReplicas  int32                 `json:"availableReplicas,omitempty"`
	Conditions         []DeploymentCondition `json:"conditions,omitempty"`
}
type DeploymentCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type Service struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   ObjectMeta  `json:"metadata"`
	Spec       ServiceSpec `json:"spec"`
}
type ServiceSpec struct {
	Selector map[string]string `json:"selector"`
	Ports    []ServicePort     `json:"ports"`
}
type ServicePort struct {
	Name       string `json:"name,omitempty"`
	Port       int32  `json:"port"`
	TargetPort int32  `json:"targetPort"`
	Protocol   string `json:"protocol,omitempty"`
}

type PodList struct {
	Items []Pod `json:"items"`
}
type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     PodSpec    `json:"spec"`
	Status   PodStatus  `json:"status"`
}
type PodStatus struct {
	Phase             string            `json:"phase"`
	ContainerStatuses []ContainerStatus `json:"containerStatuses"`
}
type ContainerStatus struct {
	Name         string `json:"name"`
	Image        string `json:"image"`
	ImageID      string `json:"imageID"`
	RestartCount int32  `json:"restartCount"`
}

type DeleteOptions struct {
	APIVersion    string         `json:"apiVersion"`
	Kind          string         `json:"kind"`
	Preconditions *Preconditions `json:"preconditions,omitempty"`
}
type Preconditions struct {
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

func deploymentPath(name string) string {
	p := "/apis/apps/v1/namespaces/" + WorkloadNamespace + "/deployments"
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}
func servicePath(name string) string {
	p := "/api/v1/namespaces/" + WorkloadNamespace + "/services"
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

func (c *Client) GetDeployment(ctx context.Context, name string) (*Deployment, error) {
	var d Deployment
	return &d, c.do(ctx, http.MethodGet, deploymentPath(name), nil, &d)
}

// CreateDeployment creates only when the observed name is still absent.
func (c *Client) CreateDeployment(ctx context.Context, d *Deployment) (*Deployment, error) {
	d.APIVersion, d.Kind, d.Metadata.Namespace = "apps/v1", "Deployment", WorkloadNamespace
	var out Deployment
	return &out, c.do(ctx, http.MethodPost, deploymentPath(""), d, &out)
}
func (c *Client) ApplyDeployment(ctx context.Context, d *Deployment) (*Deployment, error) {
	if d.Metadata.ResourceVersion == "" {
		return nil, fmt.Errorf("kubeapi: a Deployment apply needs the resourceVersion it read")
	}
	d.APIVersion, d.Kind, d.Metadata.Namespace = "apps/v1", "Deployment", WorkloadNamespace
	var out Deployment
	p := deploymentPath(d.Metadata.Name) + "?fieldManager=wildstone-wardstone&force=false"
	return &out, c.doContent(ctx, http.MethodPatch, p, "application/apply-patch+yaml", d, &out)
}
func (c *Client) UpdateDeployment(ctx context.Context, d *Deployment) (*Deployment, error) {
	if d.Metadata.ResourceVersion == "" {
		return nil, fmt.Errorf("kubeapi: a Deployment update needs resourceVersion")
	}
	var out Deployment
	return &out, c.do(ctx, http.MethodPut, deploymentPath(d.Metadata.Name), d, &out)
}
func (c *Client) GetService(ctx context.Context, name string) (*Service, error) {
	var s Service
	return &s, c.do(ctx, http.MethodGet, servicePath(name), nil, &s)
}

// CreateService creates only when the observed name is still absent.
func (c *Client) CreateService(ctx context.Context, s *Service) (*Service, error) {
	s.APIVersion, s.Kind, s.Metadata.Namespace = "v1", "Service", WorkloadNamespace
	var out Service
	return &out, c.do(ctx, http.MethodPost, servicePath(""), s, &out)
}
func (c *Client) ApplyService(ctx context.Context, s *Service) (*Service, error) {
	if s.Metadata.ResourceVersion == "" {
		return nil, fmt.Errorf("kubeapi: a Service apply needs the resourceVersion it read")
	}
	s.APIVersion, s.Kind, s.Metadata.Namespace = "v1", "Service", WorkloadNamespace
	var out Service
	p := servicePath(s.Metadata.Name) + "?fieldManager=wildstone-wardstone&force=false"
	return &out, c.doContent(ctx, http.MethodPatch, p, "application/apply-patch+yaml", s, &out)
}
func (c *Client) UpdateService(ctx context.Context, s *Service) (*Service, error) {
	if s.Metadata.ResourceVersion == "" {
		return nil, fmt.Errorf("kubeapi: a Service update needs resourceVersion")
	}
	var out Service
	return &out, c.do(ctx, http.MethodPut, servicePath(s.Metadata.Name), s, &out)
}
func (c *Client) DeleteDeployment(ctx context.Context, name, uid, rv string) error {
	o := DeleteOptions{APIVersion: "v1", Kind: "DeleteOptions", Preconditions: &Preconditions{UID: uid, ResourceVersion: rv}}
	return c.do(ctx, http.MethodDelete, deploymentPath(name), &o, nil)
}
func (c *Client) DeleteService(ctx context.Context, name, uid, rv string) error {
	o := DeleteOptions{APIVersion: "v1", Kind: "DeleteOptions", Preconditions: &Preconditions{UID: uid, ResourceVersion: rv}}
	return c.do(ctx, http.MethodDelete, servicePath(name), &o, nil)
}
func (c *Client) ListPods(ctx context.Context, workload string) (*PodList, error) {
	var out PodList
	p := "/api/v1/namespaces/" + WorkloadNamespace + "/pods?labelSelector=" + url.QueryEscape("wildstone.dev/workload="+workload)
	return &out, c.do(ctx, http.MethodGet, p, nil, &out)
}
func (c *Client) PodLogs(ctx context.Context, pod string, tail uint32, since int64, limit int64) ([]byte, bool, error) {
	q := url.Values{}
	if tail > 0 {
		q.Set("tailLines", strconv.FormatUint(uint64(tail), 10))
	}
	if since > 0 {
		q.Set("sinceTime", time.Unix(since, 0).UTC().Format(time.RFC3339))
	}
	p := "/api/v1/namespaces/" + WorkloadNamespace + "/pods/" + url.PathEscape(pod) + "/log?" + q.Encode()
	tok, err := c.token()
	if err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+p, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil {
			return nil, false, err
		}
		var se StatusError
		_ = json.NewDecoder(bytes.NewReader(b)).Decode(&se)
		se.Code = resp.StatusCode
		return nil, false, &se
	}
	return readSuffix(resp.Body, limit)
}

// readSuffix consumes the response while retaining at most limit bytes. Logs
// are tail-biased, so reading only the first limit+1 bytes is not sufficient.
func readSuffix(r io.Reader, limit int64) ([]byte, bool, error) {
	if limit <= 0 {
		return nil, false, fmt.Errorf("kubeapi: log suffix limit must be positive")
	}
	buf := make([]byte, int(limit))
	chunk := make([]byte, 32*1024)
	var total int64
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			for _, b := range chunk[:n] {
				buf[total%limit] = b
				total++
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, err
		}
	}
	if total <= limit {
		return buf[:int(total)], false, nil
	}
	start := total % limit
	out := make([]byte, 0, int(limit))
	out = append(out, buf[start:]...)
	out = append(out, buf[:start]...)
	return out, true, nil
}
