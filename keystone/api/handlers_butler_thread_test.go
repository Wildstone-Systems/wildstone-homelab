package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// answerTurns scripts a session that answers straight away, one answer
// per element — enough to drive several turns of one conversation.
func answerTurns(texts ...string) []*provider.ChatResponse {
	out := make([]*provider.ChatResponse, 0, len(texts))
	for _, t := range texts {
		out = append(out, &provider.ChatResponse{Text: t})
	}
	return out
}

func (h *harness) scripted(t *testing.T) *scriptedProvider {
	t.Helper()
	p, ok := h.s.ButlerHost.Provider.(*scriptedProvider)
	if !ok {
		t.Fatalf("butler host is not wired with a scripted provider")
	}
	return p
}

// Before ADR-W027 a session recorded the answer and not the question,
// so a transcript could not be read back as a conversation and an audit
// could not see what had been asked.
func TestButlerAskRecordsTheQuestionAsWellAsTheAnswer(t *testing.T) {
	h := newHarness(t)
	// A butler turn is had by a person, so the caller acts for one.
	asksAsRoleOperator := h.console(runtimedb.RoleOperator, "mika")
	h.wireButler(answerTurns("Two nodes: n1 and n2."), runtimedb.ButlerModeFull)

	var ask askResponse
	h.doToken(asksAsRoleOperator, "POST", "/api/v1/butler/ask",
		map[string]string{"message": "what nodes do I have?"}, &ask)

	var sess runtimedb.ButlerSession
	if resp := h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions/"+ask.SessionID, nil, &sess); resp.StatusCode != http.StatusOK {
		t.Fatalf("get session: %d", resp.StatusCode)
	}
	if len(sess.Turns) != 2 {
		t.Fatalf("turns = %d, want 2: %+v", len(sess.Turns), sess.Turns)
	}
	if sess.Turns[0].Role != runtimedb.ButlerRoleUser || sess.Turns[0].Text != "what nodes do I have?" {
		t.Fatalf("first turn = %+v, want the question", sess.Turns[0])
	}
	if sess.Turns[1].Role != runtimedb.ButlerRoleAssistant || sess.Turns[1].Text != "Two nodes: n1 and n2." {
		t.Fatalf("second turn = %+v, want the answer", sess.Turns[1])
	}
	if sess.Turns[0].Ordinal != 0 || sess.Turns[1].Ordinal != 1 {
		t.Fatalf("ordinals = %d,%d, want 0,1", sess.Turns[0].Ordinal, sess.Turns[1].Ordinal)
	}
}

// A proposal belongs to the turn that made it. One merged list per
// session cannot say which answer proposed what once a session has
// answered more than once.
func TestButlerProposalIsRecordedOnTheTurnThatMadeIt(t *testing.T) {
	h := newHarness(t)
	// A butler turn is had by a person, so the caller acts for one.
	asksAsRoleOperator := h.console(runtimedb.RoleOperator, "mika")
	h.wireButler(append(proposeGrafanaTurns(), answerTurns("Nothing else to add.")...), runtimedb.ButlerModeFull)

	var ask askResponse
	h.doToken(asksAsRoleOperator, "POST", "/api/v1/butler/ask", map[string]string{"message": "expose grafana"}, &ask)
	if len(ask.PlanIDs) != 1 {
		t.Fatalf("planIds = %v, want one", ask.PlanIDs)
	}
	h.doToken(asksAsRoleOperator, "POST", "/api/v1/butler/sessions/"+ask.SessionID+"/ask",
		map[string]string{"message": "anything else?"}, nil)

	var sess runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions/"+ask.SessionID, nil, &sess)
	if len(sess.Turns) != 4 {
		t.Fatalf("turns = %d, want 4", len(sess.Turns))
	}
	if len(sess.Turns[1].PlanIDs) != 1 || sess.Turns[1].PlanIDs[0] != ask.PlanIDs[0] {
		t.Fatalf("first answer's planIds = %v, want %v", sess.Turns[1].PlanIDs, ask.PlanIDs)
	}
	if len(sess.Turns[3].PlanIDs) != 0 {
		t.Fatalf("second answer proposed nothing but recorded %v", sess.Turns[3].PlanIDs)
	}
}

