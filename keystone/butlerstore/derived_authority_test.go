package butlerstore

// Derived Task authority (design §10.1, §18.1).
//
// "A Task inherits the bounded authority of the request that created
// it, is re-evaluated at every authorization boundary, and can never
// widen beyond that originating authority. If the Person, session,
// Connection or grant behind it later loses authority, the Task loses
// it too at its next boundary."
//
// Four revocation cases, one never-widens case, and one that pins the
// whole thing as NOT a grant object. Every negative was mutation-tested
// against a deliberately broken implementation; the notes on each say
// which mutation it catches.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// derivedFixture is a task with every origin recorded and every leg
// wired — the shape a real conversation produces.
type derivedFixture struct {
	db      *runtimedb.DB
	ledger  *Ledger
	taskID  string
	session string
	connRef int64
}

const fixtureConnection = "matrix:!room:example.org"

func newDerivedFixture(t *testing.T) *derivedFixture {
	t.Helper()
	db, ledger := testLedger(t, t.TempDir()+"/derived.db")
	for _, who := range []string{"mika", "sam"} {
		if err := db.AdmitButlerMember(who, "root-admin"); err != nil {
			t.Fatalf("admitting %q: %v", who, err)
		}
	}
	// A Connection the task can have arrived over. The built-in
	// surfaces cannot be withdrawn, so a test about losing one needs a
	// Connection somebody added.
	if err := db.UpsertModel(runtimedb.Model{
		Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama",
		ProviderModel: "llama", Endpoint: "http://kitchen:11434",
		Readiness: runtimedb.ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	if _, err := db.CreateButler(runtimedb.Butler{
		Name: "alfred", Model: "kitchen-box",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember,
	}, "root-admin"); err != nil {
		t.Fatalf("CreateButler: %v", err)
	}
	if _, err := db.AddButlerAddress("alfred", "matrix", "!room:example.org", "the kitchen", "root-admin"); err != nil {
		t.Fatalf("AddButlerAddress: %v", err)
	}
	conn, err := db.ChatConnectionAt("matrix", "!room:example.org")
	if err != nil {
		t.Fatalf("ChatConnectionAt: %v", err)
	}
	session, serr := db.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "butler", ActingFor: "mika", Provider: "test",
		Mode: runtimedb.ButlerModeFull, Surface: runtimedb.ButlerSurfaceFamily,
		TranscriptJSON: "{}",
	})
	if serr != nil {
		t.Fatalf("InsertButlerSession: %v", serr)
	}
	taskID, terr := db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember,
		Audience:  runtimedb.ButlerAudiencePrivate,
		SessionID: session, OriginConnection: fixtureConnection, OriginConnectionRef: conn.Seq,
		Objective: "answer the question", CompletionCriteria: "answered",
	})
	if terr != nil {
		t.Fatalf("InsertButlerTask: %v", terr)
	}
	ledger.DerivedAuthorityChecks()
	f := &derivedFixture{db: db, ledger: ledger, taskID: taskID, session: session, connRef: conn.Seq}
	// The fixture must be authorized BEFORE anything is revoked, or
	// every test below would pass against an implementation that
	// refuses unconditionally.
	if err := ledger.Authorize(taskID); err != nil {
		t.Fatalf("a task with every origin intact was refused: %v", err)
	}
	return f
}

// refused asserts the task has lost authority, and that the reason says
// what went away.
func (f *derivedFixture) refused(t *testing.T, mentions string) {
	t.Helper()
	err := f.ledger.Authorize(f.taskID)
	if err == nil {
		t.Fatal("the task still had authority after its origin lost it")
	}
	if !errors.Is(err, ErrAuthorityWithdrawn) {
		t.Fatalf("error is not an authority withdrawal: %v", err)
	}
	if !strings.Contains(err.Error(), mentions) {
		t.Fatalf("the refusal does not say what went away (want %q): %v", mentions, err)
	}
}

// The Person behind the work is disabled, and the task stops at its
// next boundary rather than at the moment of revocation.
//
// Mutation: dropping the OwnerActive call from Authorize makes this
// fail. It is the leg that predates this slice and is here so the four
// are tested as one rule rather than three new ones beside an old one.
func TestTaskLosesAuthorityAtItsNextBoundaryWhenThePersonIsRevoked(t *testing.T) {
	f := newDerivedFixture(t)
	if err := f.db.DisableButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	f.refused(t, "mika")
}

