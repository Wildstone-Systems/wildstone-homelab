package api

// The family surface's endpoint (ADR-W026 A1/A2, work package G1).
//
// It is a sibling of /api/v1/butler/ask, not a mode of it. The two
// endpoints build different gateways, run different system prompts,
// record different surfaces, and have different ceilings — and the
// separation is here, at the route, because that is the one place a
// request cannot argue with: the surface is a property of the door you
// came in through, never of the body you sent.
//
// What a family session CANNOT do is worth stating plainly, because
// none of it is a check in this file: it cannot read the Statefile, list
// plans, propose a change, or reach a node, because the gateway it is
// handed holds no capability that does any of those things. There is no
// filter to get wrong.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"

	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
)

// householdConfigured reports whether a family surface is wired at all:
// a provider to talk to, and at least one household seam to talk about.
// A deployment with a provider but no household seams has an operator
// butler and no family one, which is a normal state (nothing household
// is installed yet), not an error.
func (h *ButlerHost) householdConfigured() bool {
	prov, _, _, _, _ := h.Current()
	if prov == nil {
		return false
	}
	return h.householdWired()
}

// householdWired reports whether any household seam exists. The seams
// are set once when cmd/keystone builds the host and are never touched
// by a runtime provider change (ADR-W017 A9's Swap replaces the backend,
// not the household wiring), so unlike the backend they need no
// snapshot — and a model change never costs the family surface its
// tools.
func (h *ButlerHost) householdWired() bool {
	if h == nil {
		return false
	}
	return h.Household.Events != nil || h.Household.Actions != nil
}

// householdSeams is the deployment's family wiring, for a caller that
// wants to REPORT the surface rather than run on it.
//
// It reads the same field householdGateway passes, so a capability
// listing and a live session cannot disagree about which tools this
// house has a source behind. A nil host answers with the zero value,
// which is the honest one: nothing is wired.
func (h *ButlerHost) householdSeams() butler.HouseholdSeams {
	if h == nil {
		return butler.HouseholdSeams{}
	}
	return h.Household
}

// inventorySeams is householdSeams for a view that describes or assigns
// tools rather than serving a session: memory, which only a session
// binds, is present (butler.InventoryMemory).
func (h *ButlerHost) inventorySeams() butler.HouseholdSeams {
	seams := h.householdSeams()
	seams.Memory = butler.InventoryMemory()
	return seams
}

// householdGateway builds the per-session family gateway for member.
// Per-session rather than shared because the gateway is BOUND to the
// member (ADR-W026 A1/A4): the member it acts for is a construction
// argument, so no tool input, prompt, or injected event can name a
// different one.
func (h *ButlerHost) householdGateway(member string) *butler.HouseholdGateway {
	return butler.NewHouseholdGateway(member, h.Household)
}

// householdGroupGateway builds the gateway for a session whose audience
// is a room (ADR-W026 A7): the same seams, narrowed so another member's
// events are never fetched.
func (h *ButlerHost) householdGroupGateway(member string) *butler.HouseholdGateway {
	return butler.NewHouseholdGroupGateway(member, h.Household)
}

// bindPersonAuthority makes the pair of per-session bindings every path that
// serves a PERSON must make: where authority is resolved, and who is
// asking over which Connection (design §10.1).
//
// It is one helper rather than three lines repeated at each session
// opener because the three lines are a unit — an authorizer with no
// person is a resolver that cannot answer layer 1, and a person with no
// authorizer is an identity nothing checks. The named-Butler path makes
// the same binding in butlerlive.Serve; this is the legacy
// single-provider path's copy of it, and the two are the only places a
// session is opened.
//
// A deployment with no runtime DB gets no authorizer, and the seam then
// refuses every person-facing tool. That is the intended direction: the
// wiring is the act (§10, and Ledger.AdmissionCheck's own note).
type personFacingGateway interface {
	SetAuthorizer(a butler.ToolAuthorizer)
	SetCaller(kind butler.CallerKind, person, connection string)
}

func (s *Server) bindPersonAuthority(gw personFacingGateway, member, connection string) {
	if s.DB != nil {
		authority := &butlerstore.Authority{DB: s.DB}
		authority.PersonAuthority = authority.PersonIsAdmitted()
		gw.SetAuthorizer(authority)
	}
	gw.SetCaller(butler.CallerPersonFacing, member, connection)
}

