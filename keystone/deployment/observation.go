package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"gopkg.in/yaml.v3"
)

// ObservationConfig is privileged local inventory, never request input. Use a
// separate read-only identity, not a reconciler or cluster administrator.
type ObservationConfig struct {
	Server                 string `json:"server"`
	CAFile                 string `json:"caFile"`
	TokenFile              string `json:"tokenFile"`
	SourceNamespace        string `json:"sourceNamespace"`
	SourceName             string `json:"sourceName"`
	KustomizationNamespace string `json:"kustomizationNamespace"`
	KustomizationName      string `json:"kustomizationName"`
	// Without a fixed local /healthcheck endpoint, only workload readiness is known.
	HealthURL string `json:"healthURL,omitempty"`
}
type fluxReader interface {
	ObserveFlux(context.Context, string, string, string) (*kubeapi.FluxObservation, error)
}
type Observer struct {
	config ObservationConfig
	reader fluxReader
	health *http.Client
}

var observedName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func newObserver(c *ObservationConfig) (*Observer, error) {
	if c == nil {
		return nil, nil
	}
	for _, name := range []string{c.SourceNamespace, c.SourceName, c.KustomizationNamespace, c.KustomizationName} {
		if !observedName.MatchString(name) {
			return nil, fmt.Errorf("deployment: invalid fixed observation identity")
		}
	}
	u, err := url.Parse(c.Server)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("deployment: observation requires a TLS API server")
	}
	if _, err := localFile(c.TokenFile, true); err != nil {
		return nil, fmt.Errorf("deployment: private observer token unavailable")
	}
	reader, err := kubeapi.New(c.Server, c.CAFile, c.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("deployment: observer TLS configuration unavailable")
	}
	if c.HealthURL != "" && !localHealthURL(c.HealthURL) {
		return nil, fmt.Errorf("deployment: health endpoint must be fixed local /healthcheck")
	}
	return &Observer{config: *c, reader: reader, health: &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{Proxy: nil, DialContext: localHealthDial}}}, nil
}
func localHealthURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/healthcheck" {
		return false
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	return host == "localhost" || strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local") || ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}
func localHealthDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if !ip.IP.IsLoopback() && !ip.IP.IsPrivate() {
			return nil, fmt.Errorf("nonlocal health address refused")
		}
	}
	for _, ip := range ips {
		c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return c, nil
		}
	}
	return nil, fmt.Errorf("local health endpoint unavailable")
}
func currentState(o *kubeapi.FluxObservation) string {
	if o.Spec["suspend"] == true || o.Metadata.DeletionTimestamp != nil {
		return "dependency_unavailable"
	}
	if o.Status.ObservedGeneration != o.Metadata.Generation {
		return "reconciling"
	}
	ready := "unknown"
	for _, c := range o.Status.Conditions {
		if c.ObservedGeneration != o.Metadata.Generation {
			continue
		}
		if (c.Type == "Stalled" || c.Type == "Reconciling") && c.Status == "True" {
			if c.Type == "Stalled" {
				return "failed"
			}
			return "reconciling"
		}
		if c.Type == "Ready" {
			switch c.Status {
			case "True":
				ready = "ready"
			case "False":
				ready = "failed"
			}
		}
	}
	return ready
}
func revisionMatches(revision, sha string) bool { return sha != "" && revision == "deploy@sha1:"+sha }
func nested(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		next, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = next[k]
	}
	return v
}
func subset(expected, actual any) bool {
	if m, ok := expected.(map[string]any); ok {
		a, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range m {
			if !subset(v, a[k]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(expected, actual)
}
func expectedRelease(r *runtimedb.DeploymentReview) map[string]any {
	var doc map[string]any
	if yaml.Unmarshal([]byte(r.Files["release.yaml"]), &doc) != nil {
		return nil
	}
	raw, err := json.Marshal(doc["spec"])
	if err != nil {
		return nil
	}
	var spec map[string]any
	if json.Unmarshal(raw, &spec) != nil {
		return nil
	}
	return spec
}
func (o *Observer) observe(r *runtimedb.DeploymentReview, out *runtimedb.DeploymentStatus) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out.Reconciliation = "unknown"
	out.Application = "unobserved"
	out.ControllerDetail = "Controller observation is incomplete."
	defer func() {
		now := time.Now().UTC()
		expiry := now.Add(30 * time.Second)
		out.ControllerObservedAt = &now
		out.ControllerExpiresAt = &expiry
	}()
	get := func(kind, ns, name string) *kubeapi.FluxObservation {
		out.ControllerStage = kind
		out.ControllerDetail = "Observing the current " + kind + " generation."
		obj, err := o.reader.ObserveFlux(ctx, kind, ns, name)
		if err != nil {
			out.Reconciliation = "dependency_unavailable"
			out.ControllerDetail = "A required object or read-only API dependency is unavailable."
			return nil
		}
		return obj
	}
	source := get("GitRepository", o.config.SourceNamespace, o.config.SourceName)
	if source == nil {
		return
	}
	out.SourceGeneration = source.Metadata.Generation
	out.SourceObservedGeneration = source.Status.ObservedGeneration
	if source.Status.Artifact != nil {
		out.SourceRevision = safeRevision(source.Status.Artifact.Revision)
	}
	if source.Spec["suspend"] == true || nested(source.Spec, "ref", "branch") != "deploy" || nested(source.Spec, "ref", "commit") != nil || nested(source.Spec, "ref", "tag") != nil || nested(source.Spec, "ref", "semver") != nil {
		out.Reconciliation = "dependency_unavailable"
		out.ControllerDetail = "Deployment source is suspended or its branch binding changed."
		return
	}
	out.Reconciliation = currentState(source)
	if out.Reconciliation != "ready" {
		return
	}
	if !revisionMatches(out.SourceRevision, out.PromotedRevision) {
		out.Reconciliation = "reconciling"
		return
	}
	k := get("Kustomization", o.config.KustomizationNamespace, o.config.KustomizationName)
	if k == nil {
		return
	}
	out.KustomizationGeneration = k.Metadata.Generation
	out.KustomizationObservedGeneration = k.Status.ObservedGeneration
	out.ReconciledRevision = safeRevision(k.Status.LastAppliedRevision)
	ns := nested(k.Spec, "sourceRef", "namespace")
	if ns == nil {
		ns = o.config.KustomizationNamespace
	}
	if nested(k.Spec, "sourceRef", "kind") != "GitRepository" || nested(k.Spec, "sourceRef", "name") != o.config.SourceName || ns != o.config.SourceNamespace || (k.Spec["path"] != "." && k.Spec["path"] != "./") {
		out.Reconciliation = "unknown"
		out.ControllerDetail = "Kustomization source/path binding differs from the isolated deployment."
		return
	}
	out.Reconciliation = currentState(k)
	if out.Reconciliation != "ready" {
		return
	}
	if !revisionMatches(out.ReconciledRevision, out.PromotedRevision) {
		out.Reconciliation = "reconciling"
		return
	}
	h := get("HelmRelease", r.Namespace, r.Release)
	if h == nil {
		return
	}
	out.HelmGeneration = h.Metadata.Generation
	out.HelmObservedGeneration = h.Status.ObservedGeneration
	expected := expectedRelease(r)
	if expected == nil || !subset(expected, h.Spec) || h.Spec["postRenderers"] != nil || h.Spec["kubeConfig"] != nil {
		out.Reconciliation = "unknown"
		out.ControllerDetail = "Live Helm input differs from exact reviewed input."
		return
	}
	out.Reconciliation = currentState(h)
	if out.Reconciliation != "ready" {
		return
	}
	// Verify the application chart source as well as the deployment source.
	chart := get("GitRepository", r.Namespace, r.Release)
	if chart == nil {
		return
	}
	if chart.Spec["url"] != r.ChartSource || nested(chart.Spec, "ref", "commit") != r.ChartCommit || nested(chart.Spec, "ref", "branch") != nil || nested(chart.Spec, "ref", "tag") != nil || nested(chart.Spec, "ref", "semver") != nil {
		out.Reconciliation = "unknown"
		out.ControllerDetail = "Live chart source differs from the reviewed immutable chart."
		return
	}
	out.Reconciliation = currentState(chart)
	if out.Reconciliation != "ready" {
		return
	}
	if chart.Status.Artifact == nil || !chartRevisionMatches(chart.Status.Artifact.Revision, r.ChartCommit) {
		out.Reconciliation = "reconciling"
		return
	}
	d := get("Deployment", r.Namespace, r.Release)
	if d == nil {
		return
	}
	out.WorkloadGeneration = d.Metadata.Generation
	out.WorkloadObservedGeneration = d.Status.ObservedGeneration
	if d.Metadata.DeletionTimestamp != nil {
		out.Application = "dependency_unavailable"
		return
	}
	out.Application = "reconciling"
	for _, condition := range d.Status.Conditions {
		if condition.Type == "Progressing" && condition.Status == "False" && condition.Reason == "ProgressDeadlineExceeded" || condition.Type == "ReplicaFailure" && condition.Status == "True" {
			if d.Status.ObservedGeneration == d.Metadata.Generation {
				out.Application = "failed"
				out.ControllerDetail = "The current workload rollout failed."
				return
			}
		}
	}
	if d.Status.ObservedGeneration != d.Metadata.Generation || d.Spec["replicas"] != float64(1) || d.Status.Replicas != 1 || d.Status.UpdatedReplicas != 1 || d.Status.ReadyReplicas != 1 || d.Status.AvailableReplicas != 1 {
		return
	}
	containers, ok := nested(d.Spec, "template", "spec", "containers").([]any)
	if !ok || len(containers) != 1 {
		return
	}
	c, ok := containers[0].(map[string]any)
	if !ok || c["image"] != r.Image {
		return
	}
	env, _ := c["env"].([]any)
	zone := false
	for _, v := range env {
		e, _ := v.(map[string]any)
		if e["name"] == "TZ" && e["value"] == r.Timezone {
			zone = true
		}
	}
	if !zone {
		return
	}
	if d.Metadata.Labels["app.kubernetes.io/instance"] != r.Release {
		out.Application = "unknown"
		return
	}
	out.Application = "workload_ready"
	out.ControllerDetail = "Exact source applied; current Helm and workload generations are ready. Application health has not been probed."
	if o.config.HealthURL == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.config.HealthURL, nil)
	if err != nil {
		return
	}
	resp, err := o.health.Do(req)
	if err != nil {
		out.Application = "dependency_unavailable"
		out.ControllerDetail = "Application health endpoint unavailable."
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(body) > 4096 || resp.StatusCode != http.StatusOK {
		out.Application = "failed"
		out.ControllerDetail = "Application health probe did not succeed."
		return
	}
	// Refuse a torn observation if any object changed during the health check.
	for _, item := range []struct {
		kind, ns, name string
		first          *kubeapi.FluxObservation
	}{
		{"GitRepository", o.config.SourceNamespace, o.config.SourceName, source},
		{"Kustomization", o.config.KustomizationNamespace, o.config.KustomizationName, k},
		{"HelmRelease", r.Namespace, r.Release, h}, {"GitRepository", r.Namespace, r.Release, chart}, {"Deployment", r.Namespace, r.Release, d},
	} {
		again := get(item.kind, item.ns, item.name)
		if again == nil {
			out.Application = "unknown"
			return
		}
		if again.Metadata.ResourceVersion != item.first.Metadata.ResourceVersion || again.Metadata.UID != item.first.Metadata.UID {
			out.Reconciliation = "unknown"
			out.Application = "unknown"
			out.ControllerDetail = "Resources changed while observing; refresh status."
			return
		}
	}
	out.Application = "ready"
	out.ControllerDetail = "Exact source, current Helm/workload generations and configured application health probe passed."
}
func safeRevision(s string) string {
	if len(s) != 52 || !strings.HasPrefix(s, "deploy@sha1:") {
		return ""
	}
	for _, c := range strings.TrimPrefix(s, "deploy@sha1:") {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return ""
		}
	}
	return s
}

func chartRevisionMatches(revision, sha string) bool {
	if len(sha) != 40 {
		return false
	}
	if revision == "sha1:"+sha {
		return true
	}
	parts := strings.Split(revision, "@sha1:")
	return len(parts) == 2 && parts[0] != "" && parts[1] == sha
}
