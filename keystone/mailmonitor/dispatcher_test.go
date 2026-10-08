package mailmonitor

// The integrated path (S03 scheduler → mail.changes work → adapter
// answer → candidates → checkpoint), with the real S02 authority: the
// school-night message becomes one private candidate for sam, the
// cursor commits only after it is durable, a UIDVALIDITY reset replays
// nothing, the read's answer does not stay in the work queue, and a
// revocation mid-read records nothing.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/householdtz"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

type pipeline struct {
	db      *runtimedb.DB
	eng     *automation.SubscriptionEngine
	clock   *clock
	sub     int64
	collect []func()
	model   *fakeModel
	hook    *hookLog
}

// newPipeline: Owner mika approves, Member sam owns sam-mail and
// authorizes its unattended use by Agent hobson, local processing,
// target calendar sam-family.
func newPipeline(t *testing.T) *pipeline {
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
		Scope: runtimedb.BackgroundScope{Folders: []string{"INBOX"}}, ProcessingLocation: runtimedb.ProcessingLocal,
		Agent: agent, TargetCalendar: "sam-family"}
	preview, err := db.PrepareBackgroundAuthorization("sam", req)
	must(err)
	match, _ := json.Marshal(runtimedb.BackgroundSubscriptionMatch("sam-mail", preview.Digest))
	sub, err := db.ProposeAgentSubscription(runtimedb.AgentSubscriptionSpec{Butler: agent,
		TriggerKind: runtimedb.SubscriptionTriggerSchedule, MatchKind: MatchKind, MatchJSON: string(match),
		Audience: runtimedb.AudienceOwner, Capabilities: []string{"mail.changes"}, SourceConnection: "sam-mail",
		Cadence: 15 * time.Minute})
	must(err)
	req.Subscription = sub
	_, err = db.AuthorizeBackgroundUseAsPerson("sam", req)
	must(err)
	must(db.ApproveAgentSubscription(sub, "mika"))
	must(db.BindAdapterPrincipal("adapter-sam", "sam-mail", "root-admin"))
	_, err = db.AcquireCoordinator("mail-test")
	must(err)
	vault, err := db.OpenMailExcerptVault(filepath.Join(t.TempDir(), "mail-excerpts.key"))
	must(err)

	p := &pipeline{db: db, clock: &clock{t: now}, sub: sub, model: &fakeModel{replies: []string{schoolNightReply}}, hook: &hookLog{}}
	p.eng = &automation.SubscriptionEngine{DB: db, Now: p.clock.Now,
		Authority: automation.BackgroundAuthorityFunc(func(ctx context.Context, u automation.BackgroundUse) error {
			sub, err := db.GetAgentSubscription(u.Subscription)
			if err != nil {
				return err
			}
			_, err = db.AuthorizeBackgroundUse(ctx, u.Connection, u.Subscription, sub.ApprovedVersion)
			return err
		})}
	ny, _ := time.LoadLocation("America/New_York")
	proc := &Processor{DB: db, Vault: vault, Zone: householdtz.New(ny), Hook: p.hook, Now: p.clock.Now,
		Extractor: &Extractor{Router: routes{list: []ModelRoute{local(p.model)}}}}
	p.eng.Dispatchers = map[string]automation.ScheduledDispatcher{MatchKind: NewDispatcher(Config{
		DB: db, Processor: proc, Runs: p.eng, Poll: time.Millisecond, Now: p.clock.Now,
		Go: func(f func()) { p.collect = append(p.collect, f) },
	})}
	return p
}

// tick runs the scheduler once and returns the queued read.
func (p *pipeline) tick(t *testing.T) runtimedb.IntegrationWork {
	t.Helper()
	out, err := p.eng.TickScheduled(context.Background())
	if err != nil || out.Dispatched != 1 {
		t.Fatalf("tick: %+v %v", out, err)
	}
	w, ok, err := p.db.ClaimIntegrationWork("adapter-sam")
	if err != nil || !ok || w.Capability != MatchKind {
		t.Fatalf("adapter claim: %+v %v %v", w, ok, err)
	}
	return w
}

// answer reports the adapter's mail.changes result and runs the
// collector.
func (p *pipeline) answer(t *testing.T, w runtimedb.IntegrationWork, resultType string, data any, errText string) {
	t.Helper()
	raw := "{}"
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(b)
	}
	if err := p.db.ReportIntegrationResult("adapter-sam", w.Seq, resultType, raw, time.Time{}, errText); err != nil {
		t.Fatal(err)
	}
	p.drain()
}

func (p *pipeline) drain() {
	fs := p.collect
	p.collect = nil
	for _, f := range fs {
		f()
	}
}

func cursor(validity, uid int) map[string]any {
	return map[string]any{"folders": []map[string]any{{"folder": "INBOX", "uidValidity": validity, "lastUid": uid}}}
}

func (p *pipeline) checkpoint(t *testing.T) string {
	t.Helper()
	cp, err := p.db.GetSubscriptionCheckpoint(p.sub, "sam-mail")
	if err != nil {
		t.Fatal(err)
	}
	return string(cp.Cursor)
}

