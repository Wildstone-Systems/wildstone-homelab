// Package provider defines the external-resource controller contract
// (ADR-W004 A5): resources whose truth lives in a third-party control plane
// (Cloudflare DNS, later Tailscale) implement Provider instead of going
// through Wardstone. The reconcile loop treats providers identically to
// Wardstone-applied kinds — same diff -> plan -> apply -> health-gate.
package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// Provider is the external-resource controller contract (ADR-W004 A5).
// Implementations must satisfy the drift/partial-failure/rate-limit
// contract documented there:
//
//   - Drift: Observe returns the live record; diff detects out-of-band
//     changes.
//   - Partial failure: a Create that half-succeeds (record made, read-back
//     fails) must be idempotent on retry (Create-or-Update by unique key).
//   - Rate limits: retries on HTTP 429 with backoff honoring Retry-After;
//     after a bounded number of attempts it returns a *RateLimited error
//     the reconciler treats as retryable-later, not fatal.
//   - Transient server errors: an HTTP 5xx from the control plane is
//     returned as a *Transient error; the reconciler retries these too
//     (same bounded attempts/backoff as RateLimited) before treating it as
//     fatal.
type Provider interface {
	// Observe returns the live state of r's identity (kind-specific
	// idempotency key, e.g. zone+name+type for DnsRecord), whether it
	// exists, and any error. exists=false, err=nil means "confirmed
	// absent" — the normal case for a resource not yet created.
	Observe(ctx context.Context, r resource.Resource) (observed *resource.Resource, exists bool, err error)
	// Create creates r in the external control plane. Implementations
	// must make this idempotent on the resource's identity key: a retry
	// after a partial failure (e.g. the create succeeded but the caller
	// never learned that) must never produce a duplicate — it should
	// converge to a single record matching r's spec.
	Create(ctx context.Context, r resource.Resource) error
	// Update updates the existing external record matching r's identity
	// key to match r's spec.
	Update(ctx context.Context, r resource.Resource) error
	// Delete removes the external record matching r's identity key.
	Delete(ctx context.Context, r resource.Resource) error
}

// RateLimited is the typed error a Provider returns when it has exhausted
// its bounded internal retry/backoff budget against a rate-limited control
// plane (ADR-W004 A5). The reconciler treats this as retryable-later, not
// fatal: it must not be surfaced as a permanent Incident on first sight,
// only after the reconciler's own retries are also exhausted.
type RateLimited struct {
	// RetryAfter is the control plane's hint for how long to wait before
	// trying again, if known (zero if not provided).
	RetryAfter time.Duration
	// Op is a short human label for what was being attempted, e.g.
	// "create dns_record" — for logging/incident text only.
	Op string
	// Err is the underlying error (if any) that triggered the exhaustion,
	// e.g. the last HTTP 429 response's body.
	Err error
}

func (e *RateLimited) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("provider: %s: rate limited (retry-after %s): %v", e.Op, e.RetryAfter, e.Err)
	}
	return fmt.Sprintf("provider: %s: rate limited (retry-after %s)", e.Op, e.RetryAfter)
}

func (e *RateLimited) Unwrap() error { return e.Err }

// IsRateLimited reports whether err is (or wraps) a *RateLimited error.
func IsRateLimited(err error) bool {
	var rl *RateLimited
	return errors.As(err, &rl)
}

// Transient is the typed error a Provider returns for a retryable
// server-side failure — e.g. an HTTP 5xx from the control plane — that is
// not a rate limit but is still safe to retry: a blip in the remote
// service, not a permanent rejection of the request. The reconciler
// retries these with the same bounded attempts/backoff it uses for
// RateLimited before surfacing an Incident.
type Transient struct {
	// Op is a short human label for what was being attempted, e.g.
	// "create dns_record" — for logging/incident text only.
	Op string
	// Err is the underlying error that triggered this classification, e.g.
	// the HTTP 5xx response's body.
	Err error
}

func (e *Transient) Error() string {
	return fmt.Sprintf("provider: %s: transient failure: %v", e.Op, e.Err)
}

func (e *Transient) Unwrap() error { return e.Err }

// IsTransient reports whether err is (or wraps) a *Transient error.
func IsTransient(err error) bool {
	var t *Transient
	return errors.As(err, &t)
}
