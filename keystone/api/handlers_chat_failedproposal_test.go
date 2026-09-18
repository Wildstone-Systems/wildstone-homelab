package api

// A chat turn that failed after proposing something still tells the
// person what is waiting for them.
//
// The Console shows a pending plan in its own list, so a failed turn
// there is only a missing answer. In chat the reply IS the surface:
// without a notice, a proposal the Butler made sits in the approval
// queue with nobody told it exists.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// failingButlers answers every turn with work already done and then an
// error — the shape a turn takes when a tool ran and the provider died
// before it could say so.
type failingButlers struct {
	answer butlerlive.Answer
	served int
}

func (f *failingButlers) Any() (bool, error) { return true, nil }

func (f *failingButlers) Serve(_ context.Context, _ butlerlive.Turn) (butlerlive.Answer, error) {
	f.served++
	return f.answer, errors.New("the provider gave up")
}

func (f *failingButlers) Sole(runtimedb.ButlerSurface, string, bool) (runtimedb.Butler, error) {
	return runtimedb.Butler{}, errors.New("no butler was named")
}

// wireFailingButler makes every chat turn fail after proposing one
// change, and binds sam's telegram account to the family surface.
func wireFailingButler(t *testing.T, h *harness) *failingButlers {
	t.Helper()
	f := &failingButlers{answer: butlerlive.Answer{
		TaskID:   failedTurnTask(t, h),
		Butler:   "alfred",
		Surface:  runtimedb.ButlerSurfaceFamily,
		Provider: "scripted",
		Calls: []butler.ToolCallRecord{{
			Name:         "propose_change",
			Result:       "accepted",
			ResourceYAML: containerYAML(t, "grafana", "ghcr.io/example/grafana@"+testDigest, nil),
		}},
	}}
	h.s.Butlers = f
	// The bot a message arrives at IS the address (ADR-W031 A7), so the
	// adapter has to reach this path on an adapter's own credential
	// rather than the harness's generic concierge.
	name := chatAdapterPrincipalPrefix + "chat-telegram-family"
	if err := h.s.Principals.Create(name, runtimedb.RoleConcierge, ""); err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	tok, err := h.s.Principals.Mint(name, "", 0)
	if err != nil {
		t.Fatalf("Mint(%s): %v", name, err)
	}
	h.tokens[runtimedb.RoleConcierge] = tok
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	return f
}

func TestAFailedChatTurnAnnouncesWhatItProposed(t *testing.T) {
	h := newHarness(t)
	wireFailingButler(t, h)

	resp, _ := h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345",
		"text": "expose grafana", "messageId": "m-1",
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 from the failed turn", resp.StatusCode)
	}

	queued := h.poll(t, runtimedb.RoleConcierge, "")
	if len(queued) != 1 {
		t.Fatalf("queued = %+v, want one notice about the proposal", queued)
	}
	// The plan really is waiting, and the notice names THAT plan rather
	// than describing one in the abstract.
	var plans []runtimedb.PlanRecord
	h.do(runtimedb.RoleAdmin, "GET", "/api/v1/plans", nil, &plans)
	if len(plans) != 1 {
		t.Fatalf("plans = %+v, want the one the failed turn proposed", plans)
	}
	if !strings.Contains(queued[0].Body, plans[0].ID) {
		t.Fatalf("the notice does not name the plan %s:\n%s", plans[0].ID, queued[0].Body)
	}
	if !strings.Contains(queued[0].Body, "Nothing has changed yet") {
		t.Fatalf("the notice does not say the change has not happened:\n%s", queued[0].Body)
	}
}

// The notice is news, not an answer: the claim is not settled, so the
// adapter's redelivery still gets to retry the turn.
func TestAnnouncingAProposalDoesNotConsumeTheRetry(t *testing.T) {
	h := newHarness(t)
	f := wireFailingButler(t, h)

	body := map[string]any{
		"transport": "telegram", "account": "12345",
		"text": "expose grafana", "messageId": "m-1",
	}
	h.send(runtimedb.RoleConcierge, body)
	h.send(runtimedb.RoleConcierge, body)

	if f.served != 2 {
		t.Fatalf("the butler served %d turns; the redelivery after a failure must still be tried", f.served)
	}
}

