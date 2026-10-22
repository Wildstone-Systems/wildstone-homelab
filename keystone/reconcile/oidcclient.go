package reconcile

// OidcClient applier (ADR-W005 A3/S3a): render an Authelia
// identity_providers.oidc.clients fragment and WriteManagedFile it to a
// managed Authelia path, one file per client. The batched-reload seam
// (exactly one Authelia reload per plan, no matter how many OidcClient
// steps it contains) lives in reconcile.go's Reconcile loop, not here —
// see runAutheliaReload and the anyOidcApplied bookkeeping there.

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// oidcClientFragmentPath returns the managed-file path an OidcClient
// resource's Authelia fragment is rendered to:
// <managedPrefix>/authelia/oidc-clients/<name>.yaml (ADR-W005 A3/B1). Like
// routeFragmentPath, this is keyed by the resource's Metadata.Name, not a
// spec field — observe needs a stable identifier independent of the spec
// (in particular, for the "no longer desired" case, where no want.Spec is
// available at all). By convention packages set Metadata.Name ==
// OidcClientSpec.ClientId, so in practice the file is named after the
// client_id anyway.
func oidcClientFragmentPath(managedPrefix, name string) string {
	return path.Join(managedPrefix, "authelia", "oidc-clients", name+".yaml")
}

// renderOidcClientFragment deterministically renders an OidcClientSpec to
// an Authelia identity_providers.oidc.clients YAML fragment, matching the
// shape of testing/fixtures/immich/authelia-oidc-client.yml. Determinism
// matters for the same reason it does for Route (see route.go): the
// observe step byte-compares this against the live fragment to decide
// NoOp vs Update, so nothing here may vary run-to-run (all inputs are
// ordered slices, never maps).
//
// client_secret is NEVER the real secret — packages carry only a secret
// *reference* (ADR-W005 A2), and this renders a clearly-labeled
// placeholder that embeds that reference, not a resolved value. Only
// Wardstone resolves secret references to real values at materialization,
// out of this package's scope entirely.
func renderOidcClientFragment(spec *resource.OidcClientSpec) []byte {
	var b strings.Builder
	b.WriteString("# managed by wildstone-keystone — do not edit\n")
	b.WriteString("identity_providers:\n")
	b.WriteString("  oidc:\n")
	b.WriteString("    clients:\n")
	fmt.Fprintf(&b, "      - client_id: %s\n", spec.ClientId)
	fmt.Fprintf(&b, "        client_name: %s\n", spec.ClientName)
	if spec.Public {
		b.WriteString("        public: true\n")
	} else {
		fmt.Fprintf(&b, "        client_secret: DIGEST-PLACEHOLDER:%s\n", spec.SecretRef)
		b.WriteString("        public: false\n")
	}
	fmt.Fprintf(&b, "        authorization_policy: %s\n", spec.AuthPolicy)
	b.WriteString("        redirect_uris:\n")
	for _, u := range spec.RedirectUris {
		fmt.Fprintf(&b, "          - %s\n", u)
	}
	b.WriteString("        scopes:\n")
	for _, s := range spec.Scopes {
		fmt.Fprintf(&b, "          - %s\n", s)
	}
	return []byte(b.String())
}

