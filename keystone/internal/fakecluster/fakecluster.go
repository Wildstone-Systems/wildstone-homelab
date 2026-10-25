// Package fakecluster is a recording stand-in for the Wardstone leader
// (W050 slice 2c, #158). It records every typed cluster request and
// answers with a converged result, so reconciler tests can assert what
// would have been sent to a cluster without one existing.
package fakecluster

import (
	"context"
	"sync"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/reconcile"
)

// Cluster records requests in order. The zero value is ready to use.
type Cluster struct {
	mu       sync.Mutex
	requests []reconcile.ClusterRequest
	// Node is reported as where every workload runs.
	Node string
}

var _ reconcile.ClusterApplier = (*Cluster)(nil)

// Apply records req and reports the workload converged.
func (c *Cluster) Apply(_ context.Context, req reconcile.ClusterRequest) (reconcile.ClusterResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, req)
	replicas := 1
	if req.Workload != nil && req.Workload.Replicas > 0 {
		replicas = req.Workload.Replicas
	}
	return reconcile.ClusterResult{Generation: int64(len(c.requests)), Replicas: replicas, ReadyReplicas: replicas, Node: c.Node}, nil
}

// Requests returns what was recorded, oldest first.
func (c *Cluster) Requests() []reconcile.ClusterRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]reconcile.ClusterRequest(nil), c.requests...)
}
