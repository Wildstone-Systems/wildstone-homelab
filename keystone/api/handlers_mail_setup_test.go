package api

// Epic #326 S07: the Watch-this-mailbox setup read model shows a caller
// their OWN monitor and nobody else's, resumes a setup from server
// state at every step, and filing the subscription goes through an
// Owner's approval and never substitutes for the Person's authorization.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/sharedcalendar"
)

// mailCalendarsForTest declares the calendars the setup offers: the
// household's, one only sam may write, one only olive may read.
func mailCalendarsForTest(t *testing.T, c *credentialHarness) {
	t.Helper()
	reg := `{"calendars":[
	  {"id":"household","name":"Household","color":"#3b82f6","default":true,"readGroups":["homepage_users"],"writeGroups":["homepage_users"]},
	  {"id":"sam-family","name":"Sam's family","color":"#22c55e","readGroups":[],"writeGroups":[],"readMembers":["sam"],"writeMembers":["sam"]},
	  {"id":"olive-work","name":"Olive's work","color":"#ef4444","readGroups":[],"writeGroups":[],"readMembers":["olive"]}
	]}`
	if err := c.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindConfigFile,
		Metadata:   resource.Metadata{Name: sharedcalendar.ConfigName, Owner: "capability/calendar", Node: "n1"},
		ConfigFile: &resource.ConfigFileSpec{Filename: "calendars.json", Content: reg},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.s.State.Commit("test", "calendars", "calendar registry"); err != nil {
		t.Fatal(err)
	}
}

type setupForTest struct {
	Member   string `json:"member"`
	Provider *struct {
		Package  string            `json:"package"`
		Settings map[string]string `json:"settings"`
	} `json:"provider"`
	Monitors []struct {
		Connection    runtimedb.IntegrationConnection    `json:"connection"`
		Credential    *runtimedb.PersonalCredential      `json:"credential"`
		Authorization *runtimedb.BackgroundAuthorization `json:"authorization"`
		Subscriptions []subscriptionView                 `json:"subscriptions"`
		PendingPlans  []mailPendingPlan                  `json:"pendingPlans"`
	} `json:"monitors"`
	Agents    []mailAgent    `json:"agents"`
	Calendars []mailCalendar `json:"calendars"`
}

func (c *credentialHarness) homeSetup(t *testing.T, user string) setupForTest {
	t.Helper()
	var out setupForTest
	if code, body := c.homeCall(user, http.MethodGet, "/api/v1/home/mail/setup", nil, &out); code != http.StatusOK {
		t.Fatalf("%s setup = %d %s", user, code, body)
	}
	return out
}

func samWatch(c *credentialHarness) map[string]any {
	return map[string]any{
		"agent": c.agent, "targetCalendar": "sam-family", "processingLocation": "local", "cadence": "30m",
		"scope":             map[string]any{"folders": []string{"INBOX", "School"}, "senders": []string{"school.example.internal"}},
		"conflictCalendars": []string{"household", "sam-family"},
	}
}

func TestMailSetupShowsOnlyYourOwnMonitor(t *testing.T) {
	c := newCredentialHarness(t)
	installMailForTest(t, c)
	mailCalendarsForTest(t, c)

	sam := c.homeSetup(t, "sam")
	if sam.Provider == nil || sam.Provider.Package != "mail-roundcube" ||
		sam.Provider.Settings["imap_host"] != "imap.example.internal" || sam.Provider.Settings["imap_port"] != "993" {
		t.Fatalf("provider = %+v", sam.Provider)
	}
	if _, leaked := sam.Provider.Settings["smtp_host"]; leaked {
		t.Fatal("the setup shows params the adapter does not inherit")
	}
	if len(sam.Monitors) != 1 || sam.Monitors[0].Connection.Name != "sam-mail" {
		t.Fatalf("sam sees monitors %+v", sam.Monitors)
	}
	ids := map[string]bool{}
	for _, cal := range sam.Calendars {
		ids[cal.ID] = true
	}
	if !ids["household"] || !ids["sam-family"] || ids["olive-work"] {
		t.Fatalf("sam is offered calendars %v", ids)
	}
	if len(sam.Agents) != 1 || sam.Agents[0].Seq != c.agent {
		t.Fatalf("agents = %+v", sam.Agents)
	}

	// bo has no monitor and sees none of sam's.
	if bo := c.homeSetup(t, "bo"); len(bo.Monitors) != 0 {
		t.Fatalf("bo sees %+v", bo.Monitors)
	}
	// An Owner on the Console sees their own setup, not the household's.
	var olive setupForTest
	if code, body := c.consoleCall(c.olive, http.MethodGet, "/api/v1/mail/setup", nil, &olive); code != http.StatusOK {
		t.Fatalf("olive setup = %d %s", code, body)
	}
	if len(olive.Monitors) != 1 || olive.Monitors[0].Connection.Name != "olive-mail" {
		t.Fatalf("olive sees %+v", olive.Monitors)
	}
}

