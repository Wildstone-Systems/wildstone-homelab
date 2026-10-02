package api

// Epic #326 S06 through the public API: one Person-scoped surface for
// the Console, CLI, Homepage and chat; nobody but the owning Person
// lists, reads or acts on a candidate; no Agent credential acts at all,
// and none is handed a token or the excerpt.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/mailreview"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// memCalendar is an exact calendar in memory: sam may write sam-family.
type memCalendar struct {
	mu     sync.Mutex
	events map[string]caldav.EventWrite
	writes int
}

func (m *memCalendar) allowed(member, id string, write bool) error {
	if member == "sam" && id == "sam-family" {
		return nil
	}
	return fmt.Errorf("%w: %s on %s", domestic.ErrCalendarAccess, member, id)
}

func (m *memCalendar) CheckWritable(_ context.Context, member, id string) error {
	return m.allowed(member, id, true)
}

func (m *memCalendar) Availability(_ context.Context, member, id string, _, _ time.Time) ([]caldav.Event, string, error) {
	return nil, id, m.allowed(member, id, false)
}

func (m *memCalendar) EventByUID(_ context.Context, member, id, uid string) (*caldav.Event, string, error) {
	if err := m.allowed(member, id, false); err != nil {
		return nil, "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.events[uid]; !ok {
		return nil, "", nil
	}
	return &caldav.Event{UID: uid}, `"1"`, nil
}

func (m *memCalendar) CreateExact(_ context.Context, member, id string, w caldav.EventWrite) (string, string, error) {
	if err := m.allowed(member, id, true); err != nil {
		return "", "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.events[w.UID]; ok {
		return "", "", caldav.ErrPreconditionFailed
	}
	m.events[w.UID] = w
	m.writes++
	return "/cal/" + id + "/" + w.UID + ".ics", `"1"`, nil
}

func (m *memCalendar) UpdateExact(context.Context, string, string, caldav.EventWrite) (string, string, error) {
	return "", "", errors.New("not used")
}

func (m *memCalendar) DeleteExact(context.Context, string, string, string) error {
	return errors.New("not used")
}

type mailAPIHarness struct {
	*credentialHarness
	cal    *memCalendar
	engine *mailreview.Engine
	vault  *runtimedb.MailExcerptVault
	sub    int64
}

func newMailAPIHarness(t *testing.T) *mailAPIHarness {
	t.Helper()
	c := newCredentialHarness(t)
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword}, nil); code != http.StatusOK {
		t.Fatalf("intake: %d %s", code, body)
	}
	req := map[string]any{"agent": c.agent, "targetCalendar": "sam-family",
		"scope": map[string]any{"folders": []string{"INBOX"}, "conflictCalendars": []string{"household"}}}
	var preview struct {
		SubscriptionMatch map[string]string `json:"subscriptionMatch"`
	}
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/authorization", req, &preview); code != http.StatusOK {
		t.Fatalf("preview = %d %s", code, body)
	}
	match, _ := json.Marshal(preview.SubscriptionMatch)
	sub, err := c.s.DB.ProposeAgentSubscription(runtimedb.AgentSubscriptionSpec{Butler: c.agent,
		TriggerKind: runtimedb.SubscriptionTriggerSchedule, MatchKind: "mail.changes", MatchJSON: string(match),
		Audience: runtimedb.AudienceOwner, Capabilities: []string{"mail.changes"}, SourceConnection: "sam-mail",
		Cadence: 15 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	req["subscription"] = sub
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/authorization", req, nil); code != http.StatusOK {
		t.Fatalf("authorize = %d %s", code, body)
	}
	if err := c.s.DB.ApproveAgentSubscription(sub, "mika"); err != nil {
		t.Fatal(err)
	}
	if a, err := c.s.DB.LiveBackgroundAuthorization("sam-mail"); err != nil || strings.Join(a.Scope.ConflictCalendars, ",") != "household" {
		t.Fatalf("the conflict calendars were not authorized: %+v %v", a.Scope, err)
	}
	vault, err := c.s.DB.OpenMailExcerptVault(filepath.Join(t.TempDir(), "mail-excerpts.key"))
	if err != nil {
		t.Fatal(err)
	}
	cal := &memCalendar{events: map[string]caldav.EventWrite{}}
	engine := &mailreview.Engine{DB: c.s.DB, Vault: vault, Calendar: cal}
	c.s.MailReview = engine
	return &mailAPIHarness{credentialHarness: c, cal: cal, engine: engine, vault: vault, sub: sub}
}

