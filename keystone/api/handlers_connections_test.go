package api

// Chat Connections in one step (ADR-W033 B7.1).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/internal/fakesecret"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Fake credentials with each platform's real prefix, built by
// fakesecret so no token-shaped literal sits in the source.
var (
	telegramToken = "123456:" + fakesecret.Token()
	slackBot      = "xoxb-" + fakesecret.Token()
	slackApp      = "xapp-" + fakesecret.Token()
)

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

// A second person for the same Agent goes onto the bot it already has:
// no token, no new bot. One bot serves many people.
func TestASecondPersonJoinsTheExistingBot(t *testing.T) {
	h, _ := connectHarness(t)
	if code, out := h.connect(t, telegramFor("joan", "sandy")); code != http.StatusCreated {
		t.Fatalf("first = %d %v", code, out)
	}
	code, out := h.connect(t, map[string]any{"platform": "telegram", "butler": "joan", "person": "mika"})
	if code != http.StatusCreated || out["capability"] != "chat-telegram-joan" || out["reused"] != true {
		t.Fatalf("second person = %d %v", code, out)
	}
	current, _ := h.s.State.Load()
	for _, r := range current {
		if r.Kind == resource.KindContainer && strings.HasPrefix(r.Metadata.Name, "chat-telegram-joan-") {
			t.Errorf("inviting a second person made another bot: %s", r.Metadata.Name)
		}
	}
	if invites, _ := h.s.DB.OpenInvitesFor("mika"); len(invites) != 1 || invites[0].Capability != "chat-telegram-joan" {
		t.Errorf("mika's invites = %+v", invites)
	}
}

// One token is one bot. Two installs polling it would each steal the
// other's messages, so the second is refused; a different token makes a
// genuinely separate bot.
func TestOneTokenMakesOneBot(t *testing.T) {
	h, _ := connectHarness(t)
	if code, out := h.connect(t, telegramFor("joan", "sandy")); code != http.StatusCreated {
		t.Fatalf("first = %d %v", code, out)
	}
	code, out := h.connect(t, telegramFor("joan", "mika"))
	if code != http.StatusConflict || !strings.Contains(out["error"].(string), "leave the token empty") {
		t.Fatalf("the same token again = %d %v", code, out)
	}
	if strings.Contains(out["error"].(string), telegramToken) {
		t.Error("the refusal repeated the token")
	}
	other := map[string]any{"platform": "telegram", "butler": "joan", "person": "mika",
		"credentials": map[string]string{"botToken": "654321:" + fakesecret.Token()}}
	if code, out := h.connect(t, other); code != http.StatusCreated || out["capability"] != "chat-telegram-joan-2" {
		t.Fatalf("a second token = %d %v", code, out)
	}
}

