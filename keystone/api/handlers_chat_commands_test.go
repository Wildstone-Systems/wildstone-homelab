package api

import (
	"net/http"
	"regexp"
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
	// An adapter's own credential is its Connection, so it names
	// nothing (ADR-W031 A7, ADR-W033 B7). A human has no bot to arrive
	// through and names the Agent instead.
	if surface != "" && role != runtimedb.RoleConcierge && role != runtimedb.RoleValet {
		body["butler"] = agentFor(surface)
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
	h.bindAccount(t, "admin", "mika", "telegram", "12345")

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
	h.bindAccount(t, "admin", "mika", "telegram", "12345")
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
	h.bindAccount(t, "admin", "mika", "telegram", "12345")

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
	h.bindAccount(t, "admin", "mika", "telegram", "999")

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

// A room hears nothing (design §8.3, superseding ADR-W026 A7's "the
// house, not the other person").
//
// /today is the case that shows why the MVP answer had to be refusal.
// It reads the timeline DIRECTLY rather than through the family seam,
// and no model runs — so the audience note in the family prompt could
// never have constrained it, and it needed its own copy of the rule to
// avoid disclosing one member's movements to the room (Codex reported
// exactly that on #110 as a P1). Every deterministic path into a room
// needs its own copy of every disclosure rule, forever, and each one
// is a place to forget. Refusing the destination is one rule in one
// place instead.
func TestTodayDisclosesNothingIntoARoom(t *testing.T) {
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

	// In a room it answers nothing at all (design §8.3). This test used
	// to assert the narrower rule — the house's own facts are the
	// room's business, another member's movements are not — which was
	// ADR-W026 A7's bargain, and slice 11's disclosure half ends it:
	// a group destination fails closed in MVP, so the only thing said
	// into a room is the redirect.
	//
	// The old assertion is kept as the first half of the new one:
	// robin's movements still must not appear, and now neither does
	// anything else.
	var out inboundResponse
	resp := h.do(runtimedb.RoleConcierge, "POST", "/api/v1/chat/messages", map[string]any{
		"transport": "telegram", "account": "12345", "conversation": "room-1",
		"group": true, "addressed": true, "text": "/today", "messageId": "g-1",
	}, &out)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("group /today: status = %d", resp.StatusCode)
	}
	queued := h.poll(t, runtimedb.RoleConcierge, "")
	group := queued[len(queued)-1].Body
	if strings.Contains(group, "robin") {
		t.Fatalf("group /today disclosed another member's movements: %q", group)
	}
	// Nor the household's own facts, which a slash command would
	// otherwise have handed to the room deterministically — no model
	// involved, so no amount of prompt discretion was ever going to
	// stop it.
	if strings.Contains(group, "bins.collected") {
		t.Fatalf("group /today disclosed the household's facts into a room: %q", group)
	}
	if !strings.Contains(group, "one-to-one") {
		t.Fatalf("group /today did not redirect: %q", group)
	}
}

// A plan is readable over chat, and reading it says plainly that
// approving is not.
func TestPlanCommandReadsButSaysApprovalIsElsewhere(t *testing.T) {
	h := newHarness(t)
	h.wireButler(proposeGrafanaTurns(), runtimedb.ButlerModeFull)
	h.bindAccount(t, "admin", "mika", "telegram", "12345")

	var ask askResponse
	// The same Person who holds the Telegram Connection, asking from the
	// Console — which is the point of the plan being readable on either.
	h.doToken(h.console(runtimedb.RoleOperator, "mika"), "POST", "/api/v1/butler/ask",
		map[string]string{"message": "expose grafana"}, &ask)
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

// The menu a bot publishes is filtered by surface for the same reason
// /help is. A household bot whose Telegram menu offers /status teaches
// exactly the thing ADR-W026 A1 makes structural — and a menu is more
// convincing than help text, because it is tappable.
func TestCommandMenuIsFilteredBySurface(t *testing.T) {
	family := commandsFor(runtimedb.ButlerSurfaceFamily)
	admin := commandsFor(runtimedb.ButlerSurfaceAdmin)

	has := func(cmds []chatCommandInfo, name string) bool {
		for _, c := range cmds {
			if c.Command == name {
				return true
			}
		}
		return false
	}
	for _, name := range []string{"status", "plans", "plan", "nodes", "backups", "services"} {
		if has(family, name) {
			t.Errorf("the household menu offers /%s", name)
		}
		if !has(admin, name) {
			t.Errorf("the operator menu is missing /%s", name)
		}
	}
	for _, name := range []string{"today", "actions"} {
		if !has(family, name) {
			t.Errorf("the household menu is missing /%s", name)
		}
		if has(admin, name) {
			t.Errorf("the operator menu offers /%s", name)
		}
	}
	for _, name := range []string{"help", "whoami", "new"} {
		if !has(family, name) || !has(admin, name) {
			t.Errorf("/%s should be on both menus", name)
		}
	}
	// Aliases collapse: the menu exists for discovery, and three
	// entries that do one thing is clutter.
	if has(admin, "reset") || has(admin, "clear") {
		t.Errorf("aliases leaked into the menu: %+v", admin)
	}
}

// Telegram refuses a command whose name is not 1-32 of [a-z0-9_], and
// a description outside 3-256 characters. A menu entry the vendor
// rejects takes the WHOLE call down, so one bad help string would leave
// every command unlisted.
func TestCommandMenuEntriesAreValidForTelegram(t *testing.T) {
	name := regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	for _, surface := range []runtimedb.ButlerSurface{runtimedb.ButlerSurfaceAdmin, runtimedb.ButlerSurfaceFamily} {
		for _, c := range commandsFor(surface) {
			if !name.MatchString(c.Command) {
				t.Errorf("%s: %q is not a valid Telegram command name", surface, c.Command)
			}
			if n := len(c.Description); n < 3 || n > 256 {
				t.Errorf("%s: /%s description is %d characters", surface, c.Command, n)
			}
			// The name is the bare word: Telegram renders the slash.
			if strings.HasPrefix(c.Command, "/") {
				t.Errorf("%s: /%s carries its own slash", surface, c.Command)
			}
		}
	}
}

// The menu comes back with the registration, so an adapter never has to
// carry its own copy of the list — the drift that would produce is a
// household bot advertising /status.
func TestRegisterReturnsTheSurfacesCommands(t *testing.T) {
	h := newHarness(t)
	var out struct {
		Surface  string            `json:"surface"`
		Commands []chatCommandInfo `json:"commands"`
	}
	token := h.adapterToken(t, "chat-telegram-family")
	resp := h.doToken(token, http.MethodPost, "/api/v1/chat/adapters/self",
		map[string]any{"transport": "telegram", "handle": "wildstone_house_bot"}, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register: status = %d", resp.StatusCode)
	}
	if out.Surface != "family" {
		t.Fatalf("surface = %q", out.Surface)
	}
	if len(out.Commands) == 0 {
		t.Fatal("registration returned no commands")
	}
	for _, c := range out.Commands {
		if c.Command == "status" {
			t.Fatalf("a family adapter was handed the operator's menu: %+v", out.Commands)
		}
	}
}