// Each retry that proposes something NEW is announced, so nothing
// reaches the approval queue unmentioned.
func TestEveryNewProposalIsAnnounced(t *testing.T) {
	h := newHarness(t)
	wireFailingButler(t, h)

	for _, id := range []string{"m-1", "m-2", "m-3"} {
		h.send(runtimedb.RoleConcierge, map[string]any{
			"transport": "telegram", "account": "12345",
			"text": "expose grafana", "messageId": id,
		})
	}

	var plans []runtimedb.PlanRecord
	h.do(runtimedb.RoleAdmin, "GET", "/api/v1/plans", nil, &plans)
	queued := h.poll(t, runtimedb.RoleConcierge, "")
	if len(queued) != len(plans) {
		t.Fatalf("%d plans are waiting but %d notices were sent", len(plans), len(queued))
	}
	for _, p := range plans {
		var named bool
		for _, m := range queued {
			named = named || strings.Contains(m.Body, p.ID)
		}
		if !named {
			t.Errorf("plan %s is waiting for approval and nobody was told", p.ID)
		}
	}
}

// The notice's dedupe key is the message AND the outcome. Each half
// covers a case the other gets wrong, so both are asserted.
func TestTheNoticeIsKeyedOnTheMessageAndTheOutcome(t *testing.T) {
	const (
		surface = runtimedb.ButlerSurfaceFamily
		tp      = "telegram"
		convo   = "12345"
	)
	// The message identity, as the handler builds it.
	msg := func(id, text string) string {
		return chatDedupe(surface, tp, convo, id, "proposal", text)
	}
	plans := []string{"plan-1", "plan-2"}
	same := chatProposalDedupe(msg("m-1", "expose grafana"), plans, nil)

	// A redelivery of ONE message with the same outcome is one piece of
	// news, however the ids are ordered.
	if got := chatProposalDedupe(msg("m-1", "expose grafana"), []string{"plan-2", "plan-1"}, nil); got != same {
		t.Errorf("a redelivery of the same message keys differently:\n%s\n%s", same, got)
	}
	// A LATER message is a later question, even when it ends exactly the
	// same way. This is the case the notice itself invites: it tells the
	// person to ask again, and asking again must not be silent.
	if got := chatProposalDedupe(msg("m-2", "expose grafana again"), plans, nil); got == same {
		t.Errorf("a second question reuses the key %s, so the answer to it would be dropped", got)
	}
	// And the same message that ends DIFFERENTLY is different news — the
	// first attempt refused, the resumed one proposing something to
	// approve.
	if got := chatProposalDedupe(msg("m-1", "expose grafana"), []string{"plan-3"}, nil); got == same {
		t.Errorf("a different outcome for the same message reuses the key %s", got)
	}
	if got := chatProposalDedupe(msg("m-1", "expose grafana"), plans, []string{"400: not a route"}); got == same {
		t.Errorf("a refusal alongside the same plans reuses the key %s", got)
	}
	// Two different refusals are two different things to be told, and
	// the same refusal twice is one.
	refused := chatProposalDedupe(msg("m-1", "x"), nil, []string{"400: not a route"})
	if got := chatProposalDedupe(msg("m-1", "x"), nil, []string{"400: not a route"}); got != refused {
		t.Errorf("the same refusal keys differently on a second delivery:\n%s\n%s", refused, got)
	}
	if got := chatProposalDedupe(msg("m-1", "x"), nil, []string{"403: not yours"}); got == refused {
		t.Errorf("two unrelated refusals share the key %s", got)
	}
	// The key never carries the refusal text itself, which is provider
	// output and can be anything at all.
	if strings.Contains(refused, "not a route") {
		t.Error("the dedupe key spells out the refusal text")
	}
	// Scoped like every other reply, which it inherits from chatDedupe:
	// one conversation's notice must not deduplicate another's, and
	// neither must one surface's (ADR-W026 A3 — a private chat id is
	// the same value for every bot the person talks to).
	if got := chatProposalDedupe(chatDedupe(surface, tp, "67890", "m-1", "proposal", "expose grafana"), plans, nil); got == same {
		t.Errorf("two conversations share the key %s", got)
	}
	if got := chatProposalDedupe(chatDedupe(runtimedb.ButlerSurfaceAdmin, tp, convo, "m-1", "proposal", "expose grafana"), plans, nil); got == same {
		t.Errorf("two surfaces share the key %s", got)
	}
	// And it never collides with the reply the SUCCESSFUL path would
	// send for the same message, which would silently swallow the
	// answer when a retry finally worked.
	if same == chatDedupe(surface, tp, convo, "m-1", "bs-1", "expose grafana") {
		t.Error("the notice and the eventual answer to the same message share a dedupe key")
	}
}

