package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// A bot belongs to exactly one Agent (ADR-W033 B7), so credentialing
// one says which — and says it once.
//
// This replaced a check that the capability's name and a -surface flag
// agreed. That guard could only ever tell the two CLASSES apart: both
// Alfred and Robert are `admin`, so it would have let one Owner Agent's
// bot be credentialed for the other without noticing. The ceiling comes
// from the named Agent now, and there is no flag left to disagree with.
func TestCredentiallingABotNamesTheAgentItServes(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ name, capability, butler, want string }{
		{"no agent named", "chat-telegram-family", "", "a bot belongs to one agent"},
		{"unknown agent", "chat-telegram-family", "nobody", "no butler called"},
		{"not a chat capability", "chat", "sandy", "not a chat capability"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.agentAt(t, "family", "telegram")
			var body map[string]any
			resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/adapters/"+tc.capability,
				map[string]any{"node": "n1", "butler": tc.butler}, &body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if msg, _ := body["error"].(string); !strings.Contains(msg, tc.want) {
				t.Fatalf("error = %q, want it to mention %q", msg, tc.want)
			}
		})
	}
}

// A bot's token is one Agent's. Credentialing a bot that already
// answers for somebody else is refused rather than quietly repointed:
// two Agents sharing one bot would make "which agent answered" a
// question with no answer, which is what B7 binds a Connection to
// exactly one Agent to prevent.
func TestABotsTokenIsNotSharedBetweenAgents(t *testing.T) {
	h := newHarness(t)
	// sandy already answers at the family telegram bot.
	at := h.agentAt(t, "family", "telegram")
	h.agentAt(t, "admin", "telegram")

	var body map[string]any
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/adapters/"+at.ExternalID,
		map[string]any{"node": "n1", "butler": agentFor("admin")}, &body)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, at.Butler) || !strings.Contains(msg, "revoke the address") {
		t.Fatalf("error = %q, want it to name the agent that holds the bot and what to do", msg)
	}
	// And the address did not move.
	still, err := h.s.DB.ChatConnectionAt("telegram", at.ExternalID)
	if err != nil || still.Butler != at.Butler {
		t.Fatalf("connection = %+v (err %v), want it still %s's", still, err, at.Butler)
	}
}

// The surface decides the role, and it is never the butler role every
// external agent holds.
func TestRoleForSurface(t *testing.T) {
	family, err := roleForSurface(runtimedb.ButlerSurfaceFamily)
	if err != nil || family != runtimedb.RoleConcierge {
		t.Fatalf("family role = %q (err %v), want concierge", family, err)
	}
	admin, err := roleForSurface(runtimedb.ButlerSurfaceAdmin)
	if err != nil || admin != runtimedb.RoleValet {
		t.Fatalf("admin role = %q (err %v), want valet", admin, err)
	}
	if admin == runtimedb.RoleButler {
		t.Fatalf("the admin adapter runs as the butler role — external agents would share its verbs")
	}
	if _, err := roleForSurface("household"); err == nil {
		t.Fatalf("an unknown surface produced a role")
	}
}
