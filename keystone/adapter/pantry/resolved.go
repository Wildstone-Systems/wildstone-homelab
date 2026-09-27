package pantry

// A PantryOS whose address and key are looked up on every call
// (ADR-W042 A6, #384), so pairing — or re-pairing, or unpairing — takes
// effect without restarting Keystone.

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
)

// ErrNotConnected is a PantryOS that has not been paired or configured:
// not an outage, a setup step nobody has done yet, and callers say so.
var ErrNotConnected = errors.New("pantry: pantryos is not connected")

// Resolved is an Upstream whose connection comes from Resolve each call.
// Resolve reports ok=false when nothing is connected.
type Resolved struct {
	Resolve func() (h HTTP, ok bool, err error)
}

func (r *Resolved) Name() string { return "pantryos" }

func (r *Resolved) current() (*HTTP, error) {
	h, ok, err := r.Resolve()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotConnected
	}
	return &h, nil
}

func (r *Resolved) Capabilities(ctx context.Context) ([]adapter.Capability, error) {
	h, err := r.current()
	if err != nil {
		out := make([]adapter.Capability, 0, len(knownCapabilities))
		for _, c := range knownCapabilities {
			c.Version, c.Available, c.Detail = Contract, false, err.Error()
			out = append(out, c)
		}
		return out, nil
	}
	return h.Capabilities(ctx)
}

func (r *Resolved) Query(ctx context.Context, capability string, request json.RawMessage) (adapter.Answer, error) {
	h, err := r.current()
	if err != nil {
		return adapter.Answer{}, err
	}
	return h.Query(ctx, capability, request)
}

func (r *Resolved) Act(ctx context.Context, capability string, request json.RawMessage) (adapter.Answer, error) {
	h, err := r.current()
	if err != nil {
		return adapter.Answer{}, err
	}
	return h.Act(ctx, capability, request)
}

var _ adapter.Upstream = (*Resolved)(nil)