// getHouseholdButlerStatus is the family surface's half of the status
// endpoint: whether it is wired, and what it can actually do here. The
// Console renders it so a household member is never shown a chat box
// that cannot answer.
func (s *Server) getHouseholdButlerStatus(_ *http.Request, id principal.Identity) (any, int, error) {
	host := s.ButlerHost
	prov, _, mode, model, endpoint := host.Current()
	if prov == nil || !host.householdWired() {
		reason := "no Butler provider configured; start keystone with -butler-provider (off|anthropic|openai|ollama)"
		if prov != nil {
			reason = "no household surface wired: this deployment has no household timeline or actions configured"
		}
		return butlerStatus{Configured: false, Reason: reason}, http.StatusOK, nil
	}
	// Bound before it is asked, like any other session (design §10.1).
	// The endpoint's whole job is telling THIS person what they can do
	// here, so it is person-facing and reports their reach rather than
	// the registry's — a member restricted from a capability should not
	// be shown a chat box offering it.
	//
	// actingMember rather than id.Name: the person the caller acts for,
	// not the credential's own name. The ask path below has said so
	// since #240, where passing the principal's name is what filed
	// household memory under `root-admin`; this read had quietly kept
	// the old spelling, which mattered the moment the answer became
	// per-person.
	gw := host.householdGateway(actingMember(id))
	s.bindPersonAuthority(gw, actingMember(id), butlerlive.ConsoleConnection)
	specs := gw.OfferedSpecs(mode == runtimedb.ButlerModeQAOnly)
	tools := make([]string, 0, len(specs))
	for _, t := range specs {
		tools = append(tools, t.Name)
	}
	return butlerStatus{
		Configured: true,
		Provider:   prov.Name(),
		Model:      model,
		Endpoint:   redact.String(endpoint),
		Mode:       string(mode),
		Tools:      tools,
	}, http.StatusOK, nil
}

// askHouseholdButler runs one family-surface session for the
// authenticated caller (ADR-W026 A1).
//
// The member the session acts for is the CALLER — the identity Keystone
// already authenticated — and there is no field in the request through
// which a caller could name somebody else. That is deliberate for G1:
// the only callers today are the Console and `wild`, where the caller is
// a human with a Wildstone credential. When a chat adapter becomes a
// caller (work package G2) the member comes from a binding row the
// adapter cannot forge, and it arrives here through the same field, so
// this handler does not change shape.
//
// The route carries the read verb, as the operator's ask does: talking
// is not a write. The one thing a family session can cause — a
// domestic-tier action — is bounded by the domestic gateway's own
// checks, which are the same ones an approved household rule passes
// through (ADR-W024 A5).
func (s *Server) askHouseholdButler(r *http.Request, id principal.Identity) (any, int, error) {
	r, message, named, st, err := readAskMessage(r)
	if err != nil {
		return nil, st, err
	}
	// A deployment with Butlers answers as one, and the household
	// provider check below does not apply to it: a family Butler runs on
	// the model it was ASSIGNED, not on whatever -butler-provider names
	// (ADR-W031 A2). Checking the legacy host first would refuse a
	// perfectly reachable Butler because a flag the deployment no longer
	// uses was left at "off".
	if s.butlersConfigured() {
		// actingMember, not id.Name: a turn is had by a PERSON (#240),
		// and a Console caller authenticates as a principal that ACTS
		// FOR one. Passing the principal here named the credential as
		// the member, which turnHasAPerson now refuses outright — and
		// which, before it did, filed the household's memory and work
		// under the credential.
		return s.askNamedFamilyButler(r.Context(), message, named, actingMember(id), "", false, actingAsOwner(id), nil,
			butlerlive.ConnectionScope{Name: butlerlive.ConsoleConnection})
	}
	host := s.ButlerHost
	if !host.householdConfigured() {
		return nil, http.StatusServiceUnavailable,
			fmt.Errorf("household butler not configured (needs -butler-provider and a household surface)")
	}
	return s.runFamilyButlerSession(r.Context(), message, actingMember(id), "", butlerlive.ConsoleConnection)
}