func TestSchoolNightThroughTheScheduler(t *testing.T) {
	p := newPipeline(t)
	w := p.tick(t)
	var sent struct {
		Cursor json.RawMessage `json:"cursor"`
		Limit  int             `json:"limit"`
	}
	if err := json.Unmarshal([]byte(w.RequestJSON), &sent); err != nil || sent.Limit != BatchLimit || sent.Cursor != nil {
		t.Fatalf("first read request %s", w.RequestJSON)
	}
	msg := fixture(t, "plain.eml")
	p.answer(t, w, runtimedb.ResultOK, map[string]any{"outcome": "ok", "messages": []Message{msg}, "next": cursor(7, 42)}, "")

	list, err := p.db.ListMailCandidates("sam", runtimedb.MailCandidateFilter{})
	if err != nil || len(list) != 1 || list[0].State != runtimedb.MailCandidateProposed || list[0].TargetCalendar != "sam-family" {
		t.Fatalf("sam's candidates: %+v %v", list, err)
	}
	if other, _ := p.db.ListMailCandidates("bo", runtimedb.MailCandidateFilter{}); len(other) != 0 {
		t.Fatal("bo sees sam's candidate")
	}
	if got := p.checkpoint(t); got != `{"folders":[{"folder":"INBOX","lastUid":42,"uidValidity":7}]}` {
		t.Fatalf("checkpoint %s", got)
	}
	if done, _ := p.db.GetIntegrationWork(w.Seq); done.ResultJSON != "{}" {
		t.Fatal("the mail answer stayed in the work queue after processing")
	}
	runs, _ := p.db.ListSubscriptionRuns(p.sub, 5)
	if len(runs) != 1 || runs[0].State != runtimedb.SubscriptionRunSucceeded || runs[0].Outcome != runtimedb.RunOutcomeOK {
		t.Fatalf("run: %+v", runs)
	}

	// Next cadence: the adapter reports a UIDVALIDITY reset and hands the
	// same message back under new numbering. Nothing new is asked, the
	// model is not called again, and the reset cursor is committed.
	p.clock.t = p.clock.t.Add(15 * time.Minute)
	w = p.tick(t)
	if err := json.Unmarshal([]byte(w.RequestJSON), &sent); err != nil ||
		string(sent.Cursor) != `{"folders":[{"folder":"INBOX","lastUid":42,"uidValidity":7}]}` {
		t.Fatalf("second read did not resume from the committed cursor: %s", w.RequestJSON)
	}
	msg.Ref = "ref-after-reset"
	p.answer(t, w, runtimedb.ResultOK, map[string]any{"outcome": "cursor_reset", "messages": []Message{msg},
		"next": cursor(8, 3), "folders": []map[string]any{{"folder": "INBOX", "outcome": "cursor_reset"}}}, "")
	if list, _ := p.db.ListMailCandidates("sam", runtimedb.MailCandidateFilter{}); len(list) != 1 {
		t.Fatalf("a replay after cursor_reset produced %d candidates", len(list))
	}
	if p.model.calls() != 1 {
		t.Fatalf("model called %d times across a replay", p.model.calls())
	}
	if got := p.checkpoint(t); got != `{"folders":[{"folder":"INBOX","lastUid":3,"uidValidity":8}]}` {
		t.Fatalf("reset checkpoint %s", got)
	}
}

func TestAuthFailureCommitsNothingAndHolds(t *testing.T) {
	p := newPipeline(t)
	w := p.tick(t)
	p.answer(t, w, runtimedb.ResultUnavailable, nil, "adapter: the application is unavailable: auth_failed: login")
	if cp := p.checkpoint(t); cp != "" {
		t.Fatalf("an auth failure committed %s", cp)
	}
	s, _ := p.db.GetSubscriptionSchedule(p.sub)
	if s.Hold != runtimedb.SubscriptionHoldAuthFailed {
		t.Fatalf("hold = %q, want auth_failed", s.Hold)
	}
}

func TestModelOutageDoesNotAdvanceTheCursor(t *testing.T) {
	p := newPipeline(t)
	p.model.replies, p.model.err = nil, context.DeadlineExceeded
	w := p.tick(t)
	p.answer(t, w, runtimedb.ResultOK, map[string]any{"outcome": "ok", "messages": []Message{fixture(t, "plain.eml")},
		"next": cursor(7, 42)}, "")
	if cp := p.checkpoint(t); cp != "" {
		t.Fatalf("the cursor moved past an unread message: %s", cp)
	}
	runs, _ := p.db.ListSubscriptionRuns(p.sub, 5)
	if len(runs) != 1 || runs[0].Outcome != runtimedb.RunOutcomeError {
		t.Fatalf("run: %+v", runs)
	}
}

func TestRevocationMidReadRecordsNothing(t *testing.T) {
	p := newPipeline(t)
	w := p.tick(t)
	if err := p.db.RevokeBackgroundAuthorization("sam-mail", "sam", "stop"); err != nil {
		t.Fatal(err)
	}
	// The adapter's late answer is refused (the epoch moved); the
	// collector sees the read cancelled.
	if err := p.db.ReportIntegrationResult("adapter-sam", w.Seq, runtimedb.ResultOK, `{"outcome":"ok"}`, time.Time{}, ""); err == nil {
		t.Fatal("a fenced adapter's answer was accepted")
	}
	p.drain()
	if list, _ := p.db.ListMailCandidates("sam", runtimedb.MailCandidateFilter{}); len(list) != 0 {
		t.Fatal("a candidate was recorded after revocation")
	}
	if cp := p.checkpoint(t); cp != "" {
		t.Fatalf("a revoked run committed %s", cp)
	}
}

func TestClassifyUnavailable(t *testing.T) {
	for text, want := range map[string]string{
		"adapter: the application is unavailable: auth_failed":                   runtimedb.RunOutcomeAuthFailed,
		"adapter: the application is unavailable: bad_request: folder":           runtimedb.RunOutcomeConfigFailed,
		"adapter: the application is unavailable: provider_unavailable: timeout": runtimedb.RunOutcomeUnavailable,
		"adapter: the application is unavailable: not_configured":                runtimedb.RunOutcomeUnavailable,
		"something else entirely":                                                runtimedb.RunOutcomeUnavailable,
	} {
		if got, _ := classifyUnavailable(text); got != want {
			t.Errorf("%q → %s, want %s", text, got, want)
		}
	}
}