func TestButlerFollowUpContinuesTheSameSession(t *testing.T) {
	h := newHarness(t)
	// A butler turn is had by a person, so the caller acts for one.
	asksAsRoleOperator := h.console(runtimedb.RoleOperator, "mika")
	h.wireButler(answerTurns("Two nodes: n1 and n2.", "n2 has been offline since 14:04."), runtimedb.ButlerModeFull)

	var first askResponse
	h.doToken(asksAsRoleOperator, "POST", "/api/v1/butler/ask",
		map[string]string{"message": "what nodes do I have?"}, &first)

	var second askResponse
	resp := h.doToken(asksAsRoleOperator, "POST", "/api/v1/butler/sessions/"+first.SessionID+"/ask",
		map[string]string{"message": "is either of them down?"}, &second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("follow-up status = %d, want 200", resp.StatusCode)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("follow-up opened session %s, want %s", second.SessionID, first.SessionID)
	}

	// The model has to have been told what was said before, or a
	// follow-up is just a new question with a shared row id.
	convos := h.scripted(t).convos
	if len(convos) != 2 {
		t.Fatalf("provider saw %d conversations, want 2", len(convos))
	}
	got := convos[1]
	if len(got) != 3 {
		t.Fatalf("follow-up replayed %d messages, want 3: %+v", len(got), got)
	}
	want := []provider.Message{
		{Role: "user", Text: "what nodes do I have?"},
		{Role: "assistant", Text: "Two nodes: n1 and n2."},
		{Role: "user", Text: "is either of them down?"},
	}
	for i := range want {
		if got[i].Role != want[i].Role || got[i].Text != want[i].Text {
			t.Fatalf("message %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	var sess runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions/"+first.SessionID, nil, &sess)
	if len(sess.Turns) != 4 {
		t.Fatalf("turns = %d, want 4: %+v", len(sess.Turns), sess.Turns)
	}
	// The session's own finalText follows the newest answer, so every
	// reader that predates turns keeps seeing the latest one.
	if sess.FinalText != "n2 has been offline since 14:04." {
		t.Fatalf("finalText = %q, want the newest answer", sess.FinalText)
	}
}

// A tool result is a reading taken at a moment. Replaying it into a
// later turn hands the model a stale fact with the authority of a fresh
// one, so history carries the text and not the tool traffic.
func TestButlerFollowUpDoesNotReplayStaleToolResults(t *testing.T) {
	h := newHarness(t)
	// A butler turn is had by a person, so the caller acts for one.
	asksAsRoleOperator := h.console(runtimedb.RoleOperator, "mika")
	h.wireButler(append(proposeGrafanaTurns(), answerTurns("Nothing else.")...), runtimedb.ButlerModeFull)

	var first askResponse
	h.doToken(asksAsRoleOperator, "POST", "/api/v1/butler/ask", map[string]string{"message": "expose grafana"}, &first)
	h.doToken(asksAsRoleOperator, "POST", "/api/v1/butler/sessions/"+first.SessionID+"/ask",
		map[string]string{"message": "anything else?"}, nil)

	convos := h.scripted(t).convos
	follow := convos[len(convos)-1]
	for _, m := range follow {
		if m.Role == "tool" || len(m.ToolResults) > 0 || len(m.ToolCalls) > 0 {
			t.Fatalf("follow-up replayed tool traffic: %+v", m)
		}
	}
}

// Reading someone's conversation is an audit. Speaking into it is not.
func TestButlerFollowUpRefusedOnSomeoneElsesSession(t *testing.T) {
	h := newHarness(t)
	h.wireButler(answerTurns("Two nodes.", "should never run"), runtimedb.ButlerModeFull)

	var first askResponse
	h.do(runtimedb.RoleOperator, "POST", "/api/v1/butler/ask", map[string]string{"message": "what nodes?"}, &first)

	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/butler/sessions/"+first.SessionID+"/ask",
		map[string]string{"message": "and what else?"}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	// The admin can still read it — that is what the audit is for.
	if resp := h.do(runtimedb.RoleAdmin, "GET", "/api/v1/butler/sessions/"+first.SessionID, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin read = %d, want 200", resp.StatusCode)
	}
}

func TestButlerFollowUpUnknownSession404(t *testing.T) {
	h := newHarness(t)
	h.wireButler(answerTurns("should never run"), runtimedb.ButlerModeFull)
	resp := h.do(runtimedb.RoleOperator, "POST", "/api/v1/butler/sessions/bs-404/ask",
		map[string]string{"message": "hello?"}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// The surface comes from the STORED session, so continuing a household
// conversation cannot move it onto the operator's Butler (ADR-W026 A1).
func TestButlerFollowUpKeepsTheSessionsSurface(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(answerTurns("Mika had breakfast at 07:15.", "Nobody else has."), runtimedb.ButlerModeFull)

	var first askResponse
	if resp := h.do(runtimedb.RoleOperator, "POST", "/api/v1/household/butler/ask",
		map[string]string{"message": "did anyone have breakfast?"}, &first); resp.StatusCode != http.StatusOK {
		t.Fatalf("family ask: %d", resp.StatusCode)
	}

	if resp := h.do(runtimedb.RoleOperator, "POST", "/api/v1/butler/sessions/"+first.SessionID+"/ask",
		map[string]string{"message": "did anyone else?"}, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("family follow-up: %d", resp.StatusCode)
	}

	systems := h.scripted(t).systems
	if len(systems) != 2 {
		t.Fatalf("provider saw %d turns, want 2", len(systems))
	}
	for i, sys := range systems {
		if sys != systems[0] {
			t.Fatalf("turn %d ran on a different system prompt than turn 0", i)
		}
		// The household prompt is the one that must be in force; the
		// operator surface's Statefile framing must not appear.
		if strings.Contains(sys, "Statefile") {
			t.Fatalf("turn %d ran with the operator's prompt:\n%s", i, sys)
		}
	}

	var sess runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions/"+first.SessionID, nil, &sess)
	if sess.Surface != runtimedb.ButlerSurfaceFamily {
		t.Fatalf("surface = %q, want family", sess.Surface)
	}
	if len(sess.Turns) != 4 {
		t.Fatalf("turns = %d, want 4", len(sess.Turns))
	}
}