// The session the request was had in is withdrawn.
//
// Mutation: treating an unwired SessionActive as "leg passes" — the
// shape OwnerActive uses, which is right for admission and wrong here —
// makes the unwired case below fail.
func TestTaskLosesAuthorityAtItsNextBoundaryWhenTheSessionIsRevoked(t *testing.T) {
	f := newDerivedFixture(t)
	if err := f.db.RevokeButlerSession(f.session, "root-admin"); err != nil {
		t.Fatal(err)
	}
	f.refused(t, "withdrawn")

	// And a task that RECORDED a session finds an unwired check a
	// refusal, not a pass: an origin nobody can confirm is an origin
	// §10.1 answers with no.
	g := newDerivedFixture(t)
	g.ledger.SessionActive = nil
	g.refused(t, "session")
}

// The Connection the request arrived over is gone.
//
// Mutation: comparing against the CALLER's connection instead of the
// task's recorded one makes this pass for the wrong reason, which the
// never-widens test below catches in the other direction.
func TestTaskLosesAuthorityAtItsNextBoundaryWhenTheConnectionIsRevoked(t *testing.T) {
	f := newDerivedFixture(t)
	if err := f.db.RevokeButlerAddress("matrix", "!room:example.org", "root-admin"); err != nil {
		t.Fatal(err)
	}
	f.refused(t, "connection")

	g := newDerivedFixture(t)
	g.ledger.ConnectionActive = nil
	g.refused(t, "connection")
}

