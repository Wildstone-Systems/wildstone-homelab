package agentgateway

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

// The fixtures butlerlive_test.go and butlerstore's tests build, joined
// up: a migrated database with two admitted people, an admin-surface
// Butler and a family-surface Butler on a ready route, a real Statefile
// behind the operator surface and real memory seams behind the family
// one, and a coordinator-fenced ledger — the same things Serve has,
// minus the provider.

var migrated = dbtemplate.New(func(path string) error {
	db, err := runtimedb.Open(path)
	if err != nil {
		return err
	}
	return db.Close()
})

// counting wraps a real surface and counts what was REALLY dispatched,
// forwarding the scoping calls so the gateway can bind it exactly as
// it binds the surface underneath.
type counting struct {
	butler.Toolset
	mu    sync.Mutex
	calls map[string]int
	// during, when set, runs inside Execute — a hook for what happens
	// while a tool is in flight.
	during func()
}

func (c *counting) Execute(ctx context.Context, name string, input map[string]any, readOnly bool) (butler.ToolOutcome, error) {
	c.mu.Lock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[name]++
	c.mu.Unlock()
	if c.during != nil {
		c.during()
	}
	return c.Toolset.Execute(ctx, name, input, readOnly)
}

func (c *counting) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[name]
}

func (c *counting) SetAuthorizer(a butler.ToolAuthorizer) {
	c.Toolset.(butlerlive.Scoped).SetAuthorizer(a)
}
func (c *counting) SetSessionTask(id string) { c.Toolset.(butlerlive.Scoped).SetSessionTask(id) }
func (c *counting) SetCaller(kind butler.CallerKind, person, connection string) {
	c.Toolset.(butlerlive.Scoped).SetCaller(kind, person, connection)
}

// MemoryPreamble forwards the family surface's preamble so the system
// prompt assembled through the wrapper is the one Serve assembles.
func (c *counting) MemoryPreamble(ctx context.Context) string {
	if p, ok := c.Toolset.(interface{ MemoryPreamble(context.Context) string }); ok {
		return p.MemoryPreamble(ctx)
	}
	return ""
}

