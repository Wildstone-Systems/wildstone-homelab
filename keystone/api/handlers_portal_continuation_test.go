package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// With several recent conversations, the dashboard is not told to go and
// find one: nothing is continued (design §7, "Ambiguous continuation"),
// and the refusal carries the conversations to offer, most recent first,
// so the page can ask and send the same message to the one picked.
func TestTheDashboardIsHandedTheConversationsToChooseFrom(t *testing.T) {
	h, _ := portalHarness(t)
	webCommandAgents(t, h)
	h.s.Butlers = &butlerlive.Butlers{DB: h.s.DB}

	var ids []string
	for _, subject := range []string{"the shopping list", "dinner on friday"} {
		id, err := h.s.DB.InsertButlerSession(runtimedb.ButlerSession{
			Principal: "butler", ActingFor: "sam", Provider: "test", Mode: runtimedb.ButlerModeFull,
			Surface: runtimedb.ButlerSurfaceFamily, Butler: "sandy", FinalText: "ok",
			Turns: []runtimedb.ButlerTurn{
				{Role: runtimedb.ButlerRoleUser, Text: subject},
				{Role: runtimedb.ButlerRoleAssistant, Text: "ok"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.NoteButlerSessionConnection(id, "inbound", portal.Connection, 0); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	body, _ := json.Marshal(map[string]string{"agent": "sandy", "text": "add eggs"})
	res := h.member(http.MethodPost, "/api/v1/assistant/ask", "sam", "assistant_users",
		bytes.NewReader(body), "application/json")
	defer res.Body.Close()
	var out struct {
		Error  string `json:"error"`
		Choose []struct {
			Session string `json:"session"`
			Subject string `json:"subject"`
			At      string `json:"at"`
		} `json:"choose"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%+v)", res.StatusCode, out)
	}
	if out.Error == "" {
		t.Error("the refusal lost its message")
	}
	if len(out.Choose) != 2 {
		t.Fatalf("choices = %+v, want both recent conversations", out.Choose)
	}
	got := map[string]string{}
	for _, c := range out.Choose {
		if c.At == "" {
			t.Errorf("choice %s carries no time on the trusted dashboard", c.Session)
		}
		got[c.Session] = c.Subject
	}
	if got[ids[0]] != "the shopping list" || got[ids[1]] != "dinner on friday" {
		t.Fatalf("choices = %+v, want each conversation with its subject", out.Choose)
	}
}
