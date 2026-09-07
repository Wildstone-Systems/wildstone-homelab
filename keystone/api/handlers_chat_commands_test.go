package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// ask sends one message and returns the body the bot queued back.
func (h *harness) ask(t *testing.T, role runtimedb.Role, account, text, surface string) string {
	t.Helper()
	before := len(h.poll(t, role, ""))
	body := map[string]any{
		"transport": "telegram", "account": account, "text": text,
		"messageId": text + account,
	}
	if surface != "" {
		body["surface"] = surface
	}
	resp, _ := h.send(role, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("send %q: status = %d", text, resp.StatusCode)
	}
	queued := h.poll(t, role, "")
	if len(queued) <= before {
		t.Fatalf("send %q queued nothing", text)
	}
	return queued[len(queued)-1].Body
}

// The wall between the two butlers is the whole point of ADR-W026 A1.
// A home-lab command must not merely be refused on the family surface —
// it must not exist there, including in its own help.
func TestHouseholdBotOffersNoHomeLabCommands(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "sam", "telegram", "12345")

	help := h.ask(t, runtimedb.RoleConcierge, "12345", "/help", "")
	for _, forbidden := range []string{"/status", "/plans", "/nodes", "/backups", "/services"} {
		if strings.Contains(help, forbidden) {
			t.Errorf("household /help offers %s", forbidden)
		}
	}
	for _, offered := range []string{"/today", "/actions", "/new", "/help", "/whoami"} {
		if !strings.Contains(help, offered) {
			t.Errorf("household /help is missing %s", offered)
		}
	}
	for _, cmd := range []string{"/status", "/plans", "/nodes"} {
		if got := h.ask(t, runtimedb.RoleConcierge, "12345", cmd, ""); !strings.Contains(got, "I don't know") {
			t.Errorf("household answered %s: %q", cmd, got)
		}
	}
}

func TestOperatorBotOffersTheHomeLabCommands(t *testing.T) {
	h := newHarness(t)
	h.wireButler(nil, runtimedb.ButlerModeFull)
	h.bindAccount(t, "admin", "op", "telegram", "12345")

	help := h.ask(t, runtimedb.RoleValet, "12345", "/help", "admin")
	for _, offered := range []string{"/status", "/plans", "/nodes", "/backups", "/services", "/new", "/whoami"} {
		if !strings.Contains(help, offered) {
			t.Errorf("operator /help is missing %s", offered)
		}
	}
	for _, forbidden := range []string{"/today", "/actions"} {
		if strings.Contains(help, forbidden) {
			t.Errorf("operator /help offers %s", forbidden)
		}
	}
}

// A command answers from stored state, so it must work with no Butler
// configured at all — that is the argument for making them
// deterministic rather than prompts.
func TestCommandsAnswerWithNoButlerConfigured(t *testing.T) {
	h := newHarness(t)
	h.wireButler(nil, runtimedb.ButlerModeFull)
	h.bindAccount(t, "admin", "op", "telegram", "12345")
	h.s.ButlerHost = nil

	got := h.ask(t, runtimedb.RoleValet, "12345", "/status", "admin")
	if !strings.Contains(got, "no nodes have joined yet") {
		t.Fatalf("/status with no butler = %q", got)
	}
}

// An unrecognised slash command is answered, not handed to a model. A
// typo'd /aprove fed to an LLM produces a confident sentence about
// approving something.
func TestUnknownCommandIsRefusedNotAsked(t *testing.T) {
	h := newHarness(t)
	h.wireButler(answerTurns("the model should not have run"), runtimedb.ButlerModeFull)
	h.bindAccount(t, "admin", "op", "telegram", "12345")

	got := h.ask(t, runtimedb.RoleValet, "12345", "/aprove plan-1", "admin")
	if !strings.Contains(got, "I don't know /aprove") {
		t.Fatalf("unknown command = %q", got)
	}
	if len(h.scripted(t).convos) != 0 {
		t.Fatalf("an unknown command reached the model")
	}
	var sessions []runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &sessions)
	if len(sessions) != 0 {
		t.Fatalf("an unknown command opened a session: %+v", sessions)
	}
}

