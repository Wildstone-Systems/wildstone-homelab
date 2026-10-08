package mailreview

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav/caldavfake"
	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// now is a week before the school-night scenario (Thursday 15 October
// 2026, 18:00–19:30 in New York).
var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// migrated is the schema, built once for this whole test binary rather
// than replayed into every database these tests open (#231). Each test
// still gets its own file.
var migrated = dbtemplate.New(func(path string) error {
	db, err := runtimedb.Open(path)
	if err != nil {
		return err
	}
	return db.Close()
})

func newYork() *time.Location {
	l, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return l
}

// fakeDAV is the shared in-process CalDAV fixture.
type fakeDAV = caldavfake.Server

func newFakeDAV(t *testing.T) *fakeDAV {
	t.Helper()
	f := caldavfake.New()
	t.Cleanup(f.Close)
	return f
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type allowExecution struct{}

func (allowExecution) CheckHouseholdExecution() error { return nil }

// env: Owner mika approves; Members sam and bo. sam owns sam-mail and
// authorizes Agent hobson to watch it, target calendar sam-family,
// conflicts also checked on household and bo-private. sam can write
// sam-family and read household; she cannot read bo-private. sam has a
// one-to-one Telegram binding to hobson.
type env struct {
	db      *runtimedb.DB
	vault   *runtimedb.MailExcerptVault
	dav     *fakeDAV
	cal     *domestic.Calendar
	eng     *Engine
	clock   *clock
	sub     int64
	agent   int64
	binding runtimedb.ChatBinding

	accessMu sync.Mutex
	access   map[string]map[string]string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := runtimedb.Open(migrated.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.UpsertModel(runtimedb.Model{Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama",
		ProviderModel: "llama", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"))
	for _, m := range []struct{ name, role string }{{"mika", runtimedb.RoleOwner}, {"sam", runtimedb.RoleMember}, {"bo", runtimedb.RoleMember}} {
		must(db.RecordButlerMember(m.name, m.role, "", ""))
		must(db.AdmitButlerMember(m.name, "root-admin"))
	}
	agent, err := db.CreateButler(runtimedb.Butler{Name: "hobson", Directive: "watch the post",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "kitchen-box"}, "root-admin")
	must(err)
	must(db.GrantButlerAccess("hobson", "sam", "root-admin"))
	_, err = db.CreateConnection(runtimedb.IntegrationConnection{Name: "sam-mail", Adapter: "wild-mail-adapter",
		Ownership: runtimedb.OwnershipPersonal, Owner: "sam", CreatedBy: "sam"})
	must(err)
	must(db.AddPlacement("sam-mail", "n1", runtimedb.PlacementPrimary, 0, "root-admin"))
	must(db.MarkPlacementPrepared("sam-mail", "n1", true, true, true))
	_, _, err = db.PlaceConnection("sam-mail", time.Hour)
	must(err)
	must(db.SetConnectionExecutable("sam-mail", "mail-roundcube@1.1.0/wild-mail-adapter", "root-admin"))
	staged, err := db.StagePersonalCredential("sam-mail", "", "sam@example.internal", "sam", []string{"n1"})
	must(err)
	must(db.MarkCredentialCopy(staged.Seq, "n1", true, 1))
	_, _, err = db.ActivatePersonalCredential(staged.Seq, "sam")
	must(err)
	req := runtimedb.BackgroundAuthorizationRequest{Connection: "sam-mail",
		Scope:              runtimedb.BackgroundScope{Folders: []string{"INBOX"}, ConflictCalendars: []string{"household", "bo-private"}},
		ProcessingLocation: runtimedb.ProcessingLocal, Agent: agent, TargetCalendar: "sam-family"}
	preview, err := db.PrepareBackgroundAuthorization("sam", req)
	must(err)
	match, _ := json.Marshal(runtimedb.BackgroundSubscriptionMatch("sam-mail", preview.Digest))
	sub, err := db.ProposeAgentSubscription(runtimedb.AgentSubscriptionSpec{Butler: agent,
		TriggerKind: runtimedb.SubscriptionTriggerSchedule, MatchKind: "mail.changes", MatchJSON: string(match),
		Audience: runtimedb.AudienceOwner, Capabilities: []string{"mail.changes"}, SourceConnection: "sam-mail",
		Cadence: 15 * time.Minute})
	must(err)
	req.Subscription = sub
	_, err = db.AuthorizeBackgroundUseAsPerson("sam", req)
	must(err)
	must(db.ApproveAgentSubscription(sub, "mika"))
	_, err = db.AcquireCoordinator("mail-test")
	must(err)
	vault, err := db.OpenMailExcerptVault(filepath.Join(t.TempDir(), "mail-excerpts.key"))
	must(err)

	// sam's one-to-one Telegram conversation with hobson.
	_, err = db.AddButlerAddress("hobson", "telegram", "hobson-bot", "", "mika")
	must(err)
	at, err := db.ChatConnectionAt("telegram", "hobson-bot")
	must(err)
	must(db.InsertChatClaimCode("hash-sam", runtimedb.ChatClaimCode{Connection: at.Seq, Identity: "sam",
		CreatedBy: "root-admin", ExpiresAt: time.Now().Add(time.Hour)}))
	binding, err := db.RedeemChatClaimCode("hash-sam", at, "tg-sam", "sam", "dm-sam")
	must(err)

	e := &env{db: db, vault: vault, dav: newFakeDAV(t), clock: &clock{t: now}, sub: sub, agent: agent, binding: binding,
		access: map[string]map[string]string{
			"sam":   {"sam-family": "rw", "household": "r"},
			"bo":    {"household": "rw", "bo-private": "rw"},
			"mika": {"household": "rw"},
		}}
	e.cal = &domestic.Calendar{Authority: allowExecution{}, Location: newYork(), Now: e.clock.Now,
		Resolve: func(member, id string, write bool) (*caldav.Upstream, string, error) {
			e.accessMu.Lock()
			grant := e.access[member][id]
			e.accessMu.Unlock()
			if grant == "" || (write && grant != "rw") {
				return nil, "", fmt.Errorf("%s may not %s calendar %s", member, map[bool]string{true: "write", false: "read"}[write], id)
			}
			return &caldav.Upstream{CollectionURL: e.dav.URL + "/cal/" + id + "/", Writable: true, Location: newYork(),
				Now: e.clock.Now, HTTP: &http.Client{Timeout: 300 * time.Millisecond}}, id, nil
		}}
	e.eng = &Engine{DB: db, Vault: vault, Calendar: e.cal, Now: e.clock.Now}
	return e
}

func (e *env) setAccess(member, calendar, grant string) {
	e.accessMu.Lock()
	defer e.accessMu.Unlock()
	if grant == "" {
		delete(e.access[member], calendar)
		return
	}
	e.access[member][calendar] = grant
}

func schoolNight() runtimedb.MailCandidateFields {
	ny := newYork()
	return runtimedb.MailCandidateFields{
		Title: "School family night", Location: "School gym", TZ: "America/New_York",
		Start: time.Date(2026, 10, 15, 18, 0, 0, 0, ny), End: time.Date(2026, 10, 15, 19, 30, 0, 0, ny),
		Confidence: 0.9, Missing: []string{}, Ambiguous: []string{},
	}
}

// record files one message's candidate the way the monitor does and
// hands its changes to the engine, as the monitor's hook would.
func (e *env) record(t *testing.T, fp string, d runtimedb.MailCandidateDraft) runtimedb.MailCandidate {
	t.Helper()
	sam, err := e.db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := e.db.GetConnection("sam-mail")
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.db.RecordMailMessage(e.vault, runtimedb.MailMessageRecord{
		Connection: "sam-mail", Epoch: conn.LeaseEpoch, PersonID: sam.PersonID, Fingerprint: fp, SourceRef: "ref-" + fp,
		TargetCalendar: "sam-family", State: runtimedb.MailMessageNoEvent, Drafts: []runtimedb.MailCandidateDraft{d},
		At: e.clock.Now(), Source: runtimedb.MailSourceModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out runtimedb.MailCandidate
	for _, ch := range res.Changes {
		if err := e.eng.CandidateChanged(context.Background(), ch); err != nil {
			t.Fatalf("hook(%s): %v", ch.Kind, err)
		}
		if ch.Kind == runtimedb.MailChangeNew {
			out = ch.Candidate
		}
	}
	if out.ID == "" {
		t.Fatalf("no new candidate from %s: %+v", fp, res)
	}
	return out
}

func draft(f runtimedb.MailCandidateFields) runtimedb.MailCandidateDraft {
	return runtimedb.MailCandidateDraft{Fields: f, Source: runtimedb.MailSourceModel, MessageKey: "m1", SenderKey: "s1",
		SubjectKey: "subj", Excerpt: "The school family night is Thursday, October 15, 2026 from 6:00 to 7:30 p.m."}
}

// tokens returns sam's live tokens for a candidate.
func (e *env) tokens(t *testing.T, id string) (runtimedb.MailReview, map[string]string) {
	t.Helper()
	r, tok, err := e.db.MailReviewTokens(e.vault, "sam", id, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return r, tok
}

func (e *env) act(req ActRequest) (Result, error) {
	if req.Via == "" {
		req.Via = "console"
	}
	return e.eng.Act(context.Background(), req)
}

func (e *env) candidate(t *testing.T, id string) runtimedb.MailCandidate {
	t.Helper()
	c, err := e.db.MailCandidateInternal(id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
