package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// The Memory family (ADR-W039 §3.2, §3.5, Phase 3, #338). Today memory
// reaches a runtime two ways: as two household tools, recall and
// remember, on the family Toolset — bound to the session's Person,
// Butler and destination by the gateway factory (butlerstore.
// MemorySeams) and authorised per call like any other tool — and now
// also as this package's own Recall/Remember, for a runtime that wants
// structured hits rather than a model's rendered text. Both paths bind
// the identical MemoryReadContext from the same handle (BindMemoryRead,
// as the tool's seam does today) and run the identical authorisation
// gate (Toolset.Permits, which is where a group conversation's "memory
// is unavailable" and a deployment with memory turned off both live),
// so the two can never disagree about what is disclosed —
// memory_test.go pins exactly that.
//
// Processing-class filtering (§3.5 rule 3: withholding local_only
// records from a runtime not declared local_only) is Phase 5's
// gateway-side filter (ADR-W039 §8 Phase 5, "gateway-side local_only
// filtering by declared processing class"); it is not implemented here.

// Recall implements agentapi.Client: a structured search of Wildstone
// memory for the reader on the handle, disclosed exactly as the
// "recall" tool discloses it (§3.5 rule 3's first half — the read
// context and the authorisation gate — with the runtime narrowing by
// kind and limit rather than typing a tool call).
//
// A refusal — memory not wired, recall withheld from this session
// (a group conversation, a read-only session with no memory seam),
// nothing to search for — is returned as an error wrapping ErrRefused,
// because Recall's return has no Outcome to carry it, the same
// convention Remember below already uses.
func (g *Gateway) Recall(ctx context.Context, req agentapi.RecallRequest) ([]agentapi.MemoryHit, error) {
	s, err := g.open(ctx, req.Handle)
	if err != nil {
		if errors.Is(err, butlerstore.ErrHandleRefused) || errors.Is(err, ErrRefused) {
			return nil, fmt.Errorf("%w: %v", ErrRefused, err)
		}
		return nil, err
	}
	if g.DB == nil {
		return nil, fmt.Errorf("%w: the gateway is not wired", ErrRefused)
	}
	// The same gate the "recall" tool dispatches through
	// (HouseholdGateway.Permits → checkTool): a group conversation, a
	// deployment with no memory seam wired, or a read-only session with
	// recall withheld all answer here exactly as they answer a model's
	// own tool call, because it is the SAME call.
	if allowed, why := s.toolset.Permits("recall", s.readOnly); !allowed {
		return nil, fmt.Errorf("%w: %s", ErrRefused, why)
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, fmt.Errorf("%w: recall needs something to look for", ErrRefused)
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}
	// BindMemoryRead from the handle's own Person, Agent and
	// destination — never from the request, which carries neither
	// (§3.5's own "a runtime's own claim about who is asking is
	// ignored"). turn.MemoryDestination() is the exact destination
	// serve.go binds the tool's seam with (turnFor built this session's
	// turn from the same handle).
	read := g.DB.BindMemoryRead(s.hc.Person, s.hc.Butler.Name, s.turn.MemoryDestination())
	hits, err := g.DB.SearchIndex(runtimedb.IndexQuery{
		ReadContext: read, Reader: s.hc.Person, Butler: s.hc.Butler.Name, Text: text, Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("agentgateway: searching memory: %w", err)
	}
	out := make([]agentapi.MemoryHit, 0, len(hits))
	for _, h := range hits {
		if h.MemorySeq == 0 {
			continue
		}
		// GetMemory does not filter by permission and does not need
		// to: this seq came back from a query whose permission clause
		// was in the candidate SQL (butlerstore/memory.go's own note
		// on the same read), so the reader is already entitled to it —
		// this is where its scope, audience, kind and confidence come
		// from, none of which the index chunk itself carries.
		rec, err := g.DB.GetMemory(h.MemorySeq)
		if err != nil {
			continue
		}
		if req.Kind != "" && agentapi.MemoryKind(rec.Kind) != req.Kind {
			continue
		}
		out = append(out, agentapi.MemoryHit{
			Content: h.Content, Kind: agentapi.MemoryKind(rec.Kind), Scope: agentapi.MemoryScope(rec.Scope),
			Subject: rec.Subject, Source: h.SourceKind, Audience: agentapi.Audience(rec.Audience),
			Confidence: rec.Confidence, ObservedAt: rec.ObservedAt, MustVerifyLive: h.MustVerifyLive,
		})
	}
	return out, nil
}

// Remember implements agentapi.Client through the remember tool. The
// tool enforces §3.5 rule 1 today — personal or agent_local, never
// household; fact or preference, never an inference; the speaker's
// own audience — and refuses through the same authority seam every
// tool does. Household is refused a second time here, before the tool
// is even reached: §3.5 rule 1 is pinned once in butlerstore/memory.go
// (`remember cannot create Household memory`) and pinned again at the
// gateway boundary, because this is the layer an external runtime
// actually calls through, and provenance/audience/scope must never be
// something the WIRE gets to decide — they come from the handle (the
// tool's seam closes over them from the session) or, for scope, from
// the request narrowed to what the caller may write, never widened.
//
// A refusal is returned as an error wrapping ErrRefused, because
// Remember has no outcome to carry it.
func (g *Gateway) Remember(ctx context.Context, req agentapi.RememberRequest) error {
	if req.Scope == agentapi.MemoryScopeHousehold {
		return fmt.Errorf("%w: remember cannot create Household memory; promotion to Household is a Person-approved control (ADR-W039 §3.5 rule 1)", ErrRefused)
	}
	if req.Content == "" {
		return fmt.Errorf("%w: nothing to remember", ErrRefused)
	}
	args := map[string]any{"content": req.Content}
	if req.Scope != "" {
		args["scope"] = string(req.Scope)
	}
	if req.Kind != "" {
		args["kind"] = string(req.Kind)
	}
	if req.Subject != "" {
		args["subject"] = req.Subject
	}
	body, err := json.Marshal(args)
	if err != nil {
		return err
	}
	res, err := g.Invoke(ctx, agentapi.InvokeRequest{Handle: req.Handle, Tool: "remember", Args: body})
	if err != nil {
		return err
	}
	if res.Outcome != agentapi.OutcomeOK {
		return fmt.Errorf("%w: remember %s: %s", ErrRefused, res.Outcome, res.Reason)
	}
	return nil
}
