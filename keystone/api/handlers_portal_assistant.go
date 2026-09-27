package api

// The Assistant surface (#198): a household member talks to a Butler
// from the dashboard, without the dashboard becoming an agent platform.
//
// THE WHOLE OF THIS FILE IS A CEILING. There is no model here, no
// provider, no tool, no memory and no authority evaluation — all of
// that already exists behind butlerlive, and a second implementation of
// any of it on the household's surface would be a second place for the
// answer to "may this person do this" to be wrong. What is here is the
// four things this DOOR decides:
//
//  1. WHO is asking. The member comes from the forwarded identity the
//     ingress proved (auth_portal.go), never from the request body.
//  2. WHICH Butlers exist for them. The roster is filtered in the
//     store, by the access list, so a person does not learn that the
//     Butlers they were not granted exist at all.
//  3. WHAT KIND of Butler may answer here. A household surface reaches
//     the family surface only, and never an operator-profile Butler —
//     rechecked on every message, so exposing one by configuration
//     mistake still does not let it answer a dashboard.
//  4. AS WHOM. Never as the owner. `asOwner` is the exemption that
//     gives the Console holder every Butler (ADR-W031 A10); a request
//     that arrived through a household dashboard is not that, whoever
//     is signed in.
//
// Deliberately NOT the session endpoints #198 sketches. A session id
// there is minted before the first message; here a conversation IS its
// recorded session, which already exists, is already audited, and is
// already what a follow-up is addressed to (ADR-W027). Minting a second
// kind of id in front of it would be a second lifetime to get wrong.

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/speech"
)

// maxPortalMessage bounds one message from the widget.
//
// A household member types a sentence. Anything much longer arriving
// from a text box is either a paste nobody meant to send or somebody
// testing what this endpoint forwards to a model, and both are better
// refused at the door than sent to a provider and billed for.
const maxPortalMessage = 4000

// assistantAgent is one Butler as the widget's selector shows it.
//
// leavesTheHouse rides along for the reason butlerView carries it: it
// is the most consequential fact about a Butler, a person deciding what
// to type is relying on it, and a client that has to fetch it
// separately is one that eventually forgets to.
type assistantAgent struct {
	Name           string `json:"name"`
	Directive      string `json:"directive,omitempty"`
	LeavesTheHouse bool   `json:"leavesTheHouse"`
	Reachable      bool   `json:"reachable"`
	CarriesActions bool   `json:"carriesActions"`
	// Dictation says the mic button works for this assistant: voice is
	// on, and an engine its privacy class allows exists (ADR-W042).
	Dictation bool `json:"dictation"`
}

// assistantAgents answers "which assistants may I talk to from here",
// filtered server-side.
//
// #198 lists what the filter must cover: the initiating person, their
// membership, the Butler's visibility and scope, the surface, and
// revocation. Every one of those is already the store's answer —
// ButlersFor is the access list, Live() is enabled-and-not-retired, and
// the surface test is below. The client may hide entries for UX; it is
// never what decides.
func (s *Server) assistantAgents(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant is configured on this deployment")
	}
	all, err := s.DB.ButlersFor(member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	agents := make([]assistantAgent, 0, len(all))
	for _, b := range all {
		if !portalMayReach(b) {
			continue
		}
		agents = append(agents, assistantAgent{
			Name:           b.Name,
			Directive:      b.Directive,
			LeavesTheHouse: b.ModelKind == runtimedb.ModelCloud,
			Reachable:      b.Reachable(),
			CarriesActions: b.CarriesActionAuthority(),
			Dictation:      s.canDictate(b.Name),
		})
	}
	out := map[string]any{"agents": agents, "member": member,
		// How long a conversation counts as recent: the pages remember
		// the one they were in for exactly this long, so a remembered
		// conversation never outlives the window the server continues
		// by (design §7).
		"sessionRecencySeconds": int(s.sessionRecencyWindow() / time.Second)}
	if s.Speech.Enabled() {
		// How long the mic records before stopping on its own: the
		// same limit the server enforces.
		max := s.Speech.MaxSeconds
		if max <= 0 {
			max = speech.DefaultMaxSeconds
		}
		out["dictationMaxSeconds"] = max
	}
	if len(agents) == 0 {
		out["setup"] = s.assistantSetup(r, member, desired)
	}
	return out, http.StatusOK, nil
}

