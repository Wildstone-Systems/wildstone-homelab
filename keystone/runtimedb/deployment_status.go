package runtimedb

import "time"

// DeploymentStatus separates local history/delivery from unobserved controllers.
// These fields are fresh observations, not persisted desired state or grants.
type DeploymentStatus struct {
	AttemptCode      string    `json:"attemptCode,omitempty"`
	Phase            string    `json:"phase"`
	Detail           string    `json:"detail"`
	ProductRevision  string    `json:"productRevision"`
	PreparedRevision string    `json:"preparedRevision,omitempty"`
	PromotedRevision string    `json:"promotedRevision,omitempty"`
	Delivery         string    `json:"delivery"`
	MirrorRevision   string    `json:"mirrorRevision,omitempty"`
	Reconciliation   string    `json:"reconciliation"`
	Application      string    `json:"application"`
	ObservedAt       time.Time `json:"observedAt"`
}