// A proposal the queue REFUSED is told too, and is the more important
// of the two.
//
// An accepted plan sits in the queue and can be approved late. A
// refused one is gone: ResourceYAML lives on the tool call and never
// reaches the stored transcript, so the resumed turn has nothing left
// to submit. Saying nothing here is a change the person asked for that
// silently never happens.
func TestAFailedChatTurnAnnouncesARefusedProposal(t *testing.T) {
	h := newHarness(t)
	f := wireFailingButler(t, h)
	// Valid enough to be attempted, invalid enough to be refused before
	// it reaches the queue.
	f.answer.Calls[0].ResourceYAML = "apiVersion: wildstone.dev/v1\nkind: Container\nmetadata: {}\n"

	resp, _ := h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345",
		"text": "expose grafana", "messageId": "m-1",
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	// Nothing reached the queue...
	var plans []runtimedb.PlanRecord
	h.do(runtimedb.RoleAdmin, "GET", "/api/v1/plans", nil, &plans)
	if len(plans) != 0 {
		t.Fatalf("plans = %+v, want the proposal to have been refused", plans)
	}
	// ...and the person was told so.
	queued := h.poll(t, runtimedb.RoleConcierge, "")
	if len(queued) != 1 {
		t.Fatalf("queued = %+v, want one notice about the refusal", queued)
	}
	if !strings.Contains(queued[0].Body, "refused") {
		t.Fatalf("the notice does not say the proposal was refused:\n%s", queued[0].Body)
	}
	// And it does not leave them waiting for an approval that will
	// never appear.
	if strings.Contains(queued[0].Body, "approve it in the Console") {
		t.Fatalf("a refused proposal was announced as if it were waiting for approval:\n%s", queued[0].Body)
	}
	// The recovery it names has to be one that works. Asking again in
	// this conversation resumes the same task, and the accepted
	// propose_change is a verified ledger step the resume replays
	// instead of re-running — so a bare "ask me again" would send the
	// person down a path that queues nothing and looks like it worked.
	// /new clears the thread pointer, which is what makes the next
	// question a fresh task.
	if !strings.Contains(queued[0].Body, "/new") {
		t.Fatalf("the notice tells the person to retry without telling them to start a new conversation first:\n%s", queued[0].Body)
	}
}

// The same holds when one proposal landed and another was refused: the
// refused half still needs a conversation that will actually retry it.
func TestANoticeWithBothOutcomesNamesBothRecoveries(t *testing.T) {
	h := newHarness(t)
	f := wireFailingButler(t, h)
	good := f.answer.Calls[0]
	bad := good
	bad.ResourceYAML = "apiVersion: wildstone.dev/v1\nkind: Container\nmetadata: {}\n"
	f.answer.Calls = []butler.ToolCallRecord{good, bad}

	h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345",
		"text": "expose grafana and photos", "messageId": "m-1",
	})

	queued := h.poll(t, runtimedb.RoleConcierge, "")
	if len(queued) != 1 {
		t.Fatalf("queued = %+v, want one notice covering both outcomes", queued)
	}
	body := queued[0].Body
	if !strings.Contains(body, "approve it in the Console") {
		t.Errorf("the notice does not say what to do with the plan that landed:\n%s", body)
	}
	if !strings.Contains(body, "refused") || !strings.Contains(body, "/new") {
		t.Errorf("the notice does not say what to do about the one that was refused:\n%s", body)
	}
}

