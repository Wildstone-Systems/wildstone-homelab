package runtimedb

import "time"

// DeploymentStatus separates local history/delivery from optional controller observations.
// These fields are fresh observations, not persisted desired state or grants.
type DeploymentStatus struct {
	AttemptCode                     string     `json:"attemptCode,omitempty"`
	Phase                           string     `json:"phase"`
	Detail                          string     `json:"detail"`
	ProductRevision                 string     `json:"productRevision"`
	PreparedRevision                string     `json:"preparedRevision,omitempty"`
	PromotedRevision                string     `json:"promotedRevision,omitempty"`
	Delivery                        string     `json:"delivery"`
	MirrorRevision                  string     `json:"mirrorRevision,omitempty"`
	Reconciliation                  string     `json:"reconciliation"`
	Application                     string     `json:"application"`
	ControllerStage                 string     `json:"controllerStage,omitempty"`
	SourceGeneration                int64      `json:"sourceGeneration,omitempty"`
	SourceObservedGeneration        int64      `json:"sourceObservedGeneration,omitempty"`
	KustomizationGeneration         int64      `json:"kustomizationGeneration,omitempty"`
	KustomizationObservedGeneration int64      `json:"kustomizationObservedGeneration,omitempty"`
	ControllerDetail                string     `json:"controllerDetail,omitempty"`
	SourceRevision                  string     `json:"sourceRevision,omitempty"`
	ReconciledRevision              string     `json:"reconciledRevision,omitempty"`
	ControllerObservedAt            *time.Time `json:"controllerObservedAt,omitempty"`
	ControllerExpiresAt             *time.Time `json:"controllerExpiresAt,omitempty"`
	HelmGeneration                  int64      `json:"helmGeneration,omitempty"`
	HelmObservedGeneration          int64      `json:"helmObservedGeneration,omitempty"`
	WorkloadGeneration              int64      `json:"workloadGeneration,omitempty"`
	WorkloadObservedGeneration      int64      `json:"workloadObservedGeneration,omitempty"`
	ObservedAt                      time.Time  `json:"observedAt"`
}