func TestWatchThisMailboxResumesFromServerState(t *testing.T) {
	c := newCredentialHarness(t)
	installMailForTest(t, c)
	mailCalendarsForTest(t, c)
	if code, body := c.consoleCall(c.mika, http.MethodPost, "/api/v1/integrations/sam-mail/adapter/render", nil, nil); code != http.StatusOK {
		t.Fatalf("render = %d %s", code, body)
	}
	// Before a credential, nothing can be filed.
	if code, _ := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/subscription", samWatch(c), nil); code == http.StatusOK {
		t.Fatal("a subscription was filed with no credential")
	}
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword}, nil); code != http.StatusOK {
		t.Fatalf("intake = %d %s", code, body)
	}
	if got := c.homeSetup(t, "sam").Monitors[0]; got.Credential == nil || got.Credential.Login != "sam@example.internal" {
		t.Fatalf("setup does not show the active credential: %+v", got.Credential)
	}

	// Nobody but the mailbox owner files its subscription.
	if code, _ := c.homeCall("bo", http.MethodPost, "/api/v1/home/integrations/sam-mail/subscription", samWatch(c), nil); code != http.StatusForbidden {
		t.Fatalf("bo filing for sam = %d, want 403", code)
	}
	if code, _ := c.consoleCall(c.mika, http.MethodPost, "/api/v1/integrations/sam-mail/subscription", samWatch(c), nil); code != http.StatusForbidden {
		t.Fatalf("an Owner filing for sam = %d, want 403", code)
	}
	// A calendar sam cannot read is refused for conflicts; one sam
	// cannot write is refused as the target.
	bad := samWatch(c)
	bad["conflictCalendars"] = []string{"olive-work"}
	if code, _ := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/subscription", bad, nil); code != http.StatusBadRequest {
		t.Fatalf("another Person's calendar for conflicts = %d, want 400", code)
	}
	bad = samWatch(c)
	bad["targetCalendar"] = "olive-work"
	if code, _ := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/subscription", bad, nil); code != http.StatusForbidden {
		t.Fatalf("an unwritable target = %d, want 403", code)
	}

	var filed struct {
		Plan              struct{ ID string } `json:"plan"`
		SubscriptionMatch map[string]string   `json:"subscriptionMatch"`
	}
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/subscription", samWatch(c), &filed); code != http.StatusOK {
		t.Fatalf("file = %d %s", code, body)
	}
	first := filed.Plan.ID
	// Filed again (the form was changed and sent again): the first
	// filing is superseded, so only one can ever be approved.
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/subscription", samWatch(c), &filed); code != http.StatusOK {
		t.Fatalf("refile = %d %s", code, body)
	}
	if rec, _ := c.s.DB.GetPlan(first); rec.State != runtimedb.PlanSuperseded {
		t.Fatalf("the first filing is %s, want superseded", rec.State)
	}

	// After a refresh the setup knows it waits for an Owner, and can
	// rebuild the form from the pending Plan.
	mon := c.homeSetup(t, "sam").Monitors[0]
	if len(mon.PendingPlans) != 1 || mon.PendingPlans[0].Kind != "subscription" || mon.PendingPlans[0].ID != filed.Plan.ID {
		t.Fatalf("pending = %+v", mon.PendingPlans)
	}
	var match map[string]any
	if err := json.Unmarshal(mon.PendingPlans[0].Match, &match); err != nil {
		t.Fatal(err)
	}
	if match["targetCalendar"] != "sam-family" || match["authorization"] != filed.SubscriptionMatch["authorization"] ||
		!strings.Contains(string(mon.PendingPlans[0].Match), "School") {
		t.Fatalf("pending match = %s", mon.PendingPlans[0].Match)
	}
	if mon.Authorization != nil {
		t.Fatal("filing the subscription authorized something")
	}

	// A Member cannot approve it; an Owner can, and that alone authorizes
	// nothing.
	if code, _ := c.homeCall("sam", http.MethodPost, "/api/v1/plans/"+filed.Plan.ID+"/approve", nil, nil); code == http.StatusOK {
		t.Fatal("a Member approved the subscription")
	}
	c.approve(t, filed.Plan.ID)
	mon = c.homeSetup(t, "sam").Monitors[0]
	if len(mon.Subscriptions) != 1 || !mon.Subscriptions[0].Enabled || mon.Subscriptions[0].AwaitingApproval ||
		mon.Subscriptions[0].Cadence != "30m0s" {
		t.Fatalf("subscriptions = %+v", mon.Subscriptions)
	}
	if mon.Authorization != nil || len(proposedRenders(t, c, "sam-mail")) != 0 {
		t.Fatal("an Owner's approval stood in for the Person's authorization")
	}

	// The Person authorizes the approved subscription with the same
	// choices; the render Plan is then pending, and the setup says so.
	req := samWatch(c)
	delete(req, "cadence")
	delete(req, "conflictCalendars")
	req["subscription"] = mon.Subscriptions[0].ID
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/authorization", req, nil); code != http.StatusOK {
		t.Fatalf("authorize = %d %s", code, body)
	}
	mon = c.homeSetup(t, "sam").Monitors[0]
	if mon.Authorization == nil || mon.Authorization.Subscription != mon.Subscriptions[0].ID {
		t.Fatalf("authorization = %+v", mon.Authorization)
	}
	if len(mon.PendingPlans) != 1 || mon.PendingPlans[0].Kind != "render" {
		t.Fatalf("pending after authorizing = %+v", mon.PendingPlans)
	}

	// An edit revises the same subscription and waits for re-approval.
	edit := samWatch(c)
	edit["scope"] = map[string]any{"folders": []string{"INBOX"}}
	var revised struct {
		Subscription int64 `json:"subscription"`
	}
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/subscription", edit, &revised); code != http.StatusOK {
		t.Fatalf("edit = %d %s", code, body)
	}
	mon = c.homeSetup(t, "sam").Monitors[0]
	if revised.Subscription != mon.Subscriptions[0].ID || len(mon.Subscriptions) != 1 ||
		!mon.Subscriptions[0].AwaitingApproval || mon.Subscriptions[0].Version != 2 {
		t.Fatalf("after the edit: %+v", mon.Subscriptions)
	}

	// Olive's setup never shows any of it.
	var olive setupForTest
	c.consoleCall(c.olive, http.MethodGet, "/api/v1/mail/setup", nil, &olive)
	for _, m := range olive.Monitors {
		if m.Connection.Name == "sam-mail" || len(m.Subscriptions) != 0 {
			t.Fatalf("olive sees %+v", m)
		}
	}
}
