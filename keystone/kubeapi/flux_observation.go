package kubeapi

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
)

// FluxObservation is a read-only, minimal view. Controller messages and workload
// environment/Secret data are never returned through the product API.
type FluxObservation struct {
	Metadata ObjectMeta     `json:"metadata"`
	Spec     map[string]any `json:"spec"`
	Status   struct {
		ObservedGeneration int64           `json:"observedGeneration"`
		Conditions         []FluxCondition `json:"conditions"`
		Artifact           *struct {
			Revision string `json:"revision"`
		} `json:"artifact"`
		LastAppliedRevision string `json:"lastAppliedRevision"`
		Replicas            int32  `json:"replicas"`
		UpdatedReplicas     int32  `json:"updatedReplicas"`
		ReadyReplicas       int32  `json:"readyReplicas"`
		AvailableReplicas   int32  `json:"availableReplicas"`
	} `json:"status"`
}
type FluxCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	ObservedGeneration int64  `json:"observedGeneration"`
	Reason             string `json:"reason"`
}

var observationName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// ObserveFlux performs exactly one named GET. No lists, logs, proxy, Secrets,
// arbitrary paths or mutation verbs are supported by this observation surface.
func (c *Client) ObserveFlux(ctx context.Context, kind, namespace, name string) (*FluxObservation, error) {
	if !observationName.MatchString(namespace) || !observationName.MatchString(name) {
		return nil, fmt.Errorf("kubeapi: invalid observation identity")
	}
	prefixes := map[string]string{
		"GitRepository": "/apis/source.toolkit.fluxcd.io/v1",
		"Kustomization": "/apis/kustomize.toolkit.fluxcd.io/v1",
		"HelmRelease":   "/apis/helm.toolkit.fluxcd.io/v2",
		"Deployment":    "/apis/apps/v1",
	}
	plurals := map[string]string{"GitRepository": "gitrepositories", "Kustomization": "kustomizations", "HelmRelease": "helmreleases", "Deployment": "deployments"}
	prefix, ok := prefixes[kind]
	if !ok {
		return nil, fmt.Errorf("kubeapi: unsupported observation kind")
	}
	var out FluxObservation
	err := c.do(ctx, http.MethodGet, prefix+"/namespaces/"+namespace+"/"+plurals[kind]+"/"+name, nil, &out)
	if err != nil {
		return nil, err
	}
	if out.Metadata.Name != name || out.Metadata.Namespace != namespace || out.Metadata.UID == "" || out.Metadata.Generation < 1 || out.Metadata.ResourceVersion == "" {
		return nil, fmt.Errorf("kubeapi: observation identity unavailable")
	}
	return &out, nil
}