// applyOidcClientStep writes (or, on Delete, clears) an OidcClient's
// Authelia fragment. It does NOT itself trigger an Authelia reload — that
// is batched once per plan by the caller (Reconcile), after every
// OidcClient step in the plan has applied; see runAutheliaReload.
//
// Health gate (ADR-W005 A3/S3a): "fragment present + equal" — a
// ReadManagedFile byte-compare against the just-written content. This does
// NOT wait for a reload to happen, since the reload is deliberately
// deferred/batched; the gate only confirms the write itself landed and
// stuck.
func (r *Reconciler) applyOidcClientStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource) StepResult {
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}
	fragPath := oidcClientFragmentPath(r.ManagedPrefix, step.Name)

	// Capture prior bytes so a health-gate failure can restore them, same
	// posture as Route's reverse (ADR-W004 A4 step 5).
	var priorContent []byte
	if priorRes, priorErr := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ReadManagedFile{
		ReadManagedFile: &wsv1.ReadManagedFile{Path: fragPath},
	}}); priorErr == nil && priorRes.GetError() == nil {
		priorContent = priorRes.GetReadManagedFile().GetContent()
	}

	var content []byte
	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		want, ok := desiredByKey[key]
		if !ok || want.OidcClient == nil {
			sr.Error = "internal: no desired OidcClient spec for " + step.Name
			return sr
		}
		content = renderOidcClientFragment(want.OidcClient)
	case plan.ActionDelete:
		// No delete-file op exists (only WriteManagedFile/ReadManagedFile)
		// — same pattern as Route's delete.
		content = []byte{}
	default:
		sr.Error = "unexpected action " + step.Action.String()
		return sr
	}

	res, err := r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
		WriteManagedFile: &wsv1.WriteManagedFile{Path: fragPath, Content: content},
	}})
	if err != nil {
		sr.Error = err.Error()
		return sr
	}
	if oe := res.GetError(); oe != nil {
		sr.Error = fmt.Sprintf("%s: %s", oe.GetCode(), oe.GetMessage())
		return sr
	}
	sr.Applied = true

	if step.Action == plan.ActionDelete {
		checkRes, checkErr := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ReadManagedFile{
			ReadManagedFile: &wsv1.ReadManagedFile{Path: fragPath},
		}})
		sr.HealthOK = checkErr == nil && checkRes.GetError() == nil && len(checkRes.GetReadManagedFile().GetContent()) == 0
		if sr.HealthOK {
			r.markManaged(nodeID, key, false)
		} else {
			r.restoreOidcClientFragment(ctx, nodeID, fragPath, priorContent)
			sr.Reversed = true
		}
		return sr
	}

	if r.probeOidcClientFragment(ctx, nodeID, fragPath, content) {
		sr.HealthOK = true
		r.markManaged(nodeID, key, true)
		return sr
	}
	r.restoreOidcClientFragment(ctx, nodeID, fragPath, priorContent)
	sr.Reversed = true
	return sr
}

// probeOidcClientFragment health-gates an OidcClient Create/Update:
// ReadManagedFile the just-written path and byte-compare against want.
func (r *Reconciler) probeOidcClientFragment(ctx context.Context, nodeID, fragPath string, want []byte) bool {
	attempts, backoff := r.healthGateParams()
	for i := 0; i < attempts; i++ {
		res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ReadManagedFile{
			ReadManagedFile: &wsv1.ReadManagedFile{Path: fragPath},
		}})
		if err == nil && res.GetError() == nil && routeFragmentEqual(res.GetReadManagedFile().GetContent(), want) {
			return true
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(backoff):
			}
		}
	}
	return false
}

func (r *Reconciler) restoreOidcClientFragment(ctx context.Context, nodeID, fragPath string, content []byte) {
	r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{ //nolint:errcheck
		WriteManagedFile: &wsv1.WriteManagedFile{Path: fragPath, Content: content},
	}})
}

// runAutheliaReload invokes the ReloadAutheliaFn seam exactly once, called
// by Reconcile after its whole apply loop (all steps, of every kind, in
// this plan) has finished — see the anyOidcApplied bookkeeping in
// reconcile.go. This is the batching mechanism ADR-W005 B3 requires: N
// OidcClient Create/Update steps in one plan write N fragments but trigger
// exactly one reload, never one reload per client (which would otherwise
// restart Authelia N times for a single logical change).
//
// nil ReloadAutheliaFn (the default) is a documented no-op, same posture
// as Route's ReloadFn: the real Authelia reload/restart call lands in a
// later E-series work package.
func (r *Reconciler) runAutheliaReload(ctx context.Context) error {
	if r.ReloadAutheliaFn == nil {
		return nil
	}
	return r.ReloadAutheliaFn(ctx)
}