// A revoked grant refuses THAT TOOL, and does not stop the work.
//
// §10.1 names a grant among the things a Task derives from, and this is
// where that is enforced: in the resolver, per tool. Refusing the tool
// is the whole of what the revocation said.
//
// A task-level version of this check lived here and was removed. Grant
// rows are immutable and accumulate, so every predicate over them needs
// a recovery rule and each recovery rule needs another — all-revoked,
// then any-revoked, then newest-after-newest, three rounds on #287 with
// each fix creating the next gap. Stopping the WORK is a state
// transition, which is how A4 already does it: offboarding pauses
// transferred work to paused-offboarded, and an Owner stopping a
// routine cancels it.
//
// Mutations: making the revoked tool permitted fails the first case;
// stopping the task on revocation fails the second.
func TestARevokedGrantRefusesItsToolAndNotTheWork(t *testing.T) {
	f := newDerivedFixture(t)
	auth := &Authority{DB: f.db}
	auth.PersonAuthority = auth.PersonIsAdmitted()

	seq, err := f.db.GrantTool(runtimedb.ToolGrant{
		Scope: runtimedb.GrantScopeTask, ScopeID: f.taskID,
		AgentClass: runtimedb.AgentClassMember, Tool: "restricted_tool", GrantedBy: "root-admin",
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	granted := butler.ToolAuthorityQuery{
		Caller: butler.CallerPersonFacing, Person: "mika", Connection: fixtureConnection,
		AgentClass: runtimedb.AgentClassMember, Tool: "restricted_tool",
		Structural: true, GrantRequired: true, TaskID: f.taskID,
	}
	if d := mustAuthorize(t, auth, granted); !d.Allowed {
		t.Fatalf("a live grant did not permit its tool: %q", d.Reason)
	}

	if err := f.db.RevokeToolGrant(seq, "root-admin"); err != nil {
		t.Fatal(err)
	}

	// The tool is refused.
	if mustAuthorize(t, auth, granted).Allowed {
		t.Fatal("a revoked grant still permitted its tool")
	}

	// The work is not stopped: the task still has authority, and a tool
	// that never needed the grant still runs. An Owner who wants the
	// work stopped cancels it, which the state machine already answers.
	if err := f.ledger.Authorize(f.taskID); err != nil {
		t.Fatalf("revoking one tool's grant stopped the whole task: %v", err)
	}
	ungranted := granted
	ungranted.Tool, ungranted.GrantRequired = "get_household_events", false
	if d := mustAuthorize(t, auth, ungranted); !d.Allowed {
		t.Fatalf("a tool that never needed the grant was refused: %q", d.Reason)
	}

	// And cancelling IS how the work stops, so nothing is lost.
	if err := f.db.TransitionButlerTask(f.taskID, runtimedb.ButlerTaskCancelled,
		"owner stopped it", "root-admin"); err != nil {
		t.Fatal(err)
	}
	f.refused(t, "cancelled")
}

// A Task never exceeds the authority of the request that created it
// (§18.1). Naming a task id does not let a different person, or the
// same person on a different Connection, carry its work on.
//
// Mutation, both directions: dropping the comparison lets the first two
// cases through; comparing when the task recorded NO origin (a routine,
// or a pre-0062 task) makes the last case fail, and would refuse every
// routine in the house.
func TestTaskNeverExceedsTheAuthorityOfItsRequest(t *testing.T) {
	f := newDerivedFixture(t)
	auth := &Authority{DB: f.db}
	auth.PersonAuthority = auth.PersonIsAdmitted()

	query := func(person, connection string) butler.ToolAuthorityQuery {
		return butler.ToolAuthorityQuery{
			Caller: butler.CallerPersonFacing, Person: person, Connection: connection,
			AgentClass: runtimedb.AgentClassMember, Tool: "get_household_events",
			Structural: true, TaskID: f.taskID,
		}
	}

	// The person whose request it is, on the Connection it arrived
	// over. Allowed — without this the rest proves only that something
	// refuses everything.
	if d := mustAuthorize(t, auth, query("mika", fixtureConnection)); !d.Allowed {
		t.Fatalf("the originating person was refused their own task: %q", d.Reason)
	}

	// Another admitted person cannot carry it on, even though they are
	// perfectly entitled to use the tool on their own account.
	d := mustAuthorize(t, auth, query("sam", fixtureConnection))
	if d.Allowed {
		t.Fatal("somebody else's task was carried on by naming its id")
	}
	if !strings.Contains(d.Reason, "mika") {
		t.Fatalf("the refusal does not name whose work it is: %q", d.Reason)
	}

	// Nor the same person arriving somewhere else. This is the leg
	// §10.1 names and #212 will give semantics to; what it means TODAY
	// is that the Connection cannot change underneath the work.
	if mustAuthorize(t, auth, query("mika", "console")).Allowed {
		t.Fatal("a task was carried on over a connection it never arrived through")
	}

	// A task that recorded no Connection — a routine, or one created
	// before 0062 — has no Connection leg, and is not refused for
	// lacking one.
	routine, err := f.db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskRoutine, Author: "mika", Owner: "mika",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember,
		Audience:  runtimedb.ButlerAudiencePrivate,
		Objective: "nightly", CompletionCriteria: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	q := query("mika", "console")
	q.TaskID = routine
	if !mustAuthorize(t, auth, q).Allowed {
		t.Fatal("a task with no recorded connection was refused for not matching one")
	}
}

// Offboarding re-homes a shared task, and the administrator who was
// given it can actually work it (ADR-W030 A4).
//
// A4 moves operational ownership to an administrator, "preserves
// original authorship", and PAUSES the work for grant and connection
// review before it may resume. The first version of this slice compared
// the caller against the immutable AUTHOR, which made that path a dead
// end: a transferred task could be reviewed and moved back to running
// and would then refuse every tool call for ever, because the author it
// derives from is exactly the person who was offboarded. Found by a
// review bot on #287.
//
// Mutations, both directions: comparing the author again makes the
// first case fail. Dropping the comparison entirely — letting anybody
// named on the query carry it on — makes the last case fail.
func TestATransferredTaskIsItsNewOwnersToCarryOn(t *testing.T) {
	f := newDerivedFixture(t)
	auth := &Authority{DB: f.db}
	auth.PersonAuthority = auth.PersonIsAdmitted()

	// A4 transfers SHARED work only; personal work just pauses. So this
	// test needs its own shared task rather than the fixture's private
	// one, which the other tests here depend on staying private.
	shared, err := f.db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember,
		Audience:  runtimedb.ButlerAudienceShared,
		SessionID: f.session, OriginConnection: fixtureConnection, OriginConnectionRef: f.connRef,
		Objective: "the shared thing", CompletionCriteria: "done",
	})
	if err != nil {
		t.Fatal(err)
	}

	query := func(person string) butler.ToolAuthorityQuery {
		return butler.ToolAuthorityQuery{
			Caller: butler.CallerPersonFacing, Person: person, Connection: fixtureConnection,
			AgentClass: runtimedb.AgentClassMember, Tool: "get_household_events",
			Structural: true, TaskID: shared,
		}
	}

	// Ownership moves the only way anything ever moves it: offboarding.
	// runtimedb/offboarding.go is the sole writer of owner_person_id,
	// which is what makes comparing the owner as tight as comparing the
	// author everywhere else.
	if _, err := f.db.OffboardMember("mika", "root-admin", "sam"); err != nil {
		t.Fatalf("OffboardMember: %v", err)
	}
	task, err := f.db.GetButlerTask(shared)
	if err != nil {
		t.Fatal(err)
	}
	if task.Owner != "sam" || task.Author != "mika" {
		t.Fatalf("offboarding did not re-home the task: owner=%s author=%s",
			task.Owner, task.Author)
	}

	// The new owner may carry it on.
	if d := mustAuthorize(t, auth, query("sam")); !d.Allowed {
		t.Fatalf("the administrator a task was transferred to was refused it: %q", d.Reason)
	}

	// The offboarded author may not — their authority is gone, and the
	// author column is a record of whose request it was, not a key.
	if mustAuthorize(t, auth, query("mika")).Allowed {
		t.Fatal("an offboarded author still carried on their transferred task")
	}

	// And the whole recovery path works end to end: the administrator
	// carries it on from the CONSOLE, not from the chat Connection the
	// offboarded person used. Before this, the owner check passed and
	// the Connection check then refused the first tool call for ever —
	// nobody can continue that session, and every other path arrives
	// somewhere else (#287).
	if task.OriginConnection != "" || task.OriginConnectionRef != 0 {
		t.Fatalf("the transfer kept the offboarded person's connection: conn=%q ref=%d",
			task.OriginConnection, task.OriginConnectionRef)
	}
	// The session STAYS. ButlerTaskForSession is the only production
	// lookup back to this task, so clearing it would strand the
	// verified ledger A4 transfers the work in order to preserve — and
	// it is not what blocked, because derived authority asks whether
	// the SESSION was withdrawn and offboarding withdraws the PERSON.
	// An earlier version of this fix cleared it; a review bot on #287
	// caught that, and this assertion is what would have.
	if task.SessionID == "" {
		t.Fatal("the transfer threw away the only way back to the task")
	}
	if _, err := f.db.ButlerTaskForSession(task.SessionID); err != nil {
		t.Fatalf("the transferred task is no longer reachable from its session: %v", err)
	}
	fromConsole := query("sam")
	fromConsole.Connection = "console"
	if d := mustAuthorize(t, auth, fromConsole); !d.Allowed {
		t.Fatalf("the new owner could not carry the work on from the console: %q", d.Reason)
	}
	// Offboarding revoked the task's grants on the way out, so the
	// administrator issues what the reviewed work actually needs. That
	// is A4's review, and the resolver is what enforces it per tool.
	if _, err := f.db.GrantTool(runtimedb.ToolGrant{
		Scope: runtimedb.GrantScopeTask, ScopeID: shared,
		AgentClass: runtimedb.AgentClassMember, Tool: "restricted_tool", GrantedBy: "sam",
	}, time.Hour); err != nil {
		t.Fatal(err)
	}
	// And the administrator resumes it, which is the act A4 makes them
	// perform: the task waits in paused-offboarded until a human has
	// looked at it.
	if err := f.db.TransitionButlerTask(shared, runtimedb.ButlerTaskRunning,
		"reviewed and resumed", "sam"); err != nil {
		t.Fatal(err)
	}
	if err := f.ledger.Authorize(shared); err != nil {
		t.Fatalf("a reviewed, regranted, resumed transfer was still refused: %v", err)
	}

	// And nobody else, which is what keeps this a re-homing rather than
	// an open door.
	if err := f.db.RecordButlerMember("kai", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AdmitButlerMember("kai", "sam"); err != nil {
		t.Fatal(err)
	}
	if mustAuthorize(t, auth, query("kai")).Allowed {
		t.Fatal("a third party carried on somebody else's transferred task")
	}
}

// Derived authority is not a grant object: nothing is issued when a
// task is created, and nothing is issued by evaluating it (§10.1 —
// "there is no separate Task grant object to issue, hold or revoke").
//
// Mutation: implementing the rule by writing a task-scoped grant row at
// creation — the obvious wrong way to make a Task carry authority —
// makes this fail immediately.
func TestTaskAuthorityIsNotAGrantObject(t *testing.T) {
	f := newDerivedFixture(t)

	grants, err := f.db.ListToolGrants(runtimedb.GrantScopeTask, f.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Fatalf("creating a task issued %d grant(s): %+v", len(grants), grants)
	}

	// Driving it through several boundaries issues nothing either.
	for i := 0; i < 3; i++ {
		if err := f.ledger.Authorize(f.taskID); err != nil {
			t.Fatalf("boundary %d: %v", i, err)
		}
	}
	after, err := f.db.ListToolGrants(runtimedb.GrantScopeTask, f.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("re-evaluating derived authority issued %d grant(s): %+v", len(after), after)
	}
}

// revokingProvider withdraws the task's origin as it answers — the
// model call is the window this test exists to close.
type revokingProvider struct {
	steps  []*provider.ChatResponse
	at     int
	revoke func()
}

func (p *revokingProvider) Name() string { return "revoking" }

func (p *revokingProvider) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	if p.at >= len(p.steps) {
		return &provider.ChatResponse{Text: "done"}, nil
	}
	resp := p.steps[p.at]
	p.at++
	// The withdrawal lands WHILE the call is in flight. Doing it here
	// rather than between rounds is the whole point: the runner's
	// pre-round check has already passed by the time this happens.
	if p.revoke != nil {
		p.revoke()
		p.revoke = nil
	}
	return resp, nil
}

