package butlerlive

// ADR-W031's evidence at the level that SERVES a conversation, which is
// the whole point of the exercise. Every guarantee below was already
// true of the store; none of it was true of a message arriving at a bot
// until this package existed, and a property that holds one layer away
// from where messages actually arrive is not a property anyone can rely
// on.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// witness is a provider that records every message it was shown.
//
// It is the measuring instrument for A4, and it measures the right
// thing: not "was the routing correct" but "what did this model
// actually receive". A test that asserted on a routing decision would
// pass just as happily if the material had reached the other model
// first and been routed afterwards, which is the failure A2 says a
// router cannot rule out.
type witness struct {
	name string
	mu   sync.Mutex
	saw  []string
	// reply is what it answers with; empty means a plain acknowledgement.
	reply            string
	objectiveVerdict string
	// toolCalls, when set, is returned on the FIRST call instead of
	// text, so a test can drive one tool dispatch.
	toolCalls []provider.ToolCall
	rounds    int
	// dieAfter makes the provider unreachable once it has answered that
	// many rounds, standing in for a model that goes away mid-turn. 0
	// means it never dies.
	dieAfter int
}

// revive brings the model back, as a restarted deployment finds it.
//
// It deliberately KEEPS the queued tool call. A resumed slice
// reconstructs context by asking the model again rather than replaying
// a saved transcript, so the model asks for the same read a second
// time — that is normal, and it is precisely the moment the step ledger
// has to earn its keep by answering from what is already verified
// instead of dispatching again.
//
// Clearing it here would make the "not dispatched twice" assertion
// vacuous: nothing would be re-requested, so nothing could be
// re-dispatched, and the test would pass against a runner that ignored
// the ledger entirely.
func (w *witness) revive() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dieAfter, w.rounds = 0, 0
}

func (w *witness) Name() string { return w.name }

func (w *witness) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dieAfter > 0 && w.rounds >= w.dieAfter {
		return nil, errors.New("provider unreachable")
	}
	w.rounds++
	for _, m := range req.Messages {
		w.saw = append(w.saw, m.Text)
	}
	w.saw = append(w.saw, req.System)
	if strings.HasPrefix(req.System, "Classify both required and claimed effects.") {
		if len(req.Tools) != 0 {
			return nil, errors.New("verification exposed tools")
		}
		verdict := w.objectiveVerdict
		if verdict == "" {
			verdict = `{"kind":"informational"}`
		}
		return &provider.ChatResponse{Text: verdict}, nil
	}
	if w.toolCalls != nil && w.rounds == 1 {
		return &provider.ChatResponse{ToolCalls: w.toolCalls, StopReason: "tool_use"}, nil
	}
	reply := w.reply
	if reply == "" {
		reply = w.name + " answered"
	}
	return &provider.ChatResponse{Text: reply}, nil
}

// sawAnything reports whether this model received a single byte of
// anything at all.
func (w *witness) sawAnything() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.saw) > 0
}