type fixture struct {
	db     *runtimedb.DB
	ledger *butlerstore.Ledger
	store  *statefile.Store
	g      *Gateway
	// surfaces keeps one wrapped surface per Butler for the lifetime
	// of the test, as butlerlive_test does, so a dispatch count
	// survives the per-call rebuild.
	mu       sync.Mutex
	surfaces map[string]*counting
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := runtimedb.Open(migrated.Path(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.UpsertModel(runtimedb.Model{
		Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Endpoint: "http://kitchen:11434", Readiness: runtimedb.ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatalf("UpsertModel: %v", err)
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
	for _, b := range []runtimedb.Butler{
		{Name: "alfred", Directive: "look after the machines", Surface: runtimedb.ButlerSurfaceAdmin,
			AgentClass: runtimedb.AgentClassOwner, Model: "kitchen-box"},
		{Name: "joan", Directive: "look after the family", Surface: runtimedb.ButlerSurfaceFamily,
			AgentClass: runtimedb.AgentClassMember, Model: "kitchen-box"},
	} {
		seq, err := db.CreateButler(b, "mika")
		if err != nil {
			t.Fatalf("CreateButler(%s): %v", b.Name, err)
		}
		// The native runtime record the API's createButler writes
		// (ensureNativeRuntime); runtimedb's CreateButler does not.
		if err := db.SetButlerRuntime(seq, runtimedb.ButlerRuntime{Type: agentapi.RuntimeNative}, "mika"); err != nil {
			t.Fatalf("SetButlerRuntime(%s): %v", b.Name, err)
		}
		for _, who := range []string{"mika", "sam"} {
			if err := db.GrantButlerAccess(b.Name, who, "mika"); err != nil {
				t.Fatal(err)
			}
		}
	}
	store, err := statefile.New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatalf("statefile.New: %v", err)
	}
	lease, err := db.AcquireCoordinator("keystone-test")
	if err != nil {
		t.Fatalf("AcquireCoordinator: %v", err)
	}
	// All the legs of §10.1's derived Task authority on, as cmd/keystone
	// wires the ledger that serves real conversations (and as
	// butlerlive_test's servedLedger does): a task that recorded a
	// Connection is refused by a ledger that cannot confirm it.
	ledger := butlerstore.New(db, lease)
	ledger.DerivedAuthorityChecks()
	f := &fixture{db: db, ledger: ledger, store: store, surfaces: map[string]*counting{}}
	f.g = &Gateway{
		DB:     db,
		Ledger: f.ledger,
		Clock:  func() string { return "It is now Thursday 24 September 2026, 09:00 in the household's timezone." },
		Gateways: butlerlive.GatewayFunc(func(b runtimedb.Butler, turn butlerlive.Turn) (butler.Toolset, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if c, ok := f.surfaces[b.Name]; ok {
				return c, nil
			}
			c := &counting{Toolset: f.surfaceFor(b, turn)}
			f.surfaces[b.Name] = c
			return c, nil
		}),
	}
	return f
}

// surfaceFor is cmd/keystone's gateway factory in miniature: the
// operator surface over the Statefile, the family surface over the
// memory seams bound to the turn.
func (f *fixture) surfaceFor(b runtimedb.Butler, turn butlerlive.Turn) butler.Toolset {
	switch b.Surface {
	case runtimedb.ButlerSurfaceFamily:
		seams := butler.HouseholdSeams{Memory: butlerstore.MemorySeams(f.db, b.Name, turn.Member, turn.MemoryDestination(), nil)}
		if turn.Group {
			return butler.NewHouseholdGroupGateway(turn.Member, seams)
		}
		return butler.NewHouseholdGateway(turn.Member, seams)
	default:
		return butler.NewToolGateway(f.store, nil)
	}
}

func (f *fixture) surface(name string) *counting {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.surfaces[name]
}

// openTask opens a conversation task as Serve does (serve.go:363-399)
// and issues its handle.
func (f *fixture) openTask(t *testing.T, butlerName, owner string) (string, agentapi.Handle) {
	t.Helper()
	b, err := f.db.GetButler(butlerName)
	if err != nil {
		t.Fatalf("GetButler(%s): %v", butlerName, err)
	}
	id, err := f.db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: owner, Owner: owner,
		Surface: b.Surface, AgentClass: b.AgentClass, Audience: runtimedb.ButlerAudiencePrivate,
		OriginConnection: butlerlive.ConsoleConnection,
		Objective:        "answer " + owner + ", asked of " + butlerName,
		// Informational, as the runner's own verifyObjective classifies
		// a question; Serve leaves it for that call to decide.
		ObjectiveKind: runtimedb.ObjectiveInformational,
		Retention:     runtimedb.ButlerRetentionConversation,
	})
	if err != nil {
		t.Fatalf("InsertButlerTask: %v", err)
	}
	if err := f.db.SetTaskButler(id, butlerName); err != nil {
		t.Fatalf("SetTaskButler: %v", err)
	}
	h, err := f.ledger.IssueHandle(id)
	if err != nil {
		t.Fatalf("IssueHandle: %v", err)
	}
	return id, h
}

// bindRuntime mints a runtime principal bound to a Butler, as Phase 1
// does at creation.
func (f *fixture) bindRuntime(t *testing.T, principal, butlerName string) {
	t.Helper()
	b, err := f.db.GetButler(butlerName)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreatePrincipal(principal, runtimedb.RoleButler, ""); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if err := f.db.BindRuntimePrincipal(principal, b.Seq); err != nil {
		t.Fatalf("BindRuntimePrincipal: %v", err)
	}
}

// scripted is a provider that asks for the given tool calls on its
// first round and answers with text on the next.
type scripted struct {
	toolCalls []provider.ToolCall
	rounds    int
}

func (s *scripted) Name() string { return "ollama" }

func (s *scripted) Chat(_ context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	s.rounds++
	if s.rounds == 1 && len(s.toolCalls) > 0 {
		return &provider.ChatResponse{ToolCalls: s.toolCalls, StopReason: "tool_use"}, nil
	}
	return &provider.ChatResponse{Text: "done"}, nil
}

func toolNames(specs []agentapi.ToolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

func availableNames(m butler.CapabilityManifest) []string {
	var out []string
	for _, tool := range m.Tools {
		if tool.Available {
			out = append(out, tool.Name)
		}
	}
	return out
}
