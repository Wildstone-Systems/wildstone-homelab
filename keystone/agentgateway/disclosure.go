package agentgateway

// The processing-class disclosure filter (ADR-W039 §3.5 rule 3,
// #345): the gateway-side replacement for
// keystone/butlerlive.privacyProvider (C10). A model's own
// process can be wrapped; an external runtime's cannot — Wildstone has
// no way to verify which model it calls — so instead of guarding the
// inference boundary, this file guards every boundary the GATEWAY
// itself controls: what it hands the runtime as a Manifest, an Invoke
// result or a Recall hit (gateway.go, invoke.go, memory.go).
//
// Deliveries (work.go's Gateway.Deliveries and api/handlers_agent.go's
// agentDeliveries route) are filtered too, by the SAME per-record rule
// and the SAME agentapi.Delivery a runtime pulls either way: FilterDelivery
// below is the one function both readers call (ADR-W039 §3.5 rule 3,
// #345) — agentgateway cannot import keystone/api, but keystone/api may
// import keystone/agentgateway, so this is the shared side. A withheld
// delivery is still delivered, so it can still be acked: only its
// Payload is replaced with a refusal marker, never its id, kind, cursor
// or expiry, which is what keeps ack idempotent and what keeps
// TestDeliveryClientIsByteIdenticalInProcessAndOverHTTP passing — both
// readers run the identical filter over the identical row.
//
// The rule is keyed on the Owner-declared processing class on
// butler_runtimes (never a runtime's own claim — Manifest, Invoke and
// Heartbeat carry no such field, and this file never reads one off the
// wire), and enforced with the SAME per-record rule and the SAME error
// the provider wrapper used: runtimedb.PrivacyPolicy.Check and
// runtimedb.ErrPrivacy, so the wording an Owner sees never depends on
// where the refusal happened. Undeclared (empty) fails closed exactly
// like "cloud": a runtime the Owner never declared local_only is not
// entitled to local_only content. Reading GetButlerRuntime itself fails
// (a row this deployment predates, or a real database error) fails
// closed the identical way, since a delivery reader that cannot ask the
// question must answer it exactly like the row said "cloud".
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
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
	return disclosureClassFor(g.DB, butlerSeq)
}

