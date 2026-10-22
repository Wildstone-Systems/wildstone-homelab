package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerlive"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerstore"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// webCommandAgents sets up a household Agent and an operator Agent, both
// granted to sam, and giulia admitted with access to neither.
func webCommandAgents(t *testing.T, h *harness) {
	t.Helper()
	if err := h.s.DB.UpsertModel(runtimedb.Model{Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama",
		ProviderModel: "llama", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{"sam", "giulia"} {
		if err := h.s.DB.RecordButlerMember(member, runtimedb.RoleMember, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.AdmitButlerMember(member, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []runtimedb.Butler{
		{Name: "sandy", Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "kitchen-box"},
		{Name: "operator", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Model: "kitchen-box"},
	} {
		if _, err := h.s.DB.CreateButler(b, "root-admin"); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.GrantButlerAccess(b.Name, "sam", "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
}

type assistantReply struct {
	Session         string `json:"session"`
	Reply           string `json:"reply"`
	Command         string `json:"command"`
	NewConversation bool   `json:"newConversation"`
	Error           string `json:"error"`
}

// The household dashboard's assistant answers slash commands itself, as
// a chat transport does: with no model (no Butler backend is wired in
// this harness at all), and only the commands the Agent's surface
// offers.
func TestSlashCommandsOnTheDashboardAssistant(t *testing.T) {
	h, _ := portalHarness(t)
	webCommandAgents(t, h)

	ask := func(agent, text string) (int, assistantReply) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"agent": agent, "text": text})
		res := h.member(http.MethodPost, "/api/v1/assistant/ask", "sam", "assistant_users",
			bytes.NewReader(body), "application/json")
		defer res.Body.Close()
		var out assistantReply
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}

	status, out := ask("sandy", "/help")
	if status != http.StatusOK || out.Command != "help" {
		t.Fatalf("/help = %d %+v", status, out)
	}
	if !strings.Contains(out.Reply, "/new") || !strings.Contains(out.Reply, "/today") {
		t.Fatalf("/help does not list the household commands: %q", out.Reply)
	}
	if strings.Contains(out.Reply, "/status") {
		t.Fatalf("/help on a household Agent offers an operator command: %q", out.Reply)
	}

	// An operator command on a household Agent is answered as unknown,
	// here as over chat (ADR-W026 A1), and never reaches a model.
	if status, out = ask("sandy", "/status"); status != http.StatusOK || !strings.Contains(out.Reply, "I don't know /status") {
		t.Fatalf("/status on the household Agent = %d %+v", status, out)
	}
	if status, out = ask("sandy", "/wat"); status != http.StatusOK || !strings.Contains(out.Reply, "I don't know /wat") {
		t.Fatalf("an unknown command = %d %+v", status, out)
	}

	// /new tells the page to start fresh.
	if status, out = ask("sandy", "/new"); status != http.StatusOK || !out.NewConversation || out.Session != "" {
		t.Fatalf("/new = %d %+v", status, out)
	}
	if status, out = ask("sandy", "/reset"); status != http.StatusOK || !out.NewConversation {
		t.Fatalf("/reset = %d %+v", status, out)
	}

	// /sessions and /resume work, over the trusted UI's own candidates.
	if status, out = ask("sandy", "/sessions"); status != http.StatusOK || out.Command != "sessions" {
		t.Fatalf("/sessions = %d %+v", status, out)
	}
	if status, out = ask("sandy", "/resume not-a-choice"); status != http.StatusOK || !strings.Contains(out.Reply, "invalid or has expired") || out.Session != "" {
		t.Fatalf("/resume with a bad choice = %d %+v", status, out)
	}

	// The ceiling comes first: naming the operator Agent here is refused
	// exactly as a question would be, so a command cannot confirm it.
	if status, out = ask("operator", "/help"); status != http.StatusNotFound || out.Command != "" {
		t.Fatalf("a command to the operator Agent through the dashboard = %d %+v", status, out)
	}
}

// The Console answers slash commands for any Agent a question could
// reach, with that Agent's surface deciding which ones.
func TestSlashCommandsInTheConsoleChat(t *testing.T) {
	h := newHarness(t)
	webCommandAgents(t, h)
	lease, err := h.s.DB.AcquireCoordinator("web-commands-test")
	if err != nil {
		t.Fatal(err)
	}
	h.s.Butlers = &butlerlive.Butlers{DB: h.s.DB, Ledger: butlerstore.New(h.s.DB, lease)}

	ask := func(butler, member, text string) (askResponse, int, error) {
		t.Helper()
		out, code, err := h.s.runNamedButlerSession(context.Background(), butlerlive.Turn{
			Butler: butler, Member: member, Text: text,
			Connection: butlerlive.ConnectionScope{Name: butlerlive.ConsoleConnection},
		}, nil)
		resp, _ := out.(askResponse)
		return resp, code, err
	}

	resp, code, err := ask("operator", "sam", "/status")
	if err != nil || code != http.StatusOK || resp.Command != "status" || !strings.Contains(resp.FinalText, "resources unhealthy") {
		t.Fatalf("/status on the operator Agent = %d %v %+v", code, err, resp)
	}
	if resp, _, _ = ask("operator", "sam", "/whoami"); !strings.Contains(resp.FinalText, "OPERATOR") || !strings.Contains(resp.FinalText, "the Console") {
		t.Fatalf("/whoami = %+v", resp)
	}
	if resp, _, _ = ask("operator", "sam", "/today"); !strings.Contains(resp.FinalText, "I don't know /today") {
		t.Fatalf("/today on the operator Agent = %+v", resp)
	}
	if resp, _, _ = ask("operator", "sam", "/approve plan-1"); !strings.Contains(resp.FinalText, "Plans screen") {
		t.Fatalf("/approve in the Console = %+v", resp)
	}
	if resp, _, _ = ask("sandy", "sam", "/today"); resp.Command != "today" {
		t.Fatalf("/today on the household Agent = %+v", resp)
	}
	if resp, _, _ = ask("sandy", "sam", "/clear"); !resp.NewConversation {
		t.Fatalf("/clear = %+v", resp)
	}

	// No access, no command: the turn is refused the way a question is.
	resp, code, err = ask("operator", "giulia", "/status")
	if err == nil || code != http.StatusNotFound || resp.Command != "" {
		t.Fatalf("/status without access to the Agent = %d %v %+v", code, err, resp)
	}
}
