package api

// Chat Connections in one step (ADR-W033 B7.1).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

const telegramToken = "123456:AAH-very-secret-bot-token"

// connectHarness is a one-machine household running the shipped catalog,
// with a Member Agent (joan), an Owner Agent (alfred), an Owner (mika)
// and a Member (sandy).
func connectHarness(t *testing.T) (*harness, *secretsFakeAgent) {
	t.Helper()
	h := homeHarness(t)
	c, err := catalog.LoadDir("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	h.s.Catalog = c
	h.s.APIURL = "http://100.64.0.1:9441"
	if err := h.s.DB.RecordNodeJoin("n1"); err != nil {
		t.Fatal(err)
	}
	fake := &secretsFakeAgent{}
	h.s.Agents = fake
	if err := h.s.DB.UpsertModel(runtimedb.Model{Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama",
		ProviderModel: "llama", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []runtimedb.Butler{
		{Name: "joan", Directive: "look after the house", Surface: runtimedb.ButlerSurfaceFamily, Profile: runtimedb.ProfileHousehold, Model: "kitchen-box"},
		{Name: "alfred", Directive: "run the homelab", Surface: runtimedb.ButlerSurfaceAdmin, Profile: runtimedb.ProfileOperator, Model: "kitchen-box"},
	} {
		if _, err := h.s.DB.CreateButler(b, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	for member, role := range map[string]string{"mika": runtimedb.RoleOwner, "sandy": runtimedb.RoleMember} {
		if err := h.s.DB.RecordButlerMember(member, role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.AdmitButlerMember(member, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	return h, fake
}

func (h *harness) connect(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	var out map[string]any
	resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/connections", body, &out)
	return resp.StatusCode, out
}

func telegramFor(butler, person string) map[string]any {
	return map[string]any{"platform": "telegram", "butler": butler, "person": person,
		"credentials": map[string]string{"botToken": telegramToken}}
}

// The whole of it: three answers make a bot for the Agent, install it as
// its own instance, and leave the Person an invite they collect on their
// own dashboard. The key comes to them, not to the Owner.
func TestConnectingAPersonTakesThreeAnswers(t *testing.T) {
	h, fake := connectHarness(t)
	code, out := h.connect(t, telegramFor("joan", "sandy"))
	if code != http.StatusCreated || out["capability"] != "chat-telegram-joan" {
		t.Fatalf("connect = %d %v", code, out)
	}
	for _, name := range []string{"chat-telegram-joan-bot", "chat-telegram-joan-adapter"} {
		if fake.secrets[name] == 0 {
			t.Errorf("secret %s was not placed on the node", name)
		}
	}
	current, _ := h.s.State.Load()
	var container *resource.Resource
	for i, r := range current {
		if r.Kind == resource.KindContainer && r.Metadata.Name == "chat-telegram-joan" {
			container = &current[i]
		}
	}
	if container == nil || container.Metadata.Owner != "capability/chat-telegram-joan" {
		t.Fatalf("the instance's container = %+v", container)
	}
	if container.Container.Env["KEYSTONE_API"] != "http://100.64.0.1:9441" {
		t.Errorf("the adapter reaches Keystone at %q", container.Container.Env["KEYSTONE_API"])
	}
	if at, err := h.s.DB.ChatConnectionAt("telegram", "chat-telegram-joan"); err != nil || at.Butler != "joan" {
		t.Fatalf("connection = %+v %v", at, err)
	}

	// The notice says where to go and nothing secret.
	notices, err := h.s.DB.ListNotices(10, runtimedb.NoticeFilter{Recipient: "sandy"})
	if err != nil || len(notices) != 1 || notices[0].Kind != "chat.invite" {
		t.Fatalf("notices for sandy = %+v %v", notices, err)
	}
	if strings.Contains(notices[0].Question, telegramToken) || !strings.Contains(notices[0].Question, "dashboard") {
		t.Errorf("notice body = %q", notices[0].Question)
	}

	// Sandy collects the key on their own dashboard.
	res := h.member(http.MethodGet, "/api/v1/home/connections", "sandy", "homepage_users", nil, "")
	var list struct {
		Invites []inviteView `json:"invites"`
	}
	json.NewDecoder(res.Body).Decode(&list)
	res.Body.Close()
	if len(list.Invites) != 1 || list.Invites[0].Butler != "joan" || list.Invites[0].Label != "Telegram" {
		t.Fatalf("sandy's invites = %+v", list.Invites)
	}
	keyRes := h.member(http.MethodPost, "/api/v1/home/connections/"+fmt.Sprint(list.Invites[0].Seq)+"/key", "sandy", "homepage_users",
		strings.NewReader("{}"), "application/json")
	var key map[string]any
	json.NewDecoder(keyRes.Body).Decode(&key)
	keyRes.Body.Close()
	if keyRes.StatusCode != http.StatusCreated || key["code"] == "" {
		t.Fatalf("key = %d %v", keyRes.StatusCode, key)
	}

	// Somebody else cannot collect it.
	other := h.member(http.MethodPost, "/api/v1/home/connections/"+fmt.Sprint(list.Invites[0].Seq)+"/key", "mika", "homepage_users",
		strings.NewReader("{}"), "application/json")
	other.Body.Close()
	if other.StatusCode != http.StatusNotFound {
		t.Errorf("mika collected sandy's key: %d", other.StatusCode)
	}

	// Sending it to the bot binds them and closes the invite.
	at, _ := h.s.DB.ChatConnectionAt("telegram", "chat-telegram-joan")
	if _, err := h.s.Principals.RedeemChatClaim(key["code"].(string), at, "777", "Sandy", "777"); err != nil {
		t.Fatalf("redeeming: %v", err)
	}
	if left, _ := h.s.DB.OpenInvitesFor("sandy"); len(left) != 0 {
		t.Errorf("the invite is still open after sandy bound: %+v", left)
	}
}

// A second bot for the same Agent is a second instance, not the first
// one re-pointed.
func TestASecondBotIsItsOwnInstance(t *testing.T) {
	h, _ := connectHarness(t)
	if code, out := h.connect(t, telegramFor("joan", "sandy")); code != http.StatusCreated {
		t.Fatalf("first = %d %v", code, out)
	}
	code, out := h.connect(t, telegramFor("joan", "mika"))
	if code != http.StatusCreated || out["capability"] != "chat-telegram-joan-2" {
		t.Fatalf("second = %d %v", code, out)
	}
}

// An Owner Agent's invite goes to the Console, never the dashboard, and
// a Member cannot be connected to one at all (B9).
func TestAnOwnerAgentAdmitsOwnersOnly(t *testing.T) {
	h, fake := connectHarness(t)
	code, out := h.connect(t, telegramFor("alfred", "sandy"))
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "Owners only") {
		t.Fatalf("a Member connected to an Owner Agent: %d %v", code, out)
	}
	if len(fake.secrets) != 0 {
		t.Errorf("a refused connection still placed secrets: %v", fake.secrets)
	}
	if _, err := h.s.DB.GetPrincipal("chat-chat-telegram-alfred"); err == nil {
		t.Error("a refused connection still minted a credential")
	}

	if code, out := h.connect(t, telegramFor("alfred", "mika")); code != http.StatusCreated {
		t.Fatalf("owner = %d %v", code, out)
	}
	res := h.member(http.MethodGet, "/api/v1/home/connections", "mika", "homepage_users", nil, "")
	var list struct {
		Invites []inviteView `json:"invites"`
	}
	json.NewDecoder(res.Body).Decode(&list)
	res.Body.Close()
	if len(list.Invites) != 0 {
		t.Errorf("an Owner Agent's invite showed on the household dashboard: %+v", list.Invites)
	}
	invites, _ := h.s.DB.OpenInvitesFor("mika")
	if got := viewInvites(invites, runtimedb.ButlerSurfaceAdmin); len(got) != 1 || got[0].Butler != "alfred" {
		t.Errorf("mika's Console invites = %+v", got)
	}
}

// Each platform's credentials are checked before anything is made, and a
// refusal never repeats the credential back.
func TestAPlatformsCredentialsAreChecked(t *testing.T) {
	h, fake := connectHarness(t)
	for _, c := range []struct {
		what string
		body map[string]any
		want string
	}{
		{"an unknown platform", map[string]any{"platform": "irc", "butler": "joan", "person": "sandy"}, "platform must be"},
		{"no token", map[string]any{"platform": "telegram", "butler": "joan", "person": "sandy"}, "needs its bot token"},
		{"a token with spaces", map[string]any{"platform": "telegram", "butler": "joan", "person": "sandy",
			"credentials": map[string]string{"botToken": "not a token at all"}}, "one unbroken string"},
		{"slack without its app token", map[string]any{"platform": "slack", "butler": "joan", "person": "sandy",
			"credentials": map[string]string{"botToken": "xoxb-1234567890"}}, "app-level token"},
		{"slack tokens swapped", map[string]any{"platform": "slack", "butler": "joan", "person": "sandy",
			"credentials": map[string]string{"botToken": "xapp-1234567890", "appToken": "xoxb-1234567890"}}, "starts with xoxb-"},
		{"matrix over http", map[string]any{"platform": "matrix", "butler": "joan", "person": "sandy",
			"credentials": map[string]string{"homeserver": "http://matrix.example.com", "accessToken": "syt_abcdefghij"}}, "https://"},
		{"nobody", map[string]any{"platform": "telegram", "butler": "joan", "person": "nobody",
			"credentials": map[string]string{"botToken": telegramToken}}, "not an admitted member"},
	} {
		code, out := h.connect(t, c.body)
		msg, _ := out["error"].(string)
		if code != http.StatusBadRequest || !strings.Contains(msg, c.want) {
			t.Errorf("%s: %d %q, want 400 mentioning %q", c.what, code, msg, c.want)
		}
		if strings.Contains(msg, "xoxb-1234567890") || strings.Contains(msg, "not a token at all") {
			t.Errorf("%s: the refusal repeated the credential: %q", c.what, msg)
		}
	}
	if len(fake.secrets) != 0 {
		t.Errorf("refused connections placed secrets: %v", fake.secrets)
	}
}

// Every platform the form offers has a package that installs.
func TestEveryPlatformInstalls(t *testing.T) {
	creds := map[string]map[string]string{
		"telegram": {"botToken": telegramToken},
		"discord":  {"botToken": "MTE-discord-token"},
		"slack":    {"botToken": "xoxb-1234567890", "appToken": "xapp-1234567890"},
		"matrix":   {"homeserver": "https://matrix.example.com", "accessToken": "syt_abcdefghij"},
	}
	for _, p := range chatPlatforms {
		t.Run(p.Name, func(t *testing.T) {
			h, fake := connectHarness(t)
			code, out := h.connect(t, map[string]any{"platform": p.Name, "butler": "joan", "person": "sandy", "credentials": creds[p.Name]})
			if code != http.StatusCreated {
				t.Fatalf("%d %v", code, out)
			}
			for _, f := range p.Fields {
				if f.Secret != "" && fake.secrets["chat-"+p.Name+"-joan-"+f.Secret] == 0 {
					t.Errorf("%s was not stored", f.Key)
				}
			}
		})
	}
}

// Removing one bot removes that instance and no other (#246), and an
// instanced package cannot be removed without saying which instance.
func TestRemovingOneBotLeavesTheOthers(t *testing.T) {
	h, _ := connectHarness(t)
	for _, who := range []string{"sandy", "mika"} {
		if code, out := h.connect(t, telegramFor("joan", who)); code != http.StatusCreated {
			t.Fatalf("connect %s = %d %v", who, code, out)
		}
	}
	var out map[string]any
	resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/capabilities/disable",
		map[string]any{"package": "chat-telegram", "dryRun": true}, &out)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("disable with no instance = %d %v", resp.StatusCode, out)
	}
	out = nil
	resp = h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/capabilities/disable",
		map[string]any{"package": "chat-telegram", "instance": "joan", "dryRun": true}, &out)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("disable joan = %d %v", resp.StatusCode, out)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"chat-telegram-joan"`) || strings.Contains(string(raw), "chat-telegram-joan-2") {
		t.Errorf("removing joan's bot would delete %s", raw)
	}
}

// A household rule's chat.send is gated on the platform being installed,
// and an instanced platform is installed when any of its bots is.
func TestAPlatformIsInstalledWhenABotIs(t *testing.T) {
	h, _ := connectHarness(t)
	if on, _ := h.s.CapabilityEnabled("chat-telegram"); on {
		t.Fatal("telegram counts as installed before any bot exists")
	}
	if code, out := h.connect(t, telegramFor("joan", "sandy")); code != http.StatusCreated {
		t.Fatalf("connect = %d %v", code, out)
	}
	if on, err := h.s.CapabilityEnabled("chat-telegram"); !on || err != nil {
		t.Errorf("telegram with a bot installed = %v %v", on, err)
	}
	if on, _ := h.s.CapabilityEnabled("chat-slack"); on {
		t.Error("slack counts as installed because telegram is")
	}
}