// disclosureClassFor is disclosureClass's own DB-only shape, so
// FilterDelivery below can resolve it without a *Gateway — the same
// read api/handlers_agent.go's HTTP route needs and has only a
// *runtimedb.DB (s.DB) to make it with.
func disclosureClassFor(db *runtimedb.DB, butlerSeq int64) (agentapi.ProcessingClass, error) {
	rt, err := db.GetButlerRuntime(butlerSeq)
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

// beginDisclosureTurn restores the originating task/session provenance as well
// as tracking this call's reads. Call-scoped tracking alone loses workspace
// source restrictions on retries and resumed tasks (ADR-W047 / #475).
func (g *Gateway) beginDisclosureTurn(ctx context.Context, class agentapi.ProcessingClass, d runtimedb.MemoryDestination, task string) (context.Context, *runtimedb.PrivacyTurn, error) {
	ctx, p, err := g.DB.BeginPrivacyTurn(ctx, disclosureModel(class), d, "")
	if err != nil {
		return ctx, nil, err
	}
	if err = p.SetTask(task); err != nil {
		return ctx, nil, err
	}
	return ctx, p, nil
}

// disclosureRefusalError wraps a per-record refusal with the calling
// surface's name, keeping runtimedb.ErrPrivacy as the wrapped sentinel
// so `errors.Is(err, runtimedb.ErrPrivacy)` — privacy_test.go:32's own
// assertion shape — holds identically at the gateway boundary.
func disclosureRefusalError(surface string, err error) error {
	return fmt.Errorf("agentgateway: %s: %w", surface, err)
}

// FilterDeliveries applies FilterDelivery to a batch, in order — the
// one call both work.go's Gateway.Deliveries and
// api/handlers_agent.go's agentDeliveries route make on the rows
// ListUnackedAgentDeliveries hands back, so the two readers can never
// disagree about which of them are withheld.
func FilterDeliveries(db *runtimedb.DB, rows []runtimedb.AgentDelivery) []runtimedb.AgentDelivery {
	out := make([]runtimedb.AgentDelivery, len(rows))
	for i, row := range rows {
		out[i] = FilterDelivery(db, row)
	}
	return out
}

// FilterDelivery is the delivery-boundary half of this file's
// processing-class disclosure filter (see the file doc comment): one
// agent_deliveries row, read back exactly as ListUnackedAgentDeliveries
// left it, with its Payload replaced by a refusal marker when the
// content it carries is not for this Agent's declared processing class.
//
// The class is read fresh from butler_runtimes on every call — never
// cached from when the row was recorded — so an Owner who tightens a
// runtime's declaration after a delivery was queued sees it enforced on
// the very next read, exactly like every other boundary this file
// guards. A row is never dropped and never refused outright: a withheld
// delivery keeps its id, kind, cursor and expiry, so a runtime that
// cannot see its payload can still ack it and move its cursor past it.
func FilterDelivery(db *runtimedb.DB, row runtimedb.AgentDelivery) runtimedb.AgentDelivery {
	class, err := disclosureClassFor(db, row.Butler)
	if err != nil {
		class = agentapi.ProcessingCloud
	}
	if perr := deliveryPrivacyCheck(db, row, disclosureModel(class)); perr != nil {
		row.Payload = deliveryRefusalPayload(perr)
	}
	return row
}

// deliveryPayloadFields is the ONE best-effort read of a delivery's
// opaque payload this filter needs — the same "handle" field
// deliveriesFromRows/deliveriesFor already read (never a claim the
// table itself makes about what the payload holds), plus
// proposal_outcome's own "plan" field (recordProposalOutcomeDelivery,
// api/handlers_plans.go).
type deliveryPayloadFields struct {
	Handle string `json:"handle"`
	Plan   string `json:"plan"`
}

func parseDeliveryPayload(payload string) deliveryPayloadFields {
	var f deliveryPayloadFields
	_ = json.Unmarshal([]byte(payload), &f)
	return f
}

// deliveryPrivacyCheck runs the existing per-record rule this delivery's
// content is already governed by, exactly as Manifest, Invoke and
// Recall run it at their own boundaries:
//
//   - proposal_outcome carries a plan id; runtimedb.CheckPlanPrivacy is
//     the SAME check GET /api/v1/plans/{id} runs before showing an
//     Owner a plan (api/handlers_plans.go), applied here before handing
//     its outcome to the runtime that filed it.
//   - turn, subscription and escalation carry a task handle (escalation
//     deliveries — cmd/keystone/reviewed_escalator.go — do not today,
//     so this is a no-op for them; the case is here so a future one
//     that does start gets the identical check for free); the task id
//     ahead of the handle's own "." is enough to resolve its session
//     without validating the handle's secret — this filter only ever
//     narrows disclosure, it grants nothing — and runtimedb.
//     CheckSessionPrivacy is the SAME check a resumed conversation's
//     own history is checked with (butlerlive.privacyProvider, C10).
//
// A row this filter cannot resolve at all — a malformed or unknown
// plan id, a task or session the delivery outlived — is left alone
// rather than withheld: there is no restriction to enforce once the
// content it would have been checked against no longer exists, the
// same "best effort" recordProposalOutcomeDelivery and
// reofferWaitingTasks already give a failure that does not block the
// delivery itself.
func deliveryPrivacyCheck(db *runtimedb.DB, row runtimedb.AgentDelivery, model string) error {
	fields := parseDeliveryPayload(row.Payload)
	destination := runtimedb.MemoryDestination{}
	switch row.Kind {
	case agentapi.DeliveryProposalOutcome:
		if fields.Plan == "" {
			return nil
		}
		return privacyRestriction(db.CheckPlanPrivacy(fields.Plan, model, destination))
	case agentapi.DeliveryTurn, agentapi.DeliverySubscription, agentapi.DeliveryEscalation:
		if fields.Handle == "" {
			return nil
		}
		taskID, _, _ := strings.Cut(fields.Handle, ".")
		if taskID == "" {
			return nil
		}
		task, err := db.GetButlerTask(taskID)
		if err != nil {
			return nil
		}
		// The queue recipient must be the Agent assigned to this task;
		// a payload handle cannot redirect another Agent's private history.
		agentName, err := db.TaskButler(task.ID)
		if err != nil {
			return runtimedb.ErrPrivacy
		}
		agent, err := db.GetButler(agentName)
		if err != nil || agent.Seq != row.Butler {
			return runtimedb.ErrPrivacy
		}
		destination = memoryDestinationForTask(task)
		if destination.ConnectionRef != 0 {
			at, err := db.ChatConnectionBySeq(destination.ConnectionRef)
			if err != nil || at.Butler != agentName || at.Name() != task.OriginConnection {
				return runtimedb.ErrPrivacy
			}
			// Binding is a delivery admission gate for every source type, not
			// merely a disclosure-profile exception for recalled memories.
			bound, err := db.HasLivePersonChatBinding(at.Seq, task.Owner)
			if err != nil || !bound {
				return runtimedb.ErrPrivacy
			}
		}
		// Workspace reads can precede the first session. A resolved task
		// must carry its own provenance through delivery, and a failed
		// policy lookup must not turn a known task into unrestricted data.
		if err := db.CheckTaskDeliveryPrivacy(task.ID, model, destination); err != nil {
			return runtimedb.ErrPrivacy
		}
		return nil
	default:
		return nil
	}
}

// privacyRestriction narrows an underlying check's error to the one
// this filter withholds a delivery for. A malformed id or a "no such
// plan/session" is not runtimedb.ErrPrivacy and is treated as nothing
// to enforce (deliveryPrivacyCheck's own doc comment); any real
// database error underneath surfaces the exact same way, since a
// delivery reader that cannot evaluate the rule has nothing else to do
// but leave the row alone.
func privacyRestriction(err error) error {
	if errors.Is(err, runtimedb.ErrPrivacy) {
		return err
	}
	return nil
}

// deliveryRefusalRow is what a withheld delivery's Payload becomes: no
// trace of the content itself, only that it was withheld and why, in
// the SAME runtimedb.ErrPrivacy wording every other gateway boundary
// refuses with (disclosureRefusalError, "delivery" naming this one).
type deliveryRefusalRow struct {
	Withheld bool   `json:"withheld"`
	Reason   string `json:"reason"`
}

func deliveryRefusalPayload(err error) string {
	body, merr := json.Marshal(deliveryRefusalRow{Withheld: true, Reason: disclosureRefusalError("delivery", err).Error()})
	if merr != nil {
		return `{"withheld":true}`
	}
	return string(body)
}
