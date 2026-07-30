package reconcile

import (
	"context"

	"github.com/chickenrat17/wildstone-homelab/keystone/gateway"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// Applier sends one typed Wardstone operation to a node and returns its
// result. It is the seam between the Reconciler and the transport that
// carries operations to an agent (ADR-W004 B1): the real implementation
// wraps gateway.Gateway.Dispatch; tests can inject a fake that never
// touches the network.
//
// Provider-backed kinds (DnsRecord, S2c) do NOT go through Applier — they
// implement keystone/provider.Provider instead. Applier is exclusively the
// Wardstone-applied-kinds path (Container, Volume, Route).
type Applier interface {
	Apply(ctx context.Context, nodeID string, op *wsv1.Operation) (*wsv1.OpResult, error)
}

// GatewayApplier is the production Applier: a thin wrapper around
// gateway.Gateway.Dispatch. It exists (rather than using *gateway.Gateway
// directly as the Applier) so the Reconciler's dependency is the minimal
// Applier interface, keeping gateway.Gateway free of any reconcile-package
// awareness and letting tests substitute a fake with zero gateway
// involvement at all.
type GatewayApplier struct {
	Gateway *gateway.Gateway
}

// Apply dispatches op to nodeID via the wrapped Gateway. No changes to
// gateway.Gateway were needed or made for S2b — Dispatch's existing
// (ctx, nodeID, *wsv1.Operation) -> (*wsv1.OpResult, error) signature is
// exactly the Applier shape.
func (a *GatewayApplier) Apply(ctx context.Context, nodeID string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	return a.Gateway.Dispatch(ctx, nodeID, op)
}
