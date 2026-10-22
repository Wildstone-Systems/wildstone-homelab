package automation

// PersonalBackgroundAuthority against a real store: S02's evaluator
// plugged into S03's dispatch gate. The Person-authorized, Owner-approved
// intersection dispatches; anything short of it — or a grant stretched
// over a different Agent, epoch or Person than the run's — is refused;
// and S03's per-edit version bump agrees with S02's approved-version
// check.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerstore"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

type personalAuthFixture struct {
	db    *runtimedb.DB
	eng   *SubscriptionEngine
	clock *fakeClock
	rec   *recorder
	agent int64
	sub   runtimedb.AgentSubscription
}

// newPersonalAuthFixture: Owner mika, member sam with a Personal mail
// connection placed on n1, an active credential, a Member Agent hobson
// sam may use, a scheduled subscription naming sam's authorization, the
// authorization itself and mika's approval — the whole intersection.
func newPersonalAuthFixture(t *testing.T) *personalAuthFixture {
	t.Helper()
	db, err := runtimedb.Open(migrated.Into(t, filepath.Join(t.TempDir(), "keystone.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, ledger := subscriptionButler(t, db)
	if err := db.RecordButlerMember("sam", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	agent, err := db.CreateButler(runtimedb.Butler{
		Name: "hobson", Directive: "watch the post", Surface: runtimedb.ButlerSurfaceFamily,
		AgentClass: runtimedb.AgentClassMember, Model: "kitchen-box",
	}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.GrantButlerAccess("hobson", "sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateConnection(runtimedb.IntegrationConnection{
		Name: "sam-mail", Adapter: "wild-mail-adapter", Ownership: runtimedb.OwnershipPersonal,
		Owner: "sam", CreatedBy: "root-admin",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPlacement("sam-mail", "n1", runtimedb.PlacementPrimary, 0, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPlacementPrepared("sam-mail", "n1", true, true, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.PlaceConnection("sam-mail", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.SetConnectionExecutable("sam-mail", "mail-roundcube@1.1.0/wild-mail-adapter", "root-admin"); err != nil {
		t.Fatal(err)
	}
	staged, err := db.StagePersonalCredential("sam-mail", "", "sam@example.internal", "sam", []string{"n1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkCredentialCopy(staged.Seq, "n1", true, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ActivatePersonalCredential(staged.Seq, "sam"); err != nil {
		t.Fatal(err)
	}

	req := runtimedb.BackgroundAuthorizationRequest{
		Connection: "sam-mail", Scope: runtimedb.BackgroundScope{Folders: []string{"INBOX"}},
		ProcessingLocation: runtimedb.ProcessingLocal, Agent: agent, TargetCalendar: "sam-family",
	}
	preview, err := db.PrepareBackgroundAuthorization("sam", req)
	if err != nil {
		t.Fatal(err)
	}
	match, _ := json.Marshal(runtimedb.BackgroundSubscriptionMatch("sam-mail", preview.Digest))
	id, err := db.ProposeAgentSubscription(runtimedb.AgentSubscriptionSpec{
		Butler: agent, TriggerKind: runtimedb.SubscriptionTriggerSchedule, MatchKind: "mail.changes",
		MatchJSON: string(match), Audience: runtimedb.AudienceOwner, Capabilities: []string{"mail.changes"},
		SourceConnection: "sam-mail", Cadence: 15 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	req.Subscription = id
	if _, err := db.AuthorizeBackgroundUseAsPerson("sam", req); err != nil {
		t.Fatalf("AuthorizeBackgroundUseAsPerson: %v", err)
	}
	if err := db.ApproveAgentSubscription(id, "mika"); err != nil {
		t.Fatal(err)
	}
	sub, err := db.GetAgentSubscription(id)
	if err != nil {
		t.Fatal(err)
	}
	f := &personalAuthFixture{db: db, clock: &fakeClock{t: time.Now().UTC()}, rec: &recorder{}, agent: agent, sub: sub}
	f.eng = f.engine(ledger)
	return f
}

func (f *personalAuthFixture) engine(ledger *butlerstore.Ledger) *SubscriptionEngine {
	return &SubscriptionEngine{
		DB: f.db, Ledger: ledger, Now: f.clock.Now, Authority: PersonalBackgroundAuthority{DB: f.db},
		Dispatchers: map[string]ScheduledDispatcher{"mail.changes": f.rec},
	}
}

func (f *personalAuthFixture) tick(t *testing.T) ScheduledTickOutcome {
	t.Helper()
	out, err := f.eng.TickScheduled(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// finish completes the last run with a committed checkpoint and moves
// the clock past the next due time.
func (f *personalAuthFixture) finish(t *testing.T) {
	t.Helper()
	if _, err := f.eng.CompleteRun(context.Background(), f.rec.last(t).Run.ID, runtimedb.SubscriptionRunReport{
		Outcome: runtimedb.RunOutcomeOK, Checkpoint: json.RawMessage(`{"folders":[]}`),
	}); err != nil {
		t.Fatalf("CompleteRun: %v", err)
	}
	f.clock.Advance(16 * time.Minute)
}

func (f *personalAuthFixture) use(t *testing.T) BackgroundUse {
	t.Helper()
	conn, err := f.db.GetConnection("sam-mail")
	if err != nil {
		t.Fatal(err)
	}
	return BackgroundUse{
		Subscription: f.sub.ID, SubscriptionVersion: f.sub.Version, Butler: f.agent,
		Connection: "sam-mail", ConnectionEpoch: conn.LeaseEpoch, PersonID: f.sub.PersonID,
	}
}

func TestPersonalAuthorityDispatchesTheIntersection(t *testing.T) {
	f := newPersonalAuthFixture(t)
	if out := f.tick(t); out.Dispatched != 1 || out.Blocked != 0 {
		t.Fatalf("with both halves in place: %+v (blocked: %q)", out, f.blockedReason(t))
	}
	run := f.rec.last(t)
	if run.Author != "sam" {
		t.Fatalf("the run is authored by %q, want sam", run.Author)
	}
	// The checkpoint commit re-checks the same authority live.
	f.finish(t)

	// The Person revokes: the next due run is blocked as health.
	if err := f.db.RevokeBackgroundAuthorization("sam-mail", "sam", "stop watching"); err != nil {
		t.Fatal(err)
	}
	if out := f.tick(t); out.Dispatched != 0 || out.Blocked != 1 {
		t.Fatalf("after the Person revoked: %+v", out)
	}
}

func TestPersonalAuthorityHoldsTheGrantToTheRun(t *testing.T) {
	f := newPersonalAuthFixture(t)
	auth := PersonalBackgroundAuthority{DB: f.db}
	ctx := context.Background()
	if err := auth.AuthorizeBackgroundUse(ctx, f.use(t)); err != nil {
		t.Fatalf("the exact run = %v, want allowed", err)
	}
	cases := map[string]func(*BackgroundUse){
		"no Personal source":    func(u *BackgroundUse) { u.Connection = "" },
		"another Agent":         func(u *BackgroundUse) { u.Butler = f.agent + 100 },
		"a moved epoch":         func(u *BackgroundUse) { u.ConnectionEpoch += 7 },
		"another Person":        func(u *BackgroundUse) { u.PersonID = "person-somebody-else" },
		"no Person at all":      func(u *BackgroundUse) { u.PersonID = "" },
		"an unapproved version": func(u *BackgroundUse) { u.SubscriptionVersion = 2 },
		"another subscription":  func(u *BackgroundUse) { u.Subscription += 100 },
		"another connection":    func(u *BackgroundUse) { u.Connection = "bo-mail" },
	}
	for name, mutate := range cases {
		u := f.use(t)
		mutate(&u)
		if err := auth.AuthorizeBackgroundUse(ctx, u); !errors.Is(err, ErrBackgroundUseDenied) {
			t.Errorf("%s: %v, want denied", name, err)
		}
	}
	// An unset epoch is not a mismatch: the gate fills it for every
	// connection it reads, and the grant carries its own fence.
	u := f.use(t)
	u.ConnectionEpoch = 0
	if err := auth.AuthorizeBackgroundUse(ctx, u); err != nil {
		t.Fatalf("an unset epoch = %v, want allowed", err)
	}
	if err := (PersonalBackgroundAuthority{}).AuthorizeBackgroundUse(ctx, f.use(t)); !errors.Is(err, ErrBackgroundUseDenied) {
		t.Fatalf("no store wired = %v, want denied", err)
	}
}

// S03 bumps the version on every edit; S02 checks the approved version
// the gate dispatches under. An edit — even one that leaves the digested
// content alone — blocks until the Owner approves the new version, and
// that approval alone (the Person's digest did not move) resumes it.
func TestPersonalAuthorityFollowsTheEditedVersion(t *testing.T) {
	f := newPersonalAuthFixture(t)
	if out := f.tick(t); out.Dispatched != 1 {
		t.Fatalf("first run: %+v", out)
	}
	f.finish(t)

	same := f.sub.MatchJSON
	v, err := f.db.ReviseAgentSubscription(f.sub.ID, runtimedb.AgentSubscriptionRevision{MatchJSON: &same}, "mika")
	if err != nil {
		t.Fatal(err)
	}
	if out := f.tick(t); out.Dispatched != 0 || out.Blocked != 1 {
		t.Fatalf("edited, not yet re-approved: %+v", out)
	}
	if err := f.db.ApproveAgentSubscriptionVersion(f.sub.ID, v, "mika"); err != nil {
		t.Fatal(err)
	}
	if out := f.tick(t); out.Dispatched != 1 {
		t.Fatalf("version %d approved: %+v (blocked: %q)", v, out, f.blockedReason(t))
	}
	if got := f.rec.last(t).Run.SubscriptionVersion; got != v {
		t.Fatalf("the run carries version %d, want %d", got, v)
	}

	// A content edit (a wider capability set) moves the digest the
	// Person authorized: re-approval by the Owner alone does not resume.
	f.finish(t)
	caps := []string{"mail.changes", "mail.message"}
	v, err = f.db.ReviseAgentSubscription(f.sub.ID, runtimedb.AgentSubscriptionRevision{Capabilities: caps}, "mika")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.ApproveAgentSubscriptionVersion(f.sub.ID, v, "mika"); err != nil {
		t.Fatal(err)
	}
	if out := f.tick(t); out.Dispatched != 0 || out.Blocked != 1 {
		t.Fatalf("content edited and only the Owner re-approved: %+v", out)
	}
}

func (f *personalAuthFixture) blockedReason(t *testing.T) string {
	t.Helper()
	s, err := f.db.GetSubscriptionSchedule(f.sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s.BlockedReason
}
