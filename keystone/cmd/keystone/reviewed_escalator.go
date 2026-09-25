package main

// ReviewedEscalator (ADR-W039 Phase 4, C5/C6, #340/#341): a reviewed
// routine's escalation is a tool-free run request delivered to the
// assigned Agent's own runtime — recorded as an agent_deliveries row of
// kind `escalation` regardless of runtime type, since the delivery
// itself is the audit record of what was asked, whether or not
// anything is waiting synchronously on the answer. For the native
// runtime it is additionally served in-process, right here, by the same
// Phraser (keystone/agentruntime/legacy) and the same wording/fallback
// contract the engine has always had (never any tools, the rule's own
// wording used whenever this returns an error). A non-native runtime
// has no synchronous answer path yet — a future bridge answers the
// recorded delivery instead — so it falls back exactly as an
// unreachable Agent already does.
//
// Extracted to its own function, rather than inlined where it is wired
// in serve(), so it is unit-testable without standing up the whole
// server.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime/legacy"
	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// newReviewedEscalator builds the closure automation.Engine.RunReviewed
// calls. providerFor is butlerProviderFor, injected so a test can supply
// a fixed provider without a real Route/model.
func newReviewedEscalator(db *runtimedb.DB, log *slog.Logger, providerFor func(runtimedb.Butler) (provider.Provider, error)) func(context.Context, string, automation.EscalationRequest) (string, error) {
	return func(ctx context.Context, agent string, req automation.EscalationRequest) (string, error) {
		b, err := db.GetButler(agent)
		if err != nil || !b.Reachable() {
			return "", fmt.Errorf("assigned Agent %s is unavailable", agent)
		}
		rt, err := db.GetButlerRuntime(b.Seq)
		if err != nil {
			return "", fmt.Errorf("assigned Agent %s has no runtime record: %v", agent, err)
		}
		if payload, merr := json.Marshal(map[string]any{
			"rule": req.Rule, "instruction": req.Instruction, "fallback": req.Fallback,
		}); merr == nil {
			if _, derr := db.RecordAgentDelivery(b.Seq, agentapi.DeliveryEscalation, string(payload)); derr != nil {
				log.Error("recording an escalation delivery", "agent", agent, "err", derr)
			}
		}
		if rt.Type != agentapi.RuntimeNative {
			return "", fmt.Errorf("assigned Agent %s's runtime does not serve escalations in-process", agent)
		}
		p, err := providerFor(b)
		if err != nil || p == nil {
			return "", fmt.Errorf("assigned Agent %s has no available provider: %v", agent, err)
		}
		facts := make([]legacy.Fact, 0, len(req.Facts))
		for _, f := range req.Facts {
			facts = append(facts, legacy.Fact{Label: f.Label, Content: f.Content})
		}
		phraser := &legacy.Phraser{Provider: p, Model: b.ProviderModel,
			Record: func(_ context.Context, providerName, prompt, reply string) error {
				_, err := db.InsertButlerSession(runtimedb.ButlerSession{
					Principal: butlerSessionPrincipal, ActingFor: automation.Source,
					Butler: b.Name, AgentID: b.Seq, Provider: providerName,
					Mode: runtimedb.ButlerModeFull, Surface: b.Surface,
					FinalText:      redact.String(reply),
					TranscriptJSON: redact.String(`{"kind":"reviewed-routine-escalation","prompt":` + strconv.Quote(prompt) + `}`),
				})
				return err
			}}
		return phraser.Phrase(ctx, legacy.PhraseRequest{
			Rule: req.Rule, Instruction: req.Instruction, Fallback: req.Fallback, Facts: facts,
		})
	}
}