// askNamedFamilyButler answers on the family surface as a named Butler.
// It is the operator path's sibling and differs only in the ceiling it
// requires, which is the difference ADR-W026 A1 draws between the two
// surfaces and the one thing that must not be shared between them.
// conn is the Connection the request arrived through. Its NAME is now
// required — §10.1 fails closed without one, and a Console turn that
// left it empty would be indistinguishable from a caller that forgot.
//
// Its PERMITS are the ceiling, and they stay an explicit parameter
// rather than a default because the three callers genuinely differ: the
// chat adapters and the Console ask endpoint each already have their
// own ceiling, and only the dashboard has this one. A nil Permits is no
// ceiling — a ceiling none of them chose would be one nobody chose.
func (s *Server) askNamedFamilyButler(ctx context.Context, message, named, member, systemExtra string, group, asOwner bool, prior *turnContext, conn butlerlive.ConnectionScope) (any, int, error) {
	if named == "" {
		b, err := s.Butlers.Sole(runtimedb.ButlerSurfaceFamily, member, asOwner)
		if err != nil {
			if errors.Is(err, butlerlive.ErrAmbiguousButler) {
				return nil, http.StatusBadRequest, err
			}
			return nil, http.StatusNotFound, fmt.Errorf("no butler answers to you here")
		}
		named = b.Name
	}
	turn := butlerlive.Turn{
		Butler:         named,
		Member:         member,
		Text:           message,
		Group:          group,
		SystemExtra:    systemExtra,
		AsOwner:        asOwner,
		RequireSurface: runtimedb.ButlerSurfaceFamily,
		Connection:     conn,
	}
	if prior != nil {
		turn.SessionID, turn.History = prior.SessionID, prior.History
	}
	return s.runNamedButlerSession(ctx, turn, prior)
}

// runFamilyButlerSession runs one family-surface session for member and
// returns the ask response. Shared by the HTTP ask endpoint and the chat
// inbound endpoint (ADR-W026 A8).
//
// systemExtra is appended to the family system prompt for the audience
// the session is speaking to — the group-chat note (A7) is the only user
// of it today. It can add a constraint to what the Butler SAYS; it
// cannot add or remove a tool, because the tools come from the gateway
// and the gateway is built from the member.
func (s *Server) runFamilyButlerSession(ctx context.Context, message, member, systemExtra, connection string) (any, int, error) {
	return s.runFamilySession(ctx, message, member, systemExtra, connection, false, nil)
}