// Asking again after a refusal is told again.
//
// The refusal notice says "ask me again", and that is the whole point
// of it. If a second question hits the same validation error and keys
// the same way, the dedupe row swallows the answer permanently — the
// bot invites a retry and then goes quiet on it.
func TestAskingAgainAfterARefusalIsToldAgain(t *testing.T) {
	h := newHarness(t)
	f := wireFailingButler(t, h)
	f.answer.Calls[0].ResourceYAML = "apiVersion: wildstone.dev/v1\nkind: Container\nmetadata: {}\n"

	// Two different questions, each refused the same way.
	for _, m := range []struct{ id, text string }{
		{"m-1", "expose grafana"},
		{"m-2", "expose grafana, please"},
	} {
		resp, _ := h.send(runtimedb.RoleConcierge, map[string]any{
			"transport": "telegram", "account": "12345",
			"text": m.text, "messageId": m.id,
		})
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s: status = %d, want 502", m.id, resp.StatusCode)
		}
	}

	if queued := h.poll(t, runtimedb.RoleConcierge, ""); len(queued) != 2 {
		t.Fatalf("queued %d notices for two separate questions, want 2:\n%+v", len(queued), queued)
	}
}

// A redelivery of ONE question is told once, however many times the
// adapter posts it.
func TestARedeliveredQuestionIsToldOnce(t *testing.T) {
	h := newHarness(t)
	f := wireFailingButler(t, h)
	f.answer.Calls[0].ResourceYAML = "apiVersion: wildstone.dev/v1\nkind: Container\nmetadata: {}\n"

	body := map[string]any{
		"transport": "telegram", "account": "12345",
		"text": "expose grafana", "messageId": "m-1",
	}
	h.send(runtimedb.RoleConcierge, body)
	h.send(runtimedb.RoleConcierge, body)
	h.send(runtimedb.RoleConcierge, body)

	if queued := h.poll(t, runtimedb.RoleConcierge, ""); len(queued) != 1 {
		t.Fatalf("queued %d notices for one redelivered question, want 1:\n%+v", len(queued), queued)
	}
}

// A turn that failed with nothing proposed says nothing. An error the
// adapter is about to retry is not worth a message, and a "something
// went wrong" for every transient provider blip is how a household
// learns to ignore the bot.
func TestAFailedChatTurnWithNothingProposedStaysQuiet(t *testing.T) {
	h := newHarness(t)
	f := wireFailingButler(t, h)
	f.answer.Calls = nil

	resp, _ := h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345",
		"text": "how are the nodes?", "messageId": "m-1",
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if queued := h.poll(t, runtimedb.RoleConcierge, ""); len(queued) != 0 {
		t.Fatalf("queued = %+v, want nothing said about a turn that proposed nothing", queued)
	}
}

// The notice names a command, so the command has to exist on the
// surfaces the notice can reach. Renaming /new without this would leave
// the bot telling people to type something it answers with "I don't
// know that one", on the one path where they have been told the retry
// will not work otherwise.
func TestTheNoticeNamesACommandThatExists(t *testing.T) {
	const named = "new"
	if !strings.Contains(chatProposalNotice(nil, []string{"400: refused"}), "/"+named) {
		t.Fatalf("this test is guarding the wrong command name")
	}
	for _, surface := range []runtimedb.ButlerSurface{
		runtimedb.ButlerSurfaceFamily, runtimedb.ButlerSurfaceAdmin,
	} {
		var found bool
		for _, c := range chatCommands {
			found = found || (c.Name == named && c.offeredOn(surface))
		}
		if !found {
			t.Errorf("the notice tells people on %s to send /%s, and that surface does not offer it", surface, named)
		}
	}
}