// candidate records a school-night candidate for sam and offers it.
func (m *mailAPIHarness) candidate(t *testing.T, fp string, start time.Time) string {
	t.Helper()
	sam, _ := m.s.DB.GetButlerMember("sam")
	conn, _ := m.s.DB.GetConnection("sam-mail")
	res, err := m.s.DB.RecordMailMessage(m.vault, runtimedb.MailMessageRecord{
		Connection: "sam-mail", Epoch: conn.LeaseEpoch, PersonID: sam.PersonID, Fingerprint: fp, SourceRef: "ref-" + fp,
		TargetCalendar: "sam-family", At: time.Now(), Source: runtimedb.MailSourceModel,
		Drafts: []runtimedb.MailCandidateDraft{{Source: runtimedb.MailSourceModel, MessageKey: fp, SenderKey: "s1",
			Excerpt: "Family night is Thursday at six.",
			Fields: runtimedb.MailCandidateFields{Title: "School family night " + fp, Start: start, End: start.Add(90 * time.Minute),
				TZ: "UTC", Confidence: 0.9, Missing: []string{}, Ambiguous: []string{}}}},
	})
	if err != nil || len(res.Changes) != 1 {
		t.Fatalf("record: %+v %v", res, err)
	}
	id := res.Changes[0].Candidate.ID
	if _, err := m.engine.Offer(context.Background(), id); err != nil {
		t.Fatalf("offer: %v", err)
	}
	return id
}

func TestMailCandidateAPIIsPersonScopedAndAgentsNeverAct(t *testing.T) {
	m := newMailAPIHarness(t)
	start := time.Now().Add(8 * 24 * time.Hour).Truncate(time.Minute)
	id := m.candidate(t, "mfp1_a", start)
	path := "/api/v1/home/mail/candidates/" + id

	// sam sees it, with its review, tokens and excerpt.
	var list struct {
		Candidates []struct {
			ID     string                `json:"id"`
			Review *runtimedb.MailReview `json:"review"`
		} `json:"candidates"`
	}
	if code, body := m.homeCall("sam", http.MethodGet, "/api/v1/home/mail/candidates", nil, &list); code != http.StatusOK ||
		len(list.Candidates) != 1 || list.Candidates[0].Review == nil || len(list.Candidates[0].Review.Actions) != 3 {
		t.Fatalf("sam's list = %d %s", code, body)
	}
	var got struct {
		Review  runtimedb.MailReview `json:"review"`
		Tokens  map[string]string    `json:"tokens"`
		Excerpt string               `json:"excerpt"`
	}
	if code, body := m.homeCall("sam", http.MethodGet, path, nil, &got); code != http.StatusOK || got.Tokens["add"] == "" ||
		got.Excerpt == "" || got.Review.Revision != 1 {
		t.Fatalf("sam's candidate = %d %s", code, body)
	}

	// bo and the Owner see nothing, and cannot act.
	for _, who := range []string{"bo"} {
		if code, body := m.homeCall(who, http.MethodGet, "/api/v1/home/mail/candidates", nil, &list); code != http.StatusOK || len(list.Candidates) != 0 {
			t.Fatalf("%s's list = %d %s", who, code, body)
		}
		if code, _ := m.homeCall(who, http.MethodGet, path, nil, nil); code != http.StatusNotFound {
			t.Fatalf("%s read sam's candidate: %d", who, code)
		}
		if code, body := m.homeCall(who, http.MethodPost, path+"/act", map[string]any{"token": got.Tokens["add"]}, nil); code != http.StatusNotFound {
			t.Fatalf("%s acted with sam's token: %d %s", who, code, body)
		}
	}
	if code, _ := m.consoleCall(m.mika, http.MethodGet, "/api/v1/mail/candidates/"+id, nil, nil); code != http.StatusNotFound {
		t.Fatalf("the Owner read sam's candidate: %d", code)
	}
	if code, _ := m.consoleCall(m.mika, http.MethodPost, "/api/v1/mail/act", map[string]any{"token": got.Tokens["add"]}, nil); code != http.StatusNotFound {
		t.Fatalf("the Owner acted with sam's token: %d", code)
	}

	// An Agent acting for sam may read her candidate, gets no token and
	// no excerpt, and cannot act — by token or by revision.
	agent := m.console(runtimedb.RoleButler, "sam")
	var asAgent map[string]json.RawMessage
	if code, body := m.consoleCall(agent, http.MethodGet, "/api/v1/mail/candidates/"+id, nil, &asAgent); code != http.StatusOK ||
		asAgent["tokens"] != nil || asAgent["excerpt"] != nil || strings.Contains(body, "mrt_") {
		t.Fatalf("an Agent reading for sam = %d %s", code, body)
	}
	for _, body := range []map[string]any{{"token": got.Tokens["add"]}, {"revision": 1, "action": "add"}} {
		if code, _ := m.consoleCall(agent, http.MethodPost, "/api/v1/mail/candidates/"+id+"/act", body, nil); code != http.StatusForbidden {
			t.Fatalf("an Agent acted for sam: %d", code)
		}
	}
	if m.cal.writes != 0 {
		t.Fatal("something was written before sam chose")
	}

	// sam adds it; the replay is refused.
	var res mailreview.Result
	if code, body := m.homeCall("sam", http.MethodPost, path+"/act", map[string]any{"token": got.Tokens["add"]}, &res); code != http.StatusOK ||
		res.Outcome != mailreview.OutcomeAdded || res.Candidate.State != runtimedb.MailCandidateCreated {
		t.Fatalf("sam's add = %d %s", code, body)
	}
	if code, _ := m.homeCall("sam", http.MethodPost, path+"/act", map[string]any{"token": got.Tokens["add"]}, nil); code != http.StatusConflict {
		t.Fatalf("replay = %d", code)
	}
	if code, _ := m.homeCall("sam", http.MethodPost, path+"/act", map[string]any{"revision": 1, "action": "add"}, nil); code != http.StatusConflict {
		t.Fatalf("duplicate click = %d", code)
	}
	if m.cal.writes != 1 {
		t.Fatalf("%d writes", m.cal.writes)
	}
	var after struct {
		Effects []runtimedb.MailEffect `json:"effects"`
		Excerpt string                 `json:"excerpt"`
	}
	if code, body := m.homeCall("sam", http.MethodGet, path, nil, &after); code != http.StatusOK || len(after.Effects) != 1 ||
		after.Effects[0].State != runtimedb.MailEffectConfirmed || after.Excerpt != "" {
		t.Fatalf("after add = %d %s", code, body)
	}
}

