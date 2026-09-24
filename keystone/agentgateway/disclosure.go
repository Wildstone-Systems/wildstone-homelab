package agentgateway

// The processing-class disclosure filter (ADR-W039 §3.5 rule 3, §7.2
// Phase 5, #345): the gateway-side replacement for
// keystone/butlerlive.privacyProvider (C10, ADR §2.1). A model's own
// process can be wrapped; an external runtime's cannot — Wildstone has
// no way to verify which model it calls — so instead of guarding the
// inference boundary, this file guards every boundary the GATEWAY
// itself controls: what it hands the runtime as a Manifest, an Invoke
// result or a Recall hit (gateway.go, invoke.go, memory.go).
//
// Deliveries (work.go's Gateway.Deliveries) are deliberately NOT
// filtered here: api/handlers_agent.go's HTTP route re-reads
// agent_deliveries directly rather than calling this Gateway (its own
// doc comment on deliveriesFromRows explains why — agentgateway cannot
// import keystone/api), so a filter added on only one of the two
// readers would make them disagree, which is exactly what
// TestDeliveryClientIsByteIdenticalInProcessAndOverHTTP exists to catch.
// Closing that gap needs the identical check added to both readers,
// which is a keystone/api change outside this package's scope.
//
// The rule is keyed on the Owner-declared processing class on
// butler_runtimes (never a runtime's own claim — Manifest, Invoke and
// Heartbeat carry no such field, and this file never reads one off the
// wire), and enforced with the SAME per-record rule and the SAME error
// the provider wrapper used: runtimedb.PrivacyPolicy.Check and
// runtimedb.ErrPrivacy, so the wording an Owner sees never depends on
// where the refusal happened. Undeclared (empty) fails closed exactly
// like "cloud": a runtime the Owner never declared local_only is not
// entitled to local_only content.
import (
	"context"
	"fmt"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// disclosureClass resolves the Owner-declared processing class for the
// Agent behind butlerSeq. It never falls back to the Agent's assigned
// model or anything else Wildstone might infer: for a native runtime
// that value is written to the SAME row at registration time
// (ensureNativeRuntime, api/handlers_butler_runtime.go) precisely so
// this read stays a single column, and for an external runtime it is
// the Owner's own declaration (§3.5). An unrecognised or empty class —
// undeclared, or a row this deployment predates — is treated exactly
// like cloud, the safer of the two answers (§3.5's "undeclared fails
// closed").
func (g *Gateway) disclosureClass(butlerSeq int64) (agentapi.ProcessingClass, error) {
	rt, err := g.DB.GetButlerRuntime(butlerSeq)
	if err != nil {
		return "", err
	}
	if rt.ProcessingClass == agentapi.ProcessingLocalOnly {
		return agentapi.ProcessingLocalOnly, nil
	}
	return agentapi.ProcessingCloud, nil
}

// disclosureModel maps a processing class onto the model-kind
// vocabulary runtimedb.PrivacyPolicy.Check already speaks for the
// native runtime's own provider (runtimedb/privacy.go): local_only
// reads as ModelLocal, everything else as ModelCloud. This is the one
// translation that lets the gateway reuse the identical per-record rule
// and the identical runtimedb.ErrPrivacy wording the provider wrapper
// used, rather than a second, gateway-owned notion of "restricted".
func disclosureModel(class agentapi.ProcessingClass) string {
	if class == agentapi.ProcessingLocalOnly {
		return runtimedb.ModelLocal
	}
	return runtimedb.ModelCloud
}

// disclosureAllowed reports whether one memory record may reach a
// runtime whose declared processing class is class. It is a plain,
// stateless read of that record's own content_privacy row — no ledger,
// no accumulated turn history — so it answers identically whether the
// caller is the in-process native runtime or an external one polling
// over HTTP with no Go context of its own to carry state in.
func (g *Gateway) disclosureAllowed(seq int64, class agentapi.ProcessingClass, d runtimedb.MemoryDestination) error {
	return g.DB.CheckMemoryPrivacy(seq, disclosureModel(class), d)
}

// beginDisclosureTurn opens a call-scoped privacy turn for tracking
// what ONE Manifest or Invoke call discloses, reusing
// runtimedb.BeginPrivacyTurn/TrackMemoryPrivacy exactly as
// butlerlive.Serve did for a whole conversation — but scoped to this
// one call (session and task left unset) rather than the task's whole
// history, because the gateway boundary is being checked here, not the
// inference boundary a resumed conversation cares about. Any read a
// Toolset makes under the returned context — today, butlerstore/
// memory.go's recall and preamble seams — is tracked; a call that reads
// nothing privacy-tagged always passes its later Check().
func (g *Gateway) beginDisclosureTurn(ctx context.Context, class agentapi.ProcessingClass, d runtimedb.MemoryDestination) (context.Context, *runtimedb.PrivacyTurn, error) {
	return g.DB.BeginPrivacyTurn(ctx, disclosureModel(class), d, "")
}

// disclosureRefusalError wraps a per-record refusal with the calling
// surface's name, keeping runtimedb.ErrPrivacy as the wrapped sentinel
// so `errors.Is(err, runtimedb.ErrPrivacy)` — privacy_test.go:32's own
// assertion shape — holds identically at the gateway boundary.
func disclosureRefusalError(surface string, err error) error {
	return fmt.Errorf("agentgateway: %s: %w", surface, err)
}