func (w *witness) sawText(want string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range w.saw {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// servedLedger wires a ledger the way cmd/keystone wires the one that
// serves real conversations: all four legs of §10.1's derived Task
// authority on. A ledger without them refuses any task that recorded an
// origin it cannot confirm, which is a real deployment's answer too —
// so a test that left them off would be testing the refusal rather than
// whatever it meant to.
func servedLedger(l *butlerstore.Ledger) *butlerstore.Ledger {
	l.DerivedAuthorityChecks()
	return l
}

// countingToolset records how many times each tool was really
// dispatched — the measurement "resumes without repeating a dispatch"
// rests on.
type countingToolset struct {
	mu    sync.Mutex
	calls map[string]int
	// authorizer, task, caller, person and connection are what
	// butlerlive bound onto this surface, captured so a test can assert
	// the per-Butler scope and the person-facing identity were applied
	// before anything ran.
	authorizer butler.ToolAuthorizer
	task       string
	caller     butler.CallerKind
	person     string
	connection string
}

func newCountingToolset() *countingToolset {
	return &countingToolset{calls: map[string]int{}}
}

func (ts *countingToolset) SetAuthorizer(a butler.ToolAuthorizer) { ts.authorizer = a }
func (ts *countingToolset) SetSessionTask(id string)              { ts.task = id }

func (ts *countingToolset) SetCaller(kind butler.CallerKind, person, connection string) {
	ts.caller, ts.person, ts.connection = kind, person, connection
}

func (ts *countingToolset) Permits(string, bool) (bool, string) { return true, "" }

func (ts *countingToolset) OfferedSpecs(bool) []provider.Tool {
	return []provider.Tool{{Name: "get_health", Description: "read a node's health"}}
}

func (ts *countingToolset) Execute(_ context.Context, name string, input map[string]any, _ bool) (butler.ToolOutcome, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	key := fmt.Sprintf("%s/%v", name, input["node"])
	ts.calls[key]++
	return butler.ToolOutcome{Content: key + ": ok"}, nil
}

func (ts *countingToolset) SystemPrompt() string { return "operator surface" }

func (ts *countingToolset) Capabilities(bool) butler.CapabilityManifest {
	return butler.CapabilityManifest{
		AgentClass: butler.AgentClassOwner, ModelVerdict: butler.VerdictFull,
		Tools: []butler.CapabilityTool{{Name: "get_health", Available: true}},
	}
}

func (ts *countingToolset) count(key string) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.calls[key]
}

// fixture builds a deployment with two Butlers that differ in exactly
// the way the design cares about: Alfred runs on a LOCAL model, Robert
// on a CLOUD one, and each answers at its own address.
type fixture struct {
	db      *runtimedb.DB
	butlers *Butlers
	local   *witness
	cloud   *witness
	tools   map[string]*countingToolset
}

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

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := runtimedb.Open(migrated.Path(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, r := range []runtimedb.Model{
		{Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama",
			Endpoint: "http://kitchen:11434", Readiness: runtimedb.ModelReady, Enabled: true},
		{Name: "cloud", Kind: runtimedb.ModelCloud, Provider: "anthropic", ProviderModel: "sonnet",
			Readiness: runtimedb.ModelReady, Enabled: true},
	} {
		if err := db.UpsertModel(r, "root-admin"); err != nil {
			t.Fatalf("UpsertModel(%s): %v", r.Name, err)
		}
	}
	for _, m := range []struct{ name, role string }{
		{"mika", runtimedb.RoleOwner}, {"sam", runtimedb.RoleMember},
	} {
		if err := db.RecordButlerMember(m.name, m.role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember(m.name, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []struct{ name, route string }{
		{"alfred", "kitchen-box"}, {"robert", "cloud"},
	} {
		if _, err := db.CreateButler(runtimedb.Butler{
			Name: b.name, Directive: "look after the " + b.name + " things",
			Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner,
			Model: b.route,
		}, "mika"); err != nil {
			t.Fatalf("CreateButler(%s): %v", b.name, err)
		}
		if err := db.GrantButlerAccess(b.name, "mika", "mika"); err != nil {
			t.Fatal(err)
		}
		// One bot per Butler, each its own address (A7).
		if _, err := db.AddButlerAddress(b.name, "telegram", "chat-telegram-"+b.name, "", "mika"); err != nil {
			t.Fatal(err)
		}
	}

	lease, err := db.AcquireCoordinator("keystone-test")
	if err != nil {
		t.Fatalf("AcquireCoordinator: %v", err)
	}
	f := &fixture{
		db:    db,
		local: &witness{name: "ollama"},
		cloud: &witness{name: "anthropic"},
		tools: map[string]*countingToolset{},
	}
	f.butlers = &Butlers{
		DB:     db,
		Ledger: servedLedger(butlerstore.New(db, lease)),
		Providers: func(b runtimedb.Butler) (provider.Provider, error) {
			switch b.ModelKind {
			case runtimedb.ModelLocal:
				return f.local, nil
			default:
				return f.cloud, nil
			}
		},
		Gateways: GatewayFunc(func(b runtimedb.Butler, _ Turn) (butler.Toolset, error) {
			// One toolset per Butler for the LIFETIME OF THE TEST, not
			// per call. Production builds a fresh gateway per session
			// (that is what keeps one Butler's scope out of another's),
			// but the thing on the other side of it — the nodes a read
			// actually reaches — survives a restart. Counting on a
			// per-call toolset would reset to zero exactly when the
			// "does a resume re-dispatch?" question is being asked, and
			// the test would pass by forgetting.
			if ts, ok := f.tools[b.Name]; ok {
				return ts, nil
			}
			ts := newCountingToolset()
			f.tools[b.Name] = ts
			return ts, nil
		}),
	}
	return f
}

func (f *fixture) turn(butlerName, text string) Turn {
	return Turn{
		Transport: "telegram",
		Address:   "chat-telegram-" + butlerName,
		Member:    "mika",
		Text:      text,
	}
}

// A4/A2, proven where it matters: material addressed to the local-model
// Butler reaches the local model and the cloud model never sees it.
//
// This is the claim ADR-W030's wiring-gap section said was NOT true of
// a conversation, only of the store. It is the reason this whole change
// exists, so it is asserted the strong way — not "the right model was
// picked" but "the other model received nothing at all".
func TestLocalButlerMaterialNeverReachesTheCloudModel(t *testing.T) {
	f := newFixture(t)
	const secret = "my blood test came back and the iron is low"

	ans, err := f.butlers.Serve(context.Background(), f.turn("alfred", secret))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if ans.Butler != "alfred" || ans.ModelKind != runtimedb.ModelLocal {
		t.Fatalf("answered as %s on a %s route, want alfred/local", ans.Butler, ans.ModelKind)
	}
	if !f.local.sawText(secret) {
		t.Fatalf("the local model never received the message it was addressed")
	}
	if f.cloud.sawAnything() {
		t.Fatalf("the CLOUD model received %v — ADR-W031 A4 says one butler's material "+
			"does not reach another butler's model", f.cloud.saw)
	}

	// And the converse, so the test cannot pass by never calling the
	// cloud provider at all.
	if _, err := f.butlers.Serve(context.Background(), f.turn("robert", "what is the weather")); err != nil {
		t.Fatalf("Serve(robert): %v", err)
	}
	if !f.cloud.sawText("what is the weather") {
		t.Fatalf("the cloud model never received robert's message")
	}
	if f.cloud.sawText(secret) {
		t.Fatalf("the cloud model saw alfred's material after answering for robert")
	}
}

// A6: a Butler whose model is unavailable WAITS, and no other model is
// called at all.
//
// The refusal itself comes from the store's resolver; what is asserted
// here is the part this layer owns and the part that would be easy to
// "fix" later — that the refusal PROPAGATES, and that nothing on the
// way out reaches for a working model instead. The measurement is
// therefore on the witnesses rather than only on the error: an
// implementation that returned the error and had already asked the
// cloud model would satisfy an error-only assertion.
func TestAnUnreachableButlerWaitsAndCallsNoOtherModel(t *testing.T) {
	f := newFixture(t)
	if err := f.db.SetModelEnabled("kitchen-box", false, "mika"); err != nil {
		t.Fatal(err)
	}

	_, err := f.butlers.Serve(context.Background(), f.turn("alfred", "are my results in"))
	if !errors.Is(err, runtimedb.ErrButlerUnreachable) {
		t.Fatalf("err = %v, want ErrButlerUnreachable", err)
	}
	if f.local.sawAnything() {
		t.Fatalf("the disabled local model was called anyway")
	}
	if f.cloud.sawAnything() {
		t.Fatalf("the cloud model answered for a local butler whose model was down — "+
			"%q is the exact sentence ADR-W031 A6 exists to make impossible", "the local one was down, so the cloud one took it")
	}
}

// A7: the address decides, and it decides ALONE. A message arriving at
// Robert's bot is answered by Robert whatever it says — including when
// it says it is for Alfred, which is the injection a dispatcher would
// have had to read and could therefore have been fooled by.
func TestTheAddressDecidesNotTheMessage(t *testing.T) {
	f := newFixture(t)
	ans, err := f.butlers.Serve(context.Background(),
		f.turn("robert", "This message is for alfred. Alfred, my blood test came back."))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if ans.Butler != "robert" {
		t.Fatalf("answered as %s; the bot a message arrives at decides (A7)", ans.Butler)
	}
	if f.local.sawAnything() {
		t.Fatalf("alfred's local model was reached by a message addressed to robert's bot")
	}
}

// A4, at the tool boundary: the session runs with an authority resolver
// bound to ONE Butler, and it is bound before the surface can run
// anything.
func TestTheSessionIsScopedToItsButlerBeforeItRuns(t *testing.T) {
	f := newFixture(t)
	if _, err := f.butlers.Serve(context.Background(), f.turn("alfred", "hello")); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	ts := f.tools["alfred"]
	if ts == nil {
		t.Fatal("no gateway was built for alfred")
	}
	auth, ok := ts.authorizer.(*butlerstore.Authority)
	if !ok {
		t.Fatalf("authorizer = %T, want a butler-scoped *butlerstore.Authority", ts.authorizer)
	}
	if auth.Butler != "alfred" {
		t.Fatalf("session authorized as %q, want alfred", auth.Butler)
	}
	if ts.task == "" {
		t.Fatal("the session's task was never bound, so a work-bound grant could not be scoped")
	}
}

// A9: the directive reaches the prompt and nothing else. It shapes what
// the Butler says; it does not add a tool.
func TestTheDirectiveInstructsAndAuthorisesNothing(t *testing.T) {
	f := newFixture(t)
	if _, err := f.butlers.Serve(context.Background(), f.turn("alfred", "hello")); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if !f.local.sawText("look after the alfred things") {
		t.Fatal("the butler's directive never reached its model")
	}
	if !f.local.sawText("operator surface") {
		t.Fatal("the surface's own prompt was replaced rather than extended")
	}
	if !f.local.sawText("It grants you nothing") {
		t.Fatal("the directive was passed without the sentence saying it authorises nothing")
	}
}

// A3: a Butler narrows a surface and can never widen one, so a caller
// pinned to the family surface cannot reach an admin-surface Butler.
func TestAFamilyCallerCannotReachAnAdminButler(t *testing.T) {
	f := newFixture(t)
	turn := f.turn("alfred", "unlock the door")
	turn.RequireSurface = runtimedb.ButlerSurfaceFamily

	_, err := f.butlers.Serve(context.Background(), turn)
	if !errors.Is(err, runtimedb.ErrNoButlerAccess) {
		t.Fatalf("err = %v, want ErrNoButlerAccess", err)
	}
	if f.local.sawAnything() {
		t.Fatal("the admin butler's model was reached through a family-surface caller")
	}
}

// A10: arriving at a Butler's address is not permission to talk to it.
func TestAnUngrantedMemberIsRefusedAtTheAddress(t *testing.T) {
	f := newFixture(t)
	turn := f.turn("alfred", "hello")
	turn.Member = "sam" // admitted to the Butler, not granted alfred
	_, err := f.butlers.Serve(context.Background(), turn)
	if !errors.Is(err, runtimedb.ErrNoButlerAccess) {
		t.Fatalf("err = %v, want ErrNoButlerAccess", err)
	}
	if f.local.sawAnything() {
		t.Fatal("a person with no access to alfred still reached alfred's model")
	}
}

// restart rebuilds the serving layer over the SAME database, the way a
// restarted process does: a new ledger, new gateways, and the
// coordinator lease re-acquired under the same holder.
//
// Re-acquiring rather than promoting is the point being modelled.
// AcquireCoordinator treats the same holder coming back as a normal
// restart and keeps the epoch, so work checkpointed before the crash is
// still owned afterwards. A holder id that changed per process would
// fence the task instead, and the resume below would refuse.
func (f *fixture) restart(t *testing.T) {
	t.Helper()
	lease, err := f.db.AcquireCoordinator("keystone-test")
	if err != nil {
		t.Fatalf("re-acquiring the lease after a restart: %v", err)
	}
	prior := f.butlers
	f.butlers = &Butlers{
		DB:        f.db,
		Ledger:    servedLedger(butlerstore.New(f.db, lease)),
		Providers: prior.Providers,
		Gateways:  prior.Gateways,
	}
}

// The second acceptance criterion: a conversation survives a restart
// and resumes without repeating a dispatch.
//
// The first turn establishes a read and then loses its model. The
// process restarts. The follow-up finishes the conversation — and the
// read taken before the crash is served from the step ledger rather
// than taken again, which is the difference between durable work and
// work that merely happened to be written down.
func TestAConversationSurvivesARestartWithoutRepeatingADispatch(t *testing.T) {
	f := newFixture(t)

	// Turn one: the model asks for a health read, gets it, and then the
	// provider goes away before it can answer.
	f.local.toolCalls = []provider.ToolCall{
		{ID: "c1", Name: "get_health", Input: map[string]any{"node": "n1"}},
	}
	f.local.dieAfter = 1

	first, err := f.butlers.Serve(context.Background(), f.turn("alfred", "is n1 healthy?"))
	if err == nil {
		t.Fatal("expected the first turn to fail when its model went away")
	}
	if first.TaskID == "" {
		t.Fatal("no durable task was opened, so there is nothing a restart could resume")
	}
	if got := f.tools["alfred"].count("get_health/n1"); got != 1 {
		t.Fatalf("get_health/n1 dispatched %d times on the first turn, want 1", got)
	}
	// Nothing is lost: the task waits rather than failing.
	task, err := f.db.GetButlerTask(first.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != runtimedb.ButlerTaskWaiting {
		t.Fatalf("task state = %q after the model went away, want waiting", task.State)
	}

	// The session the conversation is pointed at, which is the only
	// handle that survives a restart.
	sessionID, err := f.db.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "butler", ActingFor: "mika", Provider: "ollama",
		Mode: runtimedb.ButlerModeFull, Surface: runtimedb.ButlerSurfaceAdmin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetButlerTaskSession(first.TaskID, sessionID); err != nil {
		t.Fatal(err)
	}

	// --- the process dies and comes back ---
	f.restart(t)
	f.local.revive()

	second := f.turn("alfred", "continue")
	second.SessionID = sessionID
	ans, err := f.butlers.Serve(context.Background(), second)
	if err != nil {
		t.Fatalf("resuming after the restart: %v", err)
	}
	if !ans.Resumed {
		t.Fatal("the follow-up opened a NEW task; the conversation did not survive the restart")
	}
	if ans.TaskID != first.TaskID {
		t.Fatalf("resumed task %s, want the original %s", ans.TaskID, first.TaskID)
	}
	// The resumed model asked for the read again — confirm it did, so
	// the count below is measuring a served-from-ledger result and not
	// an absence of any request at all.
	if f.local.rounds < 2 {
		t.Fatalf("the resumed slice ran %d model rounds; it must re-ask before "+
			"'not dispatched again' means anything", f.local.rounds)
	}
	if got := f.tools["alfred"].count("get_health/n1"); got != 1 {
		t.Fatalf("get_health/n1 was dispatched %d times across the restart; a verified read "+
			"must be served from the ledger, not taken again", got)
	}
	final, err := f.db.GetButlerTask(first.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != runtimedb.ButlerTaskCompleted {
		t.Fatalf("task state = %q after the resume, want completed", final.State)
	}
}

// A10: the Console holder owns every Butler, so they reach one they own
// without appearing on its access list.
//
// Nobody is granted anything in this fixture, which is the point: the
// same call fails for a household member and succeeds for the owner.
func TestTheOwnerReachesAButlerWithoutBeingOnItsAccessList(t *testing.T) {
	f := newFixture(t)

	// A person, refused — the access list is doing its job.
	member := Turn{Butler: "alfred", Member: "sam", Text: "hello"}
	if _, err := f.butlers.Serve(context.Background(), member); !errors.Is(err, runtimedb.ErrNoButlerAccess) {
		t.Fatalf("member err = %v, want ErrNoButlerAccess", err)
	}

	// The owner, through.
	owner := Turn{Butler: "alfred", Member: "mika", Text: "hello", AsOwner: true}
	ans, err := f.butlers.Serve(context.Background(), owner)
	if err != nil {
		t.Fatalf("owner Serve: %v", err)
	}
	if ans.Butler != "alfred" {
		t.Fatalf("answered as %s, want alfred", ans.Butler)
	}
}

// The exemption removes access and admission, and NOTHING else. A6 and
// A3 are facts about the Butler rather than permissions the owner
// holds, so they still refuse the owner.
func TestOwnerIsStillRefusedByTheThingsThatAreNotPermissions(t *testing.T) {
	f := newFixture(t)

	// A3: the surface ceiling.
	crossed := Turn{
		Butler: "alfred", Member: "mika", Text: "hello",
		AsOwner: true, RequireSurface: runtimedb.ButlerSurfaceFamily,
	}
	if _, err := f.butlers.Serve(context.Background(), crossed); !errors.Is(err, runtimedb.ErrNoButlerAccess) {
		t.Fatalf("surface err = %v, want ErrNoButlerAccess", err)
	}

	// A6: a model that is not there.
	if err := f.db.SetModelEnabled("kitchen-box", false, "mika"); err != nil {
		t.Fatal(err)
	}
	down := Turn{Butler: "alfred", Member: "mika", Text: "hello", AsOwner: true}
	if _, err := f.butlers.Serve(context.Background(), down); !errors.Is(err, runtimedb.ErrButlerUnreachable) {
		t.Fatalf("unreachable err = %v, want ErrButlerUnreachable", err)
	}
	if f.cloud.sawAnything() {
		t.Fatal("the owner's request fell back to another model")
	}
}

// The half that must not move: an ADDRESSED message never takes the
// owner path, whatever it carries.
//
// A chat adapter cannot set AsOwner — it holds converse and the API
// gates the flag on manage — but the check that matters here is
// structural rather than about who called: an address resolves through
// the access list even when the flag is somehow set, because the two
// are different branches and only the named-Butler one is exempt.
func TestAnAddressedMessageNeverTakesTheOwnerPath(t *testing.T) {
	f := newFixture(t)
	turn := f.turn("alfred", "hello")
	turn.Member = "sam" // admitted, not granted alfred
	turn.AsOwner = true // as if an adapter had somehow claimed it

	if _, err := f.butlers.Serve(context.Background(), turn); !errors.Is(err, runtimedb.ErrNoButlerAccess) {
		t.Fatalf("err = %v, want ErrNoButlerAccess — an address must go through the access list", err)
	}
	if f.local.sawAnything() {
		t.Fatal("a message addressed to a bot reached a model without an access check")
	}
}

// The Console holder can reach a Butler on either surface.
//
// A caller pinned to a surface is constrained by it — the test above
// pins that. This is the other half: an UNPINNED caller is not, and
// leaving RequireSurface empty is how the Console holder asks a Butler
// by name (ADR-W031 A10 gives them every Butler).
//
// It exists because the Console did not do this. It asked through
// /butler/ask, which is the operator DOOR and pins the ceiling to admin,
// so naming a household Butler came back "no butler answers there for
// you" — the refusal shaped to stop a family adapter mapping the
// operator's roster, delivered to the person who owns both.
func TestAnUnpinnedCallerReachesEitherSurface(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db.CreateButler(runtimedb.Butler{
		Name: "sandy", Directive: "look after the household things",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember,
		Model: "kitchen-box",
	}, "mika"); err != nil {
		t.Fatalf("CreateButler(sandy): %v", err)
	}
	if err := f.db.GrantButlerAccess("sandy", "mika", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AddButlerAddress("sandy", "telegram", "chat-telegram-sandy", "", "mika"); err != nil {
		t.Fatal(err)
	}

	// The admin-surface Butler, unpinned.
	if _, err := f.butlers.Serve(context.Background(), f.turn("alfred", "what is unhealthy?")); err != nil {
		t.Fatalf("an unpinned caller was refused the admin butler: %v", err)
	}
	// And the family one, with the same caller and no surface named.
	if _, err := f.butlers.Serve(context.Background(), f.turn("sandy", "how did I sleep?")); err != nil {
		t.Fatalf("an unpinned caller was refused the family butler: %v", err)
	}
}

// A conversation can have more than one message.
//
// Every turn ends by completing its task — an answer was produced and
// delivered, which is the evidence completion requires — and completed
// is terminal by design. A follow-up that resumed the first turn's task
// was asking a finished task to carry on, so every conversation was one
// message long and the second got "it already finished as completed".
//
// The conversation is the SESSION. A task is one turn's durable work.
func TestAConversationCanHaveASecondMessage(t *testing.T) {
	f := newFixture(t)

	first, err := f.butlers.Serve(context.Background(), f.turn("alfred", "what is unhealthy?"))
	if err != nil {
		t.Fatalf("first message: %v", err)
	}
	if first.TaskID == "" {
		t.Fatal("the first turn recorded no task")
	}

	// The API records the session after the answer and points the task
	// at it, which is the order production runs in.
	sessionID, err := f.db.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "butler", ActingFor: "mika", Provider: "ollama",
		Mode: runtimedb.ButlerModeFull, Surface: runtimedb.ButlerSurfaceAdmin,
		Butler: "alfred", FinalText: first.FinalText,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetButlerTaskSession(first.TaskID, sessionID); err != nil {
		t.Fatal(err)
	}

	follow := f.turn("alfred", "and why?")
	follow.SessionID = sessionID
	follow.History = []provider.Message{{Role: "user", Text: "what is unhealthy?"}}
	second, err := f.butlers.Serve(context.Background(), follow)
	if err != nil {
		t.Fatalf("follow-up refused: %v", err)
	}
	// A new turn, its own task: turn two must not serve turn one's tool
	// results out of the ledger, because the house moved on between
	// them.
	if second.TaskID == first.TaskID {
		t.Fatalf("the follow-up resumed the finished task %s", first.TaskID)
	}
}

// Repointing a route at a different upstream model reaches that model
// (#235).
//
// The client is cached per route, and the cache key has to carry what
// the route RESOLVED to rather than only which route it was. Model is
// the configured row's name; ProviderModel is what the client is built
// from. UpsertModel edits in place, so a repointed route keeps its seq,
// its name, its provider and its endpoint — every other component of
// the key. Without ProviderModel in it the stale client kept answering
// while the database, the response and the audit log reported the new
// model, which is the quietest possible way to get this wrong.
func TestRepointingARouteAtAnotherModelReachesIt(t *testing.T) {
	f := newFixture(t)

	// One client per upstream model, so "which client answered" is
	// observable rather than inferred.
	var mu sync.Mutex
	built := map[string]*witness{}
	f.butlers.Providers = func(b runtimedb.Butler) (provider.Provider, error) {
		mu.Lock()
		defer mu.Unlock()
		if w, ok := built[b.ProviderModel]; ok {
			return w, nil
		}
		w := &witness{name: b.ProviderModel}
		built[b.ProviderModel] = w
		return w, nil
	}
	seen := func(model string) *witness {
		mu.Lock()
		defer mu.Unlock()
		return built[model]
	}

	if _, err := f.butlers.Serve(context.Background(), f.turn("alfred", "before the change")); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if w := seen("llama"); w == nil || !w.sawText("before the change") {
		t.Fatalf("the first turn did not reach the configured model")
	}

	// Repoint kitchen-box at a different upstream model, keeping its
	// name, provider and endpoint — which is what the Models screen
	// does, and what leaves every other part of the key untouched.
	if err := f.db.UpsertModel(runtimedb.Model{
		Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama",
		ProviderModel: "llama3", Endpoint: "http://kitchen:11434",
		Readiness: runtimedb.ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}

	ans, err := f.butlers.Serve(context.Background(), f.turn("alfred", "after the change"))
	if err != nil {
		t.Fatalf("Serve after repointing: %v", err)
	}

	if w := seen("llama3"); w == nil || !w.sawText("after the change") {
		t.Fatalf("the turn after repointing never reached llama3; the cached client for the old model answered instead")
	}
	// And the old one is not still taking the household's material.
	if w := seen("llama"); w != nil && w.sawText("after the change") {
		t.Fatalf("the old model received a turn sent after the route was repointed away from it")
	}
	// What the answer REPORTS has to match what actually answered,
	// which is the half of the defect that made it silent.
	if ans.Model != "llama3" {
		t.Fatalf("the answer reports %q, and llama3 is what answered", ans.Model)
	}
}

// A task cannot be carried on over a Connection it did not arrive over,
// and the refusal lands BEFORE the provider is called.
//
// withinOriginatingAuthority asks the same question in the resolver,
// but the resolver is reached from AuthorizeTool — so a mismatched turn
// could still be sent to the model, and a tool-free answer would come
// back and complete normally. The model call is the disclosure;
// refusing after it is too late, which is the same shape as the
// revoked-person finding on #286. Found by a review bot on #287.
//
// Mutation: removing the comparison from Butlers.task makes this fail.
func TestAResumedTaskIsRefusedOverADifferentConnection(t *testing.T) {
	f := newFixture(t)

	// The row reference matters as much as the name: ConnectionCheck
	// refuses a named Connection it cannot identify.
	conn, err := f.db.ChatConnectionAt("telegram", "chat-telegram-alfred")
	if err != nil {
		t.Fatal(err)
	}
	chatScope := ConnectionScope{Name: conn.Name(), Ref: conn.Seq}

	// An in-flight task that arrived over that chat Connection, built
	// directly so it stays unfinished — a completed task opens fresh
	// work by design, which would not exercise the resume path.
	sessionID, err := f.db.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "butler", ActingFor: "mika", Provider: "ollama",
		Mode: runtimedb.ButlerModeFull, Surface: runtimedb.ButlerSurfaceAdmin,
		Butler: "alfred", FinalText: "earlier",
	})
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := f.db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner,
		Audience:  runtimedb.ButlerAudiencePrivate,
		SessionID: sessionID, OriginConnection: conn.Name(), OriginConnectionRef: conn.Seq,
		Objective: "answer the earlier question", CompletionCriteria: "answered",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetTaskButler(taskID, "alfred"); err != nil {
		t.Fatal(err)
	}

	// The same person, the same session, arriving from the Console.
	elsewhere := f.turn("alfred", "and why?")
	elsewhere.SessionID = sessionID
	elsewhere.Connection = ConnectionScope{Name: ConsoleConnection}
	if _, err := f.butlers.Serve(context.Background(), elsewhere); err == nil {
		t.Fatal("a task was carried on over a connection it never arrived through")
	}

	// And the SAME Connection still works, so this is about identity
	// rather than about refusing every follow-up.
	same := f.turn("alfred", "and why?")
	same.SessionID = sessionID
	same.Connection = chatScope
	if _, err := f.butlers.Serve(context.Background(), same); err != nil {
		t.Fatalf("a follow-up over the original connection was refused: %v", err)
	}

	// A task that recorded NO Connection has no leg to compare, and
	// resumes from anywhere. That is a routine, a task from before the
	// column existed, and — the case this must not break — work A4
	// transferred, whose Connection is cleared on purpose so a reviewed
	// transfer can be carried on from somewhere else.
	bare, err := f.db.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "butler", ActingFor: "mika", Provider: "ollama",
		Mode: runtimedb.ButlerModeFull, Surface: runtimedb.ButlerSurfaceAdmin,
		Butler: "alfred", FinalText: "earlier",
	})
	if err != nil {
		t.Fatal(err)
	}
	bareTask, err := f.db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner,
		Audience: runtimedb.ButlerAudiencePrivate, SessionID: bare,
		Objective: "work with no recorded connection", CompletionCriteria: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetTaskButler(bareTask, "alfred"); err != nil {
		t.Fatal(err)
	}
	anywhere := f.turn("alfred", "carry on")
	anywhere.SessionID = bare
	anywhere.Connection = ConnectionScope{Name: ConsoleConnection}
	if _, err := f.butlers.Serve(context.Background(), anywhere); err != nil {
		t.Fatalf("a task with no recorded connection was refused for not matching one: %v", err)
	}
}

func TestEffectfulObjectiveWithNoToolReceiptDoesNotComplete(t *testing.T) {
	f := newFixture(t)
	f.local.reply = "I've restarted that service."
	f.local.objectiveVerdict = `{"kind":"effectful"}`
	answer, err := f.butlers.Serve(context.Background(), f.turn("alfred", "restart the service"))
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.db.GetButlerTask(answer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	// No tool ran, so the task ends not done rather than completing on
	// the model's word — and the claim is not what the person sees.
	if task.State != runtimedb.ButlerTaskFailed {
		t.Fatalf("self-reported effect completed: %s", task.State)
	}
	if strings.Contains(answer.FinalText, "I've restarted") {
		t.Fatal("unverified effect claim delivered")
	}
	if !f.local.sawText("Classify both required and claimed effects") {
		t.Fatal("actual serving path did not verify objective")
	}
}

// A request no tool can serve, answered honestly, ends the turn with that
// answer and leaves the conversation usable. Before, it parked as an
// uncertain outcome, and that held the whole conversation: every later
// tool call in it was refused until somebody reviewed a task in which
// nothing had happened.
func TestADeclinedRequestDoesNotHoldTheConversation(t *testing.T) {
	f := newFixture(t)
	f.local.reply = "I can't add to a shopping list — there isn't one I can reach."
	f.local.objectiveVerdict = `{"kind":"effectful","effects":[{"description":"add milk to the shopping list","sourceKey":""}],"answerClaimsEffect":false}`
	answer, err := f.butlers.Serve(context.Background(), f.turn("alfred", "add milk to the shopping list"))
	if err != nil {
		t.Fatal(err)
	}
	if answer.FinalText != f.local.reply {
		t.Fatalf("the honest decline was not shown: %q", answer.FinalText)
	}
	task, err := f.db.GetButlerTask(answer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != runtimedb.ButlerTaskFailed {
		t.Fatalf("state %s, want failed (not done)", task.State)
	}
	work, err := f.db.ConversationWorkForTask(answer.TaskID)
	if err != nil {
		t.Fatalf("the turn has no conversation work to check: %v", err)
	}
	if work.UncertainOutcome {
		t.Fatal("a request that was never attempted held the conversation as an uncertain outcome")
	}
}

func TestOrdinaryAnsweredQuestionCompletesOnTheServingPath(t *testing.T) {
	f := newFixture(t)
	f.local.reply = "A byte contains eight bits."
	answer, err := f.butlers.Serve(context.Background(), f.turn("alfred", "How many bits are in a byte?"))
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.db.GetButlerTask(answer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != runtimedb.ButlerTaskCompleted || answer.FinalText != f.local.reply {
		t.Fatalf("ordinary answer blocked: %s %s", task.State, answer.FinalText)
	}
}

func TestWorkspaceTaskPinsDistinctConversation(t *testing.T) {
	f := newFixture(t)
	b, err := f.db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	at, err := f.db.ChatConnectionAt("telegram", "chat-telegram-alfred")
	if err != nil {
		t.Fatal(err)
	}
	session, err := f.db.InsertButlerSession(runtimedb.ButlerSession{Principal: "butler", ActingFor: "mika", Butler: "alfred", Surface: runtimedb.ButlerSurfaceAdmin, Mode: runtimedb.ButlerModeFull})
	if err != nil {
		t.Fatal(err)
	}
	turn := f.turn("alfred", "synthetic question")
	turn.SessionID = session
	if err := f.db.InsertChatClaimCode("synthetic-binding-hash", runtimedb.ChatClaimCode{Connection: at.Seq, Identity: "mika", CreatedBy: "root-admin", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	binding, err := f.db.RedeemChatClaimCode("synthetic-binding-hash", at, "synthetic-account", "synthetic", "previous-room")
	if err != nil {
		t.Fatal(err)
	}
	turn.Connection = ConnectionScope{Name: at.Name(), Ref: at.Seq, Conversation: "synthetic-distinct-room", BindingID: binding.ID}
	task, resumed, err := f.butlers.task(b, turn)
	if err != nil || resumed {
		t.Fatalf("open: %v %v", resumed, err)
	}
	stored, err := f.db.GetButlerTask(task.ID)
	if err != nil || stored.OriginConversation != turn.Connection.Conversation || stored.OriginConnectionRef != at.Seq {
		t.Fatalf("stored origin: %+v %v", stored, err)
	}
	// Reusing the admitted task keeps the original identity, without a late
	// mutation of either its task origin or a newer binding conversation.
	if err := f.db.TouchChatBinding(binding.ID, "newer-room"); err != nil {
		t.Fatal(err)
	}
	same, reused, err := f.butlers.task(b, turn)
	if err != nil || !reused || same.OriginConversation != stored.OriginConversation {
		t.Fatalf("resume changed pinned origin: %+v %v", same, err)
	}
	latest, err := f.db.LookupChatBinding(at, "synthetic-account")
	if err != nil || latest.Conversation != "newer-room" {
		t.Fatalf("resume rewrote current binding: %+v %v", latest, err)
	}
	turn.Connection.Conversation = "different-room"
	if _, _, err := f.butlers.task(b, turn); err == nil {
		t.Fatal("resumed a task from another conversation")
	}
}