// failingThenRevokingProvider fails its first attempt and withdraws the
// task's origin, so the RETRY is the dispatch under test.
type failingThenRevokingProvider struct {
	attempts int
	revoke   func()
}

func (p *failingThenRevokingProvider) Name() string { return "flaky" }

func (p *failingThenRevokingProvider) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	p.attempts++
	if p.attempts == 1 {
		// The first attempt fails, and authority goes away while the
		// runner is deciding to retry — which is exactly the window a
		// stall leaves open, only faster.
		if p.revoke != nil {
			p.revoke()
			p.revoke = nil
		}
		return nil, errors.New("provider unavailable")
	}
	return &provider.ChatResponse{Text: "second attempt"}, nil
}

// A retry is a dispatch, and it re-checks authority first.
//
// modelRound retried modelAttempt without re-authorizing, so a stall or
// a provider failure — by definition some time after the round's own
// check, often the whole StallTimeout — sent the conversation again
// after its Person, session, Connection or grant had been withdrawn.
// Found by a review bot on #287, one round after the same bot found the
// tool-dispatch gap: the boundary had been put next to the hole rather
// than in it.
//
// Mutation: moving the check back out of the attempt loop makes this
// fail — the second attempt goes out and the provider sees two calls.
func TestARetryReChecksAuthorityBeforeItGoesOut(t *testing.T) {
	f := newDerivedFixture(t)
	r := runner(newCountingToolset(), f.ledger)
	r.RetryLimit = 1

	p := &failingThenRevokingProvider{
		revoke: func() { _ = f.db.RevokeButlerSession(f.session, "root-admin") },
	}
	if _, err := r.Run(context.Background(), p, f.taskID, "look at something"); err == nil {
		t.Fatal("the run retried after its session was withdrawn")
	}
	if p.attempts != 1 {
		t.Fatalf("the provider saw %d attempts; the retry went out after authority was withdrawn", p.attempts)
	}
}

