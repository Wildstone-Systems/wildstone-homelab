package pkgtool

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// The pantry package's agentTools and keystone/domestic.PantryTools are
// two halves of one list (ADR-W044 A5): a tool the package declares that
// nothing registers is a tool the Butler offers and cannot perform, and a
// registered tool the package does not declare is refused on every call.
func TestPantryToolsAreDeclaredByThePackage(t *testing.T) {
	sp, ok := walkShipped(t)["pantry-pantryos"]
	if !ok {
		t.Fatal("pantry-pantryos is not in the catalog")
	}
	if sp.pkg.Metadata.Capability != domestic.PantryCapability {
		t.Fatalf("the package's capability is %q but the executors register under %q",
			sp.pkg.Metadata.Capability, domestic.PantryCapability)
	}
	declared := map[string]string{}
	for _, tool := range sp.pkg.AgentTools {
		declared[tool.Name] = tool.Risk
	}
	for name, tier := range domestic.PantryTools {
		got, present := declared[name]
		if !present {
			t.Errorf("keystone registers an executor for pantry.%s, which the package does not declare", name)
			continue
		}
		if got != tier {
			t.Errorf("pantry.%s is declared at tier %q and registered at %q", name, got, tier)
		}
	}
	for name, tier := range declared {
		if _, present := domestic.PantryTools[name]; !present {
			t.Errorf("the package declares pantry.%s, which nothing registers", name)
		}
		if !domestic.TierAllowed(tier, true) {
			t.Errorf("pantry.%s is declared at tier %q, which a family session cannot carry", name, tier)
		}
	}
	// Only finishing a cook touches stock all at once; it alone asks.
	if declared["finish_cooking"] != domestic.TierConfirm {
		t.Error("finish_cooking must be confirm: it takes every ingredient out of the pantry at once")
	}
}

// Every shipped family chat package is a routine delivery target. Chat
// packages are instanced, so their capability is "chat-matrix" and an
// installed bot "chat-matrix-<name>"; selecting them by a "-family" suffix
// once wired none of them, and no routine could send a chat message.
func TestEveryShippedChatPackageCanDeliverARoutine(t *testing.T) {
	var sources []domestic.AgentToolSource
	var chatPackages []string
	for _, sp := range walkShipped(t) {
		tools := map[string]string{}
		for _, tool := range sp.pkg.AgentTools {
			tools[tool.Name] = tool.Risk
		}
		sources = append(sources, domestic.AgentToolSource{Capability: sp.pkg.Metadata.Capability, Tools: tools})
		if _, sends := tools["send"]; sends && strings.HasPrefix(sp.pkg.Metadata.Capability, "chat-") {
			chatPackages = append(chatPackages, sp.pkg.Metadata.Capability)
		}
	}
	if len(chatPackages) == 0 {
		t.Fatal("no shipped chat package declares send")
	}
	wired := map[string]bool{}
	for _, c := range domestic.ChatSendCapabilities(sources) {
		wired[c] = true
	}
	for _, c := range chatPackages {
		if !wired[c] {
			t.Errorf("chat package %s declares send but is not wired as a routine delivery target", c)
		}
	}
	for _, c := range []string{"chat-discord", "chat-matrix", "chat-slack", "chat-telegram"} {
		if !wired[c] {
			t.Errorf("%s is not wired as a routine delivery target", c)
		}
	}
}

// docs/routines/kitchen-timer.yaml is the routine the household pastes in.
// It must stay a valid routine over things that exist: the event the pantry
// package publishes, and a send its chat package declares at that tier — and
// its message must render from what the event is certain to carry.
func TestTheKitchenTimerRoutineIsValid(t *testing.T) {
	raw, err := os.ReadFile("../../docs/routines/kitchen-timer.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := resource.Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	a := r.Automation
	shipped := walkShipped(t)

	pantryPkg := shipped["pantry-pantryos"].pkg
	var event *pkgspec.PublishedEvent
	if pantryPkg.Publishes == nil {
		t.Fatal("pantry-pantryos publishes nothing")
	}
	for i := range pantryPkg.Publishes.Events {
		if pantryPkg.Publishes.Events[i].Type == a.Trigger.EventType {
			event = &pantryPkg.Publishes.Events[i]
		}
	}
	if event == nil {
		t.Fatalf("the routine triggers on %s, which pantry-pantryos does not publish", a.Trigger.EventType)
	}
	if event.Subject != "member" {
		t.Fatalf("%s has subject %q; the routine sends to the event's member", event.Type, event.Subject)
	}

	capName, tool, _ := strings.Cut(a.Action.Tool, ".")
	chat, ok := shipped[capName]
	if !ok {
		t.Fatalf("the routine sends through %s, which no shipped package provides", capName)
	}
	declared := ""
	for _, tl := range chat.pkg.AgentTools {
		if tl.Name == tool {
			declared = tl.Risk
		}
	}
	if declared != a.Action.Tier {
		t.Fatalf("%s is declared at tier %q; the routine says %q", a.Action.Tool, declared, a.Action.Tier)
	}
	if _, sendsToMember := a.Action.Params["member"]; sendsToMember {
		t.Fatal("the routine names a member; it must send to whoever set the timer")
	}

	// Only the required payload fields: a missing optional field fails the
	// render (missingkey=error), and the routine with it.
	ev := runtimedb.HouseholdEvent{Type: a.Trigger.EventType, Subject: "sam", OccurredAt: time.Now(),
		Payload: json.RawMessage(`{"label":"Pasta"}`)}
	params, err := automation.RenderParams(a.Action.Params, ev, r.Metadata.Name, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if params["message"] != "⏰ Your Pasta timer is done." {
		t.Fatalf("message renders as %q", params["message"])
	}
}