func TestMailChatCommandIsExactAndDirectOnly(t *testing.T) {
	m := newMailAPIHarness(t)
	id := m.candidate(t, "mfp1_b", time.Now().Add(8*24*time.Hour).Truncate(time.Minute))
	rv, err := m.s.DB.MailReviewFor("sam", id)
	if err != nil {
		t.Fatal(err)
	}
	// sam has no chat binding in this harness, so the review went to
	// her in-app notice only, and no chat reply is its route.
	if rv.Route != runtimedb.MailRouteNotice {
		t.Fatalf("route %s", rv.Route)
	}
	for _, tc := range []struct {
		ctx  chatCommandCtx
		text string
	}{
		{chatCommandCtx{Identity: "sam", Binding: runtimedb.ChatBinding{ID: 7}}, "/mail add " + id + " 1"},
		{chatCommandCtx{Identity: "sam", Group: true, Binding: runtimedb.ChatBinding{ID: 7}}, "/mail add " + id + " 1"},
		{chatCommandCtx{Identity: "bo", Binding: runtimedb.ChatBinding{ID: 8}}, "/mail add " + id + " 1"},
		{chatCommandCtx{Identity: "sam", Binding: runtimedb.ChatBinding{ID: 7}}, "/mail add " + id},
		{chatCommandCtx{Identity: "sam", Binding: runtimedb.ChatBinding{ID: 7}}, "/mail yes"},
	} {
		tc.ctx.At = runtimedb.Connection{Surface: runtimedb.ButlerSurfaceFamily}
		body, _, handled := m.s.runChatCommand(tc.ctx, tc.text)
		if !handled || m.cal.writes != 0 {
			t.Fatalf("%q as %s: handled %v, writes %d (%s)", tc.text, tc.ctx.Identity, handled, m.cal.writes, body)
		}
	}
	// The web chat on sam's own authenticated session is her.
	body, _, _ := m.s.runChatCommand(chatCommandCtx{Identity: "sam", Web: &webCommandEffect{},
		At: runtimedb.Connection{Surface: runtimedb.ButlerSurfaceFamily}}, "/mail add "+id+" 1")
	if m.cal.writes != 1 || !strings.Contains(body, "Added") {
		t.Fatalf("sam's own web chat: %s (writes %d)", body, m.cal.writes)
	}
}

func TestHomeSubscriptionControlIsThePersonsOwn(t *testing.T) {
	m := newMailAPIHarness(t)
	path := fmt.Sprintf("/api/v1/home/agent-subscriptions/%d/control", m.sub)
	if code, body := m.homeCall("bo", http.MethodPost, path, map[string]any{"action": "pause"}, nil); code != http.StatusNotFound {
		t.Fatalf("bo paused sam's monitor: %d %s", code, body)
	}
	for _, step := range []map[string]any{{"action": "pause", "reason": "away"}, {"action": "resume"},
		{"action": "cadence", "cadence": "30m"}, {"action": "disable"}} {
		if code, body := m.homeCall("sam", http.MethodPost, path, step, nil); code != http.StatusOK {
			t.Fatalf("sam %v: %d %s", step, code, body)
		}
	}
	if code, _ := m.homeCall("sam", http.MethodPost, path, map[string]any{"action": "enable"}, nil); code != http.StatusForbidden {
		t.Fatalf("a Person re-enabled from the Homepage: %d", code)
	}
	sub, err := m.s.DB.GetAgentSubscription(m.sub)
	if err != nil || sub.Enabled || sub.Cadence != 30*time.Minute {
		t.Fatalf("subscription after sam's controls: %+v %v", sub, err)
	}
}