// Authority withdrawn during a model call stops the tool calls that
// call returned, before any of them runs.
//
// The runner authorized before the slice and before each model round,
// and then dispatched whatever came back — so the gap was exactly as
// long as the model took to answer, which is the slowest thing it does.
// ADR-W030 A2.1 says "at every retrieval and dispatch"; dispatch was
// the boundary that was missing. Found by a review bot on #287.
//
// Mutations: removing either stillAuthorized call from the runner makes
// this fail. Removing only the final-answer one makes the second case
// fail on its own.
func TestAuthorityWithdrawnDuringAModelCallStopsTheDispatch(t *testing.T) {
	f := newDerivedFixture(t)
	ts := newCountingToolset()
	r := runner(ts, f.ledger)

	p := &revokingProvider{
		steps:  []*provider.ChatResponse{readsFor("node-a")},
		revoke: func() { _ = f.db.RevokeButlerSession(f.session, "root-admin") },
	}
	if _, err := r.Run(context.Background(), p, f.taskID, "look at node-a"); err == nil {
		t.Fatal("the run continued after its session was withdrawn mid-call")
	}
	// Every call, whatever it was keyed as. Asserting on a single
	// expected key is how the first version of this test passed against
	// an implementation with no dispatch boundary at all: countingToolset
	// keys by name AND argument, so "get_health" was never a key and the
	// assertion was vacuous. The run errored on the NEXT round's check,
	// which is exactly the behaviour this test exists to rule out.
	if n := len(ts.calls); n != 0 {
		t.Fatalf("%d tool call(s) were dispatched after authority was withdrawn: %v", n, ts.calls)
	}

	// The same for a FINAL ANSWER: completing the task writes evidence
	// and a result summary the household reads, and it must not be
	// recorded for a person whose authority went away mid-call.
	g := newDerivedFixture(t)
	gr := runner(newCountingToolset(), g.ledger)
	answer := &revokingProvider{
		steps:  []*provider.ChatResponse{{Text: "all fine"}},
		revoke: func() { _ = g.db.RevokeButlerSession(g.session, "root-admin") },
	}
	if _, err := gr.Run(context.Background(), answer, g.taskID, "anything"); err == nil {
		t.Fatal("a final answer was accepted after its session was withdrawn mid-call")
	}
	task, err := g.db.GetButlerTask(g.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State == runtimedb.ButlerTaskCompleted {
		t.Fatal("the task was completed under withdrawn authority")
	}
}