// runFamilySession is the shared body. group selects the narrowed
// gateway (ADR-W026 A7) — the audience decides which CAPABILITIES the
// session has, not merely what its prompt asks it to do.
func (s *Server) runFamilySession(ctx context.Context, message, member, systemExtra, connection string, group bool, prior *turnContext) (any, int, error) {
	sessionIDForPrivacy := ""
	if prior != nil {
		sessionIDForPrivacy = prior.SessionID
	}
	// Unknown legacy processing routes cannot receive local-only content.
	ctx, privacy, privacyErr := s.DB.BeginPrivacyTurn(ctx, "", runtimedb.MemoryDestination{BuiltIn: connection == "console" || connection == portal.Connection, Group: group}, sessionIDForPrivacy)
	if privacyErr != nil {
		return nil, http.StatusForbidden, privacyErr
	}
	if privacyErr = privacy.Check(); privacyErr != nil {
		return nil, http.StatusForbidden, privacyErr
	}
	if note, handled, e := s.prepareSecretInput(member, message, 0); handled {
		if e != nil {
			return nil, http.StatusServiceUnavailable, e
		}
		return askResponse{FinalText: note}, http.StatusOK, nil
	}
	host := s.ButlerHost
	// One snapshot for the whole session, as the operator surface takes
	// (ADR-W017 A9): a model swapped in mid-session would produce a
	// transcript whose tool results and final answer came from different
	// backends.
	prov, _, mode, _, _ := host.Current()
	if prov == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no Butler provider configured")
	}
	// Before a single word reaches the provider. The named path has
	// checked this since it existed; the legacy single-provider path
	// never did, and binding the gateway's authority below does not
	// cover it — that makes the TOOLS refuse, while a model answer
	// carrying no tool call completes normally and the message has
	// already left for the provider (see turnHasAPerson).
	//
	// After the configuration check, deliberately, for the reason the
	// operator path gives: a deployment with no Butler at all has
	// nothing to say about who is asking.
	if code, err := s.turnHasAPerson(member); err != nil {
		return nil, code, err
	}
	gateway := host.householdGateway(member)
	if group {
		gateway = host.householdGroupGateway(member)
	}
	// The legacy single-provider path opens a session the same way the
	// named path does, so it is told the same things: who is asking,
	// which Connection they arrived on, and where authority is resolved
	// (design §10.1). Before this it asked the seam nothing at all and
	// ran on structural presence alone, which is precisely the
	// permissive default this slice removes from person-facing work.
	s.bindPersonAuthority(gateway, member, connection)
	session := &butler.Session{
		Gateway:  gateway,
		ReadOnly: mode == runtimedb.ButlerModeQAOnly,
	}
	if systemExtra != "" {
		session.System = butler.HouseholdSystemPrompt + "\n\n" + systemExtra
	}
	if prior != nil {
		session.History = prior.History
	}
	tr, err := session.Run(ctx, butlerlive.GuardPrivacyProvider(prov, privacy), message)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("household butler session: %w", err)
	}

	// Persist the session (ADR-W017 A1, unchanged for this surface): the
	// record is principal-bound and carries the surface, so an audit can
	// separate what the household asked for from what the operator did.
	// Nothing here can carry a plan id — the family surface has no
	// propose path to produce one.
	stored := storedTranscript{Provider: tr.Audit.Provider}
	for _, call := range tr.Audit.Calls {
		stored.Calls = append(stored.Calls, transcriptRecord{
			Name:     call.Name,
			Input:    call.Input,
			Result:   call.Result,
			Error:    call.Error,
			Rejected: call.Rejected,
		})
	}
	transcriptJSON, err := json.Marshal(stored)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("encoding transcript: %w", err)
	}
	finalText := redact.String(tr.FinalText)
	sessionID, err := s.recordTurn(prior, runtimedb.ButlerSession{
		Principal:         butlerPrincipal,
		ActingFor:         member,
		Provider:          tr.Audit.Provider,
		Mode:              mode,
		Surface:           runtimedb.ButlerSurfaceFamily,
		InboundConnection: connection,
		FinalText:         finalText,
		Capped:            tr.CappedIterations,
		TranscriptJSON:    redact.String(string(transcriptJSON)),
		Turns: []runtimedb.ButlerTurn{
			{Role: runtimedb.ButlerRoleUser, Text: redact.String(message), Source: turnSource(ctx)},
			{
				Role: runtimedb.ButlerRoleAssistant, Text: finalText, Capped: tr.CappedIterations,
				TranscriptJSON: redact.String(string(transcriptJSON)),
			},
		},
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err = privacy.SetSession(sessionID); err != nil {
		return askResponse{SessionID: sessionID}, http.StatusForbidden, err
	}
	s.noteBuiltInSessionReply(sessionID, connection)
	s.event("butler.session", "", principal.Identity{Name: butlerPrincipal, ActingFor: member},
		fmt.Sprintf("household session %s: %d tool calls", sessionID, len(tr.Audit.Calls)))

	return askResponse{
		SessionID: sessionID,
		Mode:      string(mode),
		FinalText: finalText,
		Capped:    tr.CappedIterations,
	}, http.StatusOK, nil
}

// householdFactsFrom flattens runtime-DB household event rows into the
// small shape the family gateway shows a model, newest first. It lives
// in the API package rather than in keystone/butler because that package
// must not learn the runtime DB's row type — the seam exists to keep it
// from doing so.
func householdFactsFrom(rows []runtimedb.HouseholdEvent) []butler.HouseholdFact {
	out := make([]butler.HouseholdFact, 0, len(rows))
	for _, e := range rows {
		out = append(out, butler.HouseholdFact{
			OccurredAt: e.OccurredAt,
			Type:       e.Type,
			Subject:    e.Subject,
			Entity:     e.Entity,
			Detail:     householdDetail(e.Payload),
		})
	}
	return out
}

// householdDetail renders an event payload as one short line. A payload
// is publisher-written text; it is bounded here and enveloped downstream
// like every other untrusted fact.
func householdDetail(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	const maxDetail = 300
	s := string(payload)
	if len(s) <= maxDetail {
		return s
	}
	// Cut on a rune boundary: a payload is publisher-written text and
	// may be any language, and half a rune is invalid UTF-8 rather than
	// a shortened word.
	cut := maxDetail
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// HouseholdEventSeam builds the family surface's timeline reader over
// the runtime DB (ADR-W026 A1). cmd/keystone wires it; it lives here so
// the only query shape a family session can make is written next to the
// endpoint that serves it.
//
// Every lookup is narrowed and bounded before it reaches SQL: a family
// session cannot ask for the whole timeline, because there is no value
// of HouseholdLookup that means "everything" — an absent limit becomes
// the default, and an oversized one is clamped.
//
// memberScoped reports whether an event type is ABOUT A PERSON — the
// `subject: member` a package declares for each type it publishes
// (ADR-W024 A10). It is the whole basis of the group rule below, and it
// comes from the catalog rather than from anything a message can say.
// A nil function means nothing is member-scoped, which is the right
// reading for a deployment with no catalog: no declarations, no claims
// about whose facts these are.
func HouseholdEventSeam(db *runtimedb.DB, memberScoped func(eventType string) bool) func(context.Context, butler.HouseholdLookup) ([]butler.HouseholdFact, error) {
	return func(_ context.Context, q butler.HouseholdLookup) ([]butler.HouseholdFact, error) {
		limit := q.Limit
		if limit <= 0 || limit > butler.MaxHouseholdFacts {
			limit = butler.MaxHouseholdFacts
		}
		// A group read fetches wider, because the rule below removes rows
		// after the query: asking for five and dropping three would
		// answer with two when five were available.
		fetch := limit
		if q.GroupSafe {
			fetch = limit * groupOverfetch
		}
		rows, err := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{
			Type:       q.EventType,
			Subject:    q.Subject,
			SubjectSet: q.Subject != "",
			Limit:      fetch,
			Newest:     true,
		})
		if err != nil {
			return nil, err
		}
		if q.GroupSafe {
			rows = groupSafeRows(rows, q.Member, memberScoped)
		}
		if len(rows) > limit {
			rows = rows[:limit]
		}
		return householdFactsFrom(rows), nil
	}
}

// groupOverfetch is how much wider a group read looks, to leave room for
// the rows the rule removes.
const groupOverfetch = 4

// groupSafeRows drops facts about a person who is not the asker
// (ADR-W026 A7).
//
// The rule is deliberately about WHOSE fact it is rather than what the
// fact says. A subject filter can tell that `person.left_home` belongs
// to Sam; nothing here can read the note inside a shared calendar entry
// that happens to mention her. That limit is real and is recorded in the
// ADR — the prompt asks for discretion about content, and this is the
// part that does not depend on the model agreeing.
func groupSafeRows(rows []runtimedb.HouseholdEvent, member string, memberScoped func(string) bool) []runtimedb.HouseholdEvent {
	if memberScoped == nil {
		// No declarations to reason from. Withhold every attributed fact
		// except the asker's rather than assume the unknown ones are
		// harmless: a deployment with no catalog is not a deployment
		// where everyone's movements are public.
		memberScoped = func(string) bool { return true }
	}
	out := rows[:0]
	for _, r := range rows {
		// Nobody's fact — the house itself, or a publisher that declared
		// no subject. A room may hear it.
		if r.Subject == "" {
			out = append(out, r)
			continue
		}
		// A device, a room, a vehicle: not a person, so not private to
		// one. This is the case the first attempt got wrong.
		if !memberScoped(r.Type) {
			out = append(out, r)
			continue
		}
		// A person's fact: only if it is the asker's own.
		if r.Subject == member {
			out = append(out, r)
		}
	}
	return out
}

// MemberScopedTypes builds the memberScoped function from a catalog's
// `publishes` declarations: the event types whose subject is a member.
//
// It is a function rather than a snapshot map so that enabling a package
// mid-run changes the answer, the same way the chat action gate reads
// the Statefile per call rather than at startup.
func MemberScopedTypes(types func() map[string]bool) func(string) bool {
	return func(eventType string) bool {
		if types == nil {
			return true
		}
		return types()[eventType]
	}
}