// /whoami has to say which bot this is, because a person has both in
// one app and the difference is what each is allowed to do.
func TestWhoamiNamesTheSurfaceAndItsCeiling(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	h.bindAccount(t, "admin", "op", "telegram", "999")

	family := h.ask(t, runtimedb.RoleConcierge, "12345", "/whoami", "family")
	if !strings.Contains(family, "HOUSEHOLD") || !strings.Contains(family, "no access to the home lab") {
		t.Fatalf("family /whoami = %q", family)
	}
	if !strings.Contains(family, "sam") {
		t.Fatalf("family /whoami does not say who you speak as: %q", family)
	}
	admin := h.ask(t, runtimedb.RoleValet, "999", "/whoami", "admin")
	if !strings.Contains(admin, "OPERATOR") {
		t.Fatalf("admin /whoami = %q", admin)
	}
	if !strings.Contains(admin, "cannot approve") {
		t.Fatalf("admin /whoami does not state the approval ceiling: %q", admin)
	}
}

// A room hears about the house, not about the other person
// (ADR-W026 A7). /today reads the timeline directly rather than through
// the family seam, so it has to apply the same rule the seam does —
// otherwise it discloses one member's movements to everyone in the
// room. Reported by Codex on #110 as a P1.
func TestTodayWithholdsAnotherMembersFactsInAGroup(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
	h.s.MemberScoped = func(eventType string) bool { return eventType == "presence.arrived" }
	h.bindAccount(t, "family", "sam", "telegram", "12345")

	for _, e := range []runtimedb.HouseholdEvent{
		{EventID: "e1", Source: "test", Type: "presence.arrived", Subject: "robin", OccurredAt: time.Now()},
		{EventID: "e2", Source: "test", Type: "presence.arrived", Subject: "sam", OccurredAt: time.Now()},
		{EventID: "e3", Source: "test", Type: "bins.collected", OccurredAt: time.Now()},
	} {
		if _, _, err := h.s.DB.AppendHouseholdEvent(e); err != nil {
			t.Fatal(err)
		}
	}

	// In a direct chat, sam sees everything the household recorded.
	direct := h.ask(t, runtimedb.RoleConcierge, "12345", "/today", "family")
	if !strings.Contains(direct, "robin") {
		t.Fatalf("direct /today withheld a fact it should show: %q", direct)
	}

	// In a room, robin's movements are not sam's to broadcast.
	var out inboundResponse
	resp := h.do(runtimedb.RoleConcierge, "POST", "/api/v1/chat/messages", map[string]any{
		"transport": "telegram", "account": "12345", "conversation": "room-1",
		"group": true, "text": "/today", "messageId": "g-1", "surface": "family",
	}, &out)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("group /today: status = %d", resp.StatusCode)
	}
	queued := h.poll(t, runtimedb.RoleConcierge, "")
	group := queued[len(queued)-1].Body
	if strings.Contains(group, "robin") {
		t.Fatalf("group /today disclosed another member's movements: %q", group)
	}
	// It still answers — the house's own facts are the room's business.
	if !strings.Contains(group, "bins.collected") {
		t.Fatalf("group /today withheld a fact about the house: %q", group)
	}
}

// A plan is readable over chat, and reading it says plainly that
// approving is not.
func TestPlanCommandReadsButSaysApprovalIsElsewhere(t *testing.T) {
	h := newHarness(t)
	h.wireButler(proposeGrafanaTurns(), runtimedb.ButlerModeFull)
	h.bindAccount(t, "admin", "op", "telegram", "12345")

	var ask askResponse
	h.do(runtimedb.RoleOperator, "POST", "/api/v1/butler/ask", map[string]string{"message": "expose grafana"}, &ask)
	if len(ask.PlanIDs) != 1 {
		t.Fatalf("no plan proposed: %+v", ask)
	}

	list := h.ask(t, runtimedb.RoleValet, "12345", "/plans", "admin")
	if !strings.Contains(list, ask.PlanIDs[0]) {
		t.Fatalf("/plans omitted the pending plan: %q", list)
	}
	one := h.ask(t, runtimedb.RoleValet, "12345", "/plan "+ask.PlanIDs[0], "admin")
	if !strings.Contains(one, "Console") {
		t.Fatalf("/plan does not say where approval happens: %q", one)
	}
	if got := h.ask(t, runtimedb.RoleValet, "12345", "/plan plan-nope", "admin"); !strings.Contains(got, "don't have a plan") {
		t.Fatalf("/plan with a bad id = %q", got)
	}
}
