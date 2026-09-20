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
	session, err := db.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "butler", ActingFor: "mika", Provider: "test",
		Mode: runtimedb.ButlerModeFull, Surface: runtimedb.ButlerSurfaceFamily,
		TranscriptJSON: "{}",
	})
	if err != nil {
		t.Fatalf("InsertButlerSession: %v", err)
	}
	taskID, err := db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember,
		Audience:  runtimedb.ButlerAudiencePrivate,
		SessionID: session, OriginConnection: fixtureConnection,
		Objective: "answer the question", CompletionCriteria: "answered",
	})
	if err != nil {
		t.Fatalf("InsertButlerTask: %v", err)
	}
	ledger.DerivedAuthorityChecks()
	f := &derivedFixture{db: db, ledger: ledger, taskID: taskID, session: session}
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

// The grants the work was admitted under are revoked.
//
// Mutation, both directions: treating "no grants at all" as "every
// grant revoked" makes the second case fail — most work holds no grant
// and must not be killed for it. Treating an EXPIRED grant as a
// revocation makes the third fail, and would stop tasks that simply
// finished with the tool.
func TestTaskLosesAuthorityAtItsNextBoundaryWhenTheGrantIsRevoked(t *testing.T) {
	f := newDerivedFixture(t)
	seq, err := f.db.GrantTool(runtimedb.ToolGrant{
		Scope: runtimedb.GrantScopeTask, ScopeID: f.taskID,
		AgentClass: runtimedb.AgentClassMember, Tool: "restricted_tool", GrantedBy: "root-admin",
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Still fine while the grant stands.
	if err := f.ledger.Authorize(f.taskID); err != nil {
		t.Fatalf("a task with a live grant was refused: %v", err)
	}
	if err := f.db.RevokeToolGrant(seq, "root-admin"); err != nil {
		t.Fatal(err)
	}
	f.refused(t, "revoked")

	// ONE revocation is enough, even with another grant still live.
	// Stopping only once every grant had gone was a threshold with no
	// principle behind it: there is no reason a task should survive the
	// first revocation and die on the last (#287).
	two := newDerivedFixture(t)
	first, err := two.db.GrantTool(runtimedb.ToolGrant{
		Scope: runtimedb.GrantScopeTask, ScopeID: two.taskID,
		AgentClass: runtimedb.AgentClassMember, Tool: "tool_one", GrantedBy: "root-admin",
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := two.db.GrantTool(runtimedb.ToolGrant{
		Scope: runtimedb.GrantScopeTask, ScopeID: two.taskID,
		AgentClass: runtimedb.AgentClassMember, Tool: "tool_two", GrantedBy: "root-admin",
	}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := two.ledger.Authorize(two.taskID); err != nil {
		t.Fatalf("a task with two live grants was refused: %v", err)
	}
	if err := two.db.RevokeToolGrant(first, "root-admin"); err != nil {
		t.Fatal(err)
	}
	two.refused(t, "tool_one")

	// A task that never held a grant is untouched. "No grants exist" is
	// not "a grant was taken away".
	g := newDerivedFixture(t)
	if err := g.ledger.Authorize(g.taskID); err != nil {
		t.Fatalf("a task that holds no grant was refused: %v", err)
	}

	// And an expired grant is not a revocation: the tool it covered is
	// no longer permitted, which the resolver answers per tool, but the
	// work carries on. A one-nanosecond window is the shortest the
	// store accepts and is already past by the time it returns.
	h := newDerivedFixture(t)
	if _, err := h.db.GrantTool(runtimedb.ToolGrant{
		Scope: runtimedb.GrantScopeTask, ScopeID: h.taskID,
		AgentClass: runtimedb.AgentClassMember, Tool: "restricted_tool", GrantedBy: "root-admin",
	}, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if err := h.ledger.Authorize(h.taskID); err != nil {
		t.Fatalf("an expired grant killed the whole task: %v", err)
	}
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
		SessionID: f.session, OriginConnection: fixtureConnection,
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