// assistantSetupState is what an empty assistant card says, and to whom.
type assistantSetupState struct {
	// Needed is true when the household has no assistant at all, as
	// opposed to having one that has not been shared with this member.
	Needed bool `json:"needed"`
	// CanConfigure is whether this member can do something about it:
	// they are in the Console's group.
	CanConfigure bool `json:"canConfigure"`
	// Console is the Console's address, offered only to somebody who
	// can use it.
	Console string `json:"console,omitempty"`
	Message string `json:"message"`
}

// assistantSetup explains an empty assistant card.
//
// A fresh install has the assistant widget and no assistant: Butlers
// are created in the Console, each with its own model (ADR-W031), and
// nothing does that for the household. "No assistant has been shared
// with you yet" was true and useless — it named no next step and no
// place to take it. The card now says what is missing and, to somebody
// who can fix it, where.
func (s *Server) assistantSetup(r *http.Request, member string, desired []resource.Resource) assistantSetupState {
	exists := false
	if s.DB != nil {
		if all, err := s.DB.ListButlers(); err == nil {
			for _, b := range all {
				if b.Live() && portalMayReach(b) {
					exists = true
					break
				}
			}
		}
	}
	st := assistantSetupState{Needed: !exists, CanConfigure: slices.Contains(forwardedGroups(r), ConsoleGroup)}
	switch {
	case exists:
		st.Message = "No assistant has been shared with you yet — ask whoever looks after Wildstone to share one with you."
	case st.CanConfigure:
		st.Console = consoleAddress(desired)
		st.Message = "The assistant needs a model to think with. In the Console, open Butlers, add one for the " +
			"household and give it a model provider (your own Ollama server, for example). It answers here as soon as it has one."
	default:
		st.Message = "The assistant isn't set up yet. Whoever looks after Wildstone can give it a model in the Console."
	}
	return st
}

// consoleAddress is where the Console is served, or "" when it is not
// behind the proxy: the Console route's host as a browser reaches it,
// which under the tailnet posture is the node's name and a port.
func consoleAddress(desired []resource.Resource) string {
	posture := ingressorigin.Of(desired)
	for _, res := range desired {
		if res.Kind == resource.KindRoute && res.Route != nil && strings.HasPrefix(res.Route.Host, "console.") {
			return "https://" + posture.SiteAddress(res.Route.Host) + "/"
		}
	}
	return ""
}

// portalMayReach is the Homepage connection ceiling, as one function.
//
// Two conditions, and the second is not implied by the first even
// though CreateButler makes them agree today: the surface says where a
// Butler answers, the profile says what it may do, and a household
// dashboard is entitled to neither the operator's surface nor the
// operator's profile. Checking both means a row that somehow holds a
// mismatched pair is refused rather than admitted by whichever half was
// tested.
//
// It is called on the roster AND on every message, because a filter
// applied only when building a list is a filter a hand-written request
// skips — which is precisely the acceptance criterion #198 states.
func portalMayReach(b runtimedb.Butler) bool {
	// Both halves, still, even though 0058's paired CHECK now makes them
	// agree by construction. A defence that is correct only because of
	// an invariant elsewhere stops being a defence the day the invariant
	// is relaxed, and this one is checked on every message.
	return b.Surface == runtimedb.ButlerSurfaceFamily && b.AgentClass != runtimedb.AgentClassOwner
}