// Removing a bot uninstalls it and ends everything that pointed at it:
// the Agent's address, the bot's own credential, and open invites.
func TestRemovingABotEndsItsConnection(t *testing.T) {
	h, _ := connectHarness(t)
	if code, out := h.connect(t, telegramFor("joan", "sandy")); code != http.StatusCreated {
		t.Fatalf("connect = %d %v", code, out)
	}
	var out map[string]any
	resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/connections/chat-telegram-joan/remove", nil, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove = %d %v", resp.StatusCode, out)
	}
	if _, err := h.s.DB.ChatConnectionAt("telegram", "chat-telegram-joan"); err == nil {
		t.Error("the Agent still answers at the removed bot")
	}
	if p, err := h.s.DB.GetPrincipal("chat-chat-telegram-joan"); err != nil || !p.Disabled {
		t.Errorf("the bot's credential = %+v %v, want disabled", p, err)
	}
	if invites, _ := h.s.DB.OpenInvitesFor("sandy"); len(invites) != 0 {
		t.Errorf("invites to a removed bot are still open: %+v", invites)
	}
	current, _ := h.s.State.Load()
	for _, r := range current {
		if r.Metadata.Owner == "capability/chat-telegram-joan" && r.Kind == resource.KindContainer {
			t.Error("the bot's container is still desired")
		}
	}
	// Its token is free again, for a new bot under a fresh name: the
	// removed one's credential stays disabled rather than revived.
	if code, out := h.connect(t, telegramFor("joan", "sandy")); code != http.StatusCreated || out["capability"] != "chat-telegram-joan-2" {
		t.Errorf("reusing a removed bot's token = %d %v", code, out)
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
		{"no token and no bot yet", map[string]any{"platform": "telegram", "butler": "joan", "person": "sandy"}, "has no Telegram bot yet"},
		{"a token with spaces", map[string]any{"platform": "telegram", "butler": "joan", "person": "sandy",
			"credentials": map[string]string{"botToken": "not a token at all"}}, "one unbroken string"},
		{"slack without its app token", map[string]any{"platform": "slack", "butler": "joan", "person": "sandy",
			"credentials": map[string]string{"botToken": slackBot}}, "app-level token"},
		{"slack tokens swapped", map[string]any{"platform": "slack", "butler": "joan", "person": "sandy",
			"credentials": map[string]string{"botToken": slackApp, "appToken": slackBot}}, "starts with xoxb-"},
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
		if strings.Contains(msg, slackBot) || strings.Contains(msg, "not a token at all") {
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
		"slack":    {"botToken": slackBot, "appToken": slackApp},
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
	for i, who := range []string{"sandy", "mika"} {
		body := telegramFor("joan", who)
		body["credentials"] = map[string]string{"botToken": fmt.Sprintf("%d:", 100000+i) + fakesecret.Token()}
		if code, out := h.connect(t, body); code != http.StatusCreated {
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

// A stock install passes no -hosts, so Keystone advertises no address of
// its own. The Console route's upstream — where the proxy's container
// reaches the API — is where a bot's container reaches it too, and a
// connection must not fail for want of it.
func TestABotReachesKeystoneWhereTheProxyDoes(t *testing.T) {
	h, _ := connectHarness(t)
	h.s.APIURL = ""
	if code, out := h.connect(t, telegramFor("joan", "sandy")); code != http.StatusBadRequest {
		t.Fatalf("with no address at all = %d %v, want a refusal that says so", code, out)
	}
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindRoute,
		Metadata: resource.Metadata{Name: "wildstone-console", Owner: "capability/console", Node: "n1"},
		Route:    &resource.RouteSpec{Host: "console.home.test", Upstream: "100.70.0.9:9441", Auth: "authelia_oidc", Classification: "admin_tailscale"},
	}); err != nil {
		t.Fatal(err)
	}
	if code, out := h.connect(t, telegramFor("joan", "sandy")); code != http.StatusCreated {
		t.Fatalf("connect = %d %v", code, out)
	}
	current, _ := h.s.State.Load()
	for _, r := range current {
		if r.Kind == resource.KindContainer && r.Metadata.Name == "chat-telegram-joan" {
			if got := r.Container.Env["KEYSTONE_API"]; got != "http://100.70.0.9:9441" {
				t.Errorf("the bot reaches Keystone at %q", got)
			}
			return
		}
	}
	t.Fatal("no bot container")
}

// Making a bot mints a credential and removing one ends people's way to
// an Agent, so both ask for a fresh sign-in (A4.1). Inviting somebody
// to a bot that exists is admission, which is ordinary.
func TestOnlyMakingOrRemovingABotAsksForAFreshSignIn(t *testing.T) {
	h, _ := connectHarness(t)
	stale := sessionCookie(h.s.StartConsoleSession("mika", time.Now().Add(-time.Hour)))
	body := func(m map[string]any) string { b, _ := json.Marshal(m); return string(b) }

	code, out := h.sessionRequest(http.MethodPost, "/api/v1/connections", body(telegramFor("joan", "sandy")), "mika", stale)
	if code != http.StatusForbidden || out["stepUp"] == nil {
		t.Fatalf("a new bot on a stale sign-in = %d %v, want a step-up", code, out)
	}
	// Made on a fresh sign-in, then invited to on the stale one.
	fresh := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	if code, out := h.sessionRequest(http.MethodPost, "/api/v1/connections", body(telegramFor("joan", "sandy")), "mika", fresh); code != http.StatusCreated {
		t.Fatalf("a new bot on a fresh sign-in = %d %v", code, out)
	}
	code, out = h.sessionRequest(http.MethodPost, "/api/v1/connections",
		body(map[string]any{"platform": "telegram", "butler": "joan", "person": "mika"}), "mika", stale)
	if code != http.StatusCreated || out["stepUp"] != nil {
		t.Errorf("inviting to an existing bot on a stale sign-in = %d %v, want no challenge", code, out)
	}
	code, out = h.sessionRequest(http.MethodPost, "/api/v1/connections/chat-telegram-joan/remove", "", "mika", stale)
	if code != http.StatusForbidden || out["stepUp"] == nil {
		t.Errorf("removing a bot on a stale sign-in = %d %v, want a step-up", code, out)
	}
}