// assistantAsk runs one turn.
//
// A first message names an agent; a follow-up names the conversation,
// and the agent comes from the conversation's own stamp rather than
// from the request — the same rule continueButlerSession follows, and
// for the same reason: "I sent this to Sandy" and "this went to Alfred"
// must not both be true of one message.
func (s *Server) assistantAsk(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	var req struct {
		Agent   string `json:"agent"`
		Session string `json:"session"`
		Text    string `json:"text"`
		New     bool   `json:"new,omitempty"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	text := strings.TrimSpace(req.Text)
	switch {
	case text == "":
		return nil, http.StatusBadRequest, fmt.Errorf("a question needs a message")
	case len(text) > maxPortalMessage:
		return nil, http.StatusRequestEntityTooLarge,
			fmt.Errorf("that message is longer than this surface carries (%d characters)", maxPortalMessage)
	case s.DB == nil:
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant is configured on this deployment")
	}

	var prior *turnContext
	agent := strings.TrimSpace(req.Agent)
	if id := strings.TrimSpace(req.Session); id != "" {
		sess, err := s.DB.GetButlerSession(id)
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("that conversation is not here any more")
		}
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		// Somebody else's conversation is not found rather than
		// forbidden: a 403 on a guessed id confirms the id.
		if !strings.EqualFold(sess.ActingFor, member) || sess.Surface != runtimedb.ButlerSurfaceFamily {
			return nil, http.StatusNotFound, fmt.Errorf("that conversation is not here any more")
		}
		if sess.Butler != "" {
			if agent != "" && agent != sess.Butler {
				return nil, http.StatusConflict, fmt.Errorf(
					"this conversation is with %s — start a new one to talk to %s", sess.Butler, agent)
			}
			agent = sess.Butler
		}
		prior = &turnContext{SessionID: sess.ID, History: historyFrom(sess.Turns)}
	}
	if agent == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("name an assistant")
	}

	// The ceiling, rechecked here on every single message rather than
	// trusted from the roster this client was given.
	b, err := s.DB.ResolveButlerFor(agent, member)
	switch {
	case err == nil, errors.Is(err, runtimedb.ErrButlerUnreachable):
		// Unreachable is still a Butler this person may reach, and the
		// ceiling below applies to it; the ask path answers 503 with
		// which model is down rather than moving to another one.
	case errors.Is(err, runtimedb.ErrNoButlerAccess), errors.Is(err, runtimedb.ErrNotFound),
		errors.Is(err, runtimedb.ErrNotAdmitted):
		return nil, http.StatusNotFound, fmt.Errorf("no assistant answers there for you")
	default:
		return nil, http.StatusInternalServerError, err
	}
	if !portalMayReach(b) {
		// Same shape as "no such assistant": that an operator Butler
		// exists, and is called this, is not something a household
		// surface should be able to confirm.
		return nil, http.StatusNotFound, fmt.Errorf("no assistant answers there for you")
	}

	// Slash commands answer here, after the ceiling and before anything
	// else: deterministically, with no model, and whether or not this
	// deployment's Butlers are running (the deterministic backbone).
	if resp, handled := s.runWebChatCommand(b, member, portal.Connection, text); handled {
		return map[string]any{
			"session":         resp.SessionID,
			"agent":           agent,
			"reply":           resp.FinalText,
			"command":         resp.Command,
			"newConversation": resp.NewConversation,
		}, http.StatusOK, nil
	}

	// Whether this deployment has any Butler at all is asked AFTER the
	// ceiling, not before. Both answers are true facts about the
	// deployment, and the one a refused caller is entitled to is the
	// narrower: "no assistant answers there for you" rather than "there
	// is one, and this control plane is configured like so".
	if !s.butlersConfigured() {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant is configured on this deployment")
	}
	if prior == nil && !req.New {
		found, candidates, err := s.trustedContinuation(member, agent, portal.Connection)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		if len(candidates) > 1 {
			// Ask, don't guess (design §7): nothing is continued, and the
			// dashboard is handed the candidates to offer, so the Person
			// picks one and the same message is sent to it — rather than
			// being told to go and find it in their history.
			return map[string]any{
				"error":  continuationChoiceError(len(candidates)).Error(),
				"choose": continuationChoices(candidates),
			}, http.StatusConflict, nil
		}
		prior = found
	}

	// asOwner is false, always. Everything else — tools, memory scope,
	// authority, the model — comes from the Butler and the member, in
	// the one place that decides it for every surface.
	// THE CONNECTION CEILING, and this is the line that makes the
	// implementation package's acceptance criterion true: an operator
	// action is denied even with a hand-altered request, and even if an
	// owner-capable assistant is exposed here by mistake. It rides on
	// the turn rather than being checked here, so it reaches the tool
	// dispatch itself — one level below anything this handler can see.
	body, status, err := s.askNamedFamilyButler(r.Context(), text, agent, member, "", false, false, prior,
		butlerlive.ConnectionScope{Name: portal.Connection, Permits: portal.Permits})
	if err != nil {
		return nil, status, err
	}
	ans, ok := body.(askResponse)
	if !ok {
		return body, status, nil
	}
	out := map[string]any{
		"session":   ans.SessionID,
		"agent":     agent,
		"reply":     ans.FinalText,
		"capped":    ans.Capped,
		"proposals": len(ans.PlanIDs),
	}
	// The reply is a stand-in and the answer comes later as a new turn
	// (#389). seen is how many messages the conversation already shows,
	// so the page waits for the one after them, even if it lands before
	// the page first looks.
	if ans.Pending && ans.SessionID != "" {
		if sess, err := s.DB.GetButlerSession(ans.SessionID); err == nil {
			out["pending"] = true
			out["seen"] = len(assistantMessages(sess))
		}
	}
	return out, status, nil
}

// assistantConversation replays one conversation into a reloaded
// widget, so a household member who refreshes the dashboard does not
// lose what they were in the middle of.
func (s *Server) assistantConversation(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant is configured on this deployment")
	}
	sess, err := s.DB.GetButlerSession(r.PathValue("id"))
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("that conversation is not here any more")
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if !strings.EqualFold(sess.ActingFor, member) || sess.Surface != runtimedb.ButlerSurfaceFamily {
		return nil, http.StatusNotFound, fmt.Errorf("that conversation is not here any more")
	}
	return map[string]any{"session": sess.ID, "agent": sess.Butler, "messages": assistantMessages(sess)}, http.StatusOK, nil
}

// assistantMessages is a conversation's turns as the page renders them:
// every turn with text, in order.
func assistantMessages(sess runtimedb.ButlerSession) []assistantMessage {
	messages := make([]assistantMessage, 0, len(sess.Turns))
	for _, t := range sess.Turns {
		if t.Text == "" {
			continue
		}
		messages = append(messages, assistantMessage{
			Role:     t.Role,
			Text:     t.Text,
			At:       t.CreatedAt,
			Receipts: receiptsFrom(t.TranscriptJSON),
			// A turn that proposed something says so. The proposals
			// themselves live on the Console's Plans page, which is the
			// owner's; what the member is owed here is that it happened.
			Proposals: len(t.PlanIDs),
		})
	}
	return messages
}

// assistantMessage is one turn as the full page renders it.
type assistantMessage struct {
	Role string    `json:"role"`
	Text string    `json:"text"`
	At   time.Time `json:"at,omitzero"`
	// Receipts is what the assistant actually DID during this turn.
	Receipts  []receipt `json:"receipts,omitempty"`
	Proposals int       `json:"proposals,omitempty"`
}

// receipt is one action, as the design's "what was done in your name"
// panel shows it.
//
// The design asks for who it acted as, through which connection, the
// exact call, the result, the time and the duration. Five of those are
// here. Duration is not recorded anywhere — the transcript keeps what
// happened, not how long it took — so it is absent rather than
// estimated: a made-up number on a receipt is worse than a missing
// field on one.
//
// WHO and WHICH CONNECTION are not on the row because they are the same
// for every row on this surface, and repeating them per action is how a
// panel becomes unreadable. The page says them once, at the top: acting
// as this member, through household_portal.
type receipt struct {
	// Capability is the tool's name, which IS the capability — the same
	// string the connection ceiling matches against.
	Capability string `json:"capability"`
	// Refused marks an action the authority layer stopped BEFORE it
	// ran. This is the row the design says to open first, because it is
	// the ceiling made visible.
	Refused bool `json:"refused,omitempty"`
	// Why is the refusal's own sentence, which names the rule rather
	// than an error code.
	Why string `json:"why,omitempty"`
	// Result is what came back, for an action that ran. Bounded,
	// because a tool may return a page of text and a receipt is a line.
	Result string `json:"result,omitempty"`
	// Failed marks an action that ran and errored, which is neither a
	// refusal nor a success and must not be rendered as either.
	Failed bool `json:"failed,omitempty"`
}

// maxReceiptResult bounds one receipt's result line.
const maxReceiptResult = 160

// receiptsFrom turns a stored transcript into receipts.
//
// A transcript this cannot parse yields NO receipts rather than a
// partial list, and that is the important line in this function. The
// panel's whole promise is that a member can see what was done in their
// name; a list that quietly dropped the one action that did not parse
// would break that promise while looking complete. An empty panel is
// visibly empty.
func receiptsFrom(raw string) []receipt {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var stored storedTranscript
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return nil
	}
	out := make([]receipt, 0, len(stored.Calls))
	for _, c := range stored.Calls {
		r := receipt{Capability: c.Name}
		switch {
		case c.Rejected:
			// Refused before it ran, which is the design's own wording.
			r.Refused, r.Why = true, c.Error
			if r.Why == "" {
				r.Why = "Refused before it ran."
			}
		case c.Error != "":
			r.Failed, r.Why = true, c.Error
		default:
			r.Result = c.Result
			if len([]rune(r.Result)) > maxReceiptResult {
				r.Result = string([]rune(r.Result)[:maxReceiptResult]) + "\u2026"
			}
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// assistantScope is what this surface may and may not do, for the panel
// that tells a member so before they discover it by being refused.
//
// Served from the connection profile itself rather than restated here,
// so the page and the ceiling cannot drift: the same portal.Permits
// that refuses a tool at dispatch is what these two lists come from.
//
// Headed "as <member>", because authority is per person and not per
// assistant. A list titled with the agent's name would read as a
// description of the agent, and the member's own permissions are half
// of the intersection.
func (s *Server) assistantScope(_ http.ResponseWriter, _ *http.Request, member string, _ []resource.Resource) (any, int, error) {
	return map[string]any{
		"as":         member,
		"connection": portal.Connection,
		"allowed":    portal.Allows(),
		"denied":     portal.Denies(),
		// The sentence the design asks for beneath the list. It is the
		// whole intersection model in one line, and it is here rather
		// than in the page so every surface that shows this says it.
		"note": "The server decides this list, and the denials hold even if a request is altered by hand. " +
			"What you can actually do is the narrowest of: your own permissions, what this assistant was " +
			"given, and what this dashboard allows.",
	}, http.StatusOK, nil
}

// maxConversationsShown bounds the left-hand column. A household's
// history grows without limit and a sidebar does not.
const maxConversationsShown = 24

// assistantConversations lists this member's own conversations, newest
// first, for the full page's left-hand column.
//
// SCOPED IN THE QUERY, and it is worth being plain about why it moved
// there. This used to read the newest 200 family sessions and test each
// one against the member afterwards, over-reading on the theory that a
// household where one person talks a lot would otherwise push everybody
// else off their own list. It does not help: if a partner owns the
// newest 200, all 200 are discarded and the member is shown an empty
// list under a heading that says it is their history. A fixed over-read
// only chooses the number of conversations at which somebody disappears.
//
// The member comes from the forwarded identity the ingress proved,
// never from the request, so the filter cannot be aimed and a household
// assistant never exposes one member's conversations to another.
//
// The surface filter is not redundant with it. A member may also have
// talked to an operator-surface Butler if they hold the Console, and
// those conversations are not this page's to show.
func (s *Server) assistantConversations(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant is configured on this deployment")
	}
	// Exactly what is shown, because the database is now doing the
	// narrowing: every row it returns is one of this member's.
	sessions, err := s.DB.ListButlerSessions(maxConversationsShown, runtimedb.ButlerSessionFilter{
		Surface: runtimedb.ButlerSurfaceFamily, ActingFor: member,
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	type conversation struct {
		ID    string    `json:"id"`
		Agent string    `json:"agent,omitempty"`
		At    time.Time `json:"at"`
		// Title is generated from the conversation rather than typed:
		// the first thing the member said, trimmed. A household member
		// does not name a chat before having it.
		Title string `json:"title"`
	}
	// AND THE FIRST THING THEY SAID IS NOT final_text. That column is
	// the ASSISTANT's latest reply — AppendButlerTurns overwrites it on
	// every assistant turn — so titling from it named each conversation
	// after the answer instead of the question, and renamed it again
	// every time the member followed up. A list whose rows change their
	// names is a list nobody can find anything in.
	seqs := make([]int64, 0, len(sessions))
	for _, sess := range sessions {
		seqs = append(seqs, sess.Seq)
	}
	opening, err := s.DB.FirstUserTurns(seqs)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := make([]conversation, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, conversation{
			ID:    sess.ID,
			Agent: sess.Butler,
			At:    sess.CreatedAt,
			Title: conversationTitle(opening[sess.Seq]),
		})
	}
	return map[string]any{"conversations": out, "member": member}, http.StatusOK, nil
}

// maxConversationTitle is how much of a conversation becomes its name in
// the list.
const maxConversationTitle = 60

// conversationTitle turns a conversation into a line for the list.
//
// From the session's own recorded text, and it is deliberately not
// empty-tolerant in a silent way: a session with nothing in it is
// called "Nothing said yet" rather than rendering as a blank row a
// member cannot tell apart from a broken one.
func conversationTitle(text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return "Nothing said yet"
	}
	// One line: a model's answer may carry newlines and a list row may
	// not grow to fit them.
	if i := strings.IndexAny(t, "\r\n"); i > 0 {
		t = strings.TrimSpace(t[:i])
	}
	runes := []rune(t)
	if len(runes) > maxConversationTitle {
		return strings.TrimSpace(string(runes[:maxConversationTitle])) + "\u2026"
	}
	return t
}
