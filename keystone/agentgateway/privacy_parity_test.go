package agentgateway_test

// butlerlive/privacy_test.go:32's own TestLocalOnlyNeverFallsBackToCloud
// pins a refusal on the closure path (UseAgentGateway=false, its
// fixture's default) that must hold identically once the provider
// wrapper is replaced by the gateway-side disclosure filter (ADR-W039
// §7, #345): a Butler whose local model is unavailable
// fails rather than silently reaching a cloud provider with local-only
// content, however the turn is wired underneath. This is that SAME
// scenario driven with UseAgentGateway=true, in an external test
// package for the reason switch_parity_test.go's own doc comment gives
// — keystone/agentgateway imports keystone/butlerlive, so neither
// package's own tests can import the other.
//
// butlerlive/privacy_test.go itself is untouched and keeps proving the
// identical refusal on the closure path.

import (
	"context"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentgateway"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// parityWitness is a minimal provider.Provider recording whether it was
// ever called — this package cannot reach butlerlive_test.go's own
// unexported witness type from an external test package.
type parityWitness struct{ called bool }

func (w *parityWitness) Name() string { return "witness" }
func (w *parityWitness) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	w.called = true
	return &provider.ChatResponse{Text: "should never be reached"}, nil
}

func TestLocalOnlyNeverFallsBackToCloudUnderTheGatewayPath(t *testing.T) {
	db, err := runtimedb.Open(t.TempDir() + "/keystone.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
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
	seq, err := db.CreateButler(runtimedb.Butler{
		Name: "memory-agent", Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "kitchen-box",
	}, "mika")
	if err != nil {
		t.Fatalf("CreateButler: %v", err)
	}
	if err := db.SetButlerRuntime(seq, runtimedb.ButlerRuntime{Type: agentapi.RuntimeNative}, "mika"); err != nil {
		t.Fatalf("SetButlerRuntime: %v", err)
	}
	for _, who := range []string{"sam", "mika"} {
		if err := db.GrantButlerAccess("memory-agent", who, "mika"); err != nil {
			t.Fatal(err)
		}
	}

	memSeq, err := db.RecordMemory(runtimedb.MemoryRecord{
		Scope: runtimedb.MemoryScopePersonal, Kind: runtimedb.MemoryPreference, Content: "jasmine private canary",
		Owner: "sam", Author: "sam", Butler: "memory-agent", Audience: runtimedb.ButlerAudiencePrivate,
		PermissionLabel: "household", SourceKind: "conversation", SourceID: "fixture",
	})
	if err != nil {
		t.Fatalf("RecordMemory: %v", err)
	}
	if err := db.SetMemoryPrivacy("sam", memSeq, runtimedb.PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatalf("SetMemoryPrivacy: %v", err)
	}
	if err := db.SetModelEnabled("kitchen-box", false, "mika"); err != nil {
		t.Fatalf("SetModelEnabled: %v", err)
	}

	lease, err := db.AcquireCoordinator("keystone-test")
	if err != nil {
		t.Fatalf("AcquireCoordinator: %v", err)
	}
	ledger := butlerstore.New(db, lease)
	ledger.DerivedAuthorityChecks()
	gateways := butlerlive.GatewayFunc(func(b runtimedb.Butler, turn butlerlive.Turn) (butler.Toolset, error) {
		seams := butler.HouseholdSeams{Memory: butlerstore.MemorySeams(db, b.Name, turn.Member, turn.MemoryDestination(), nil)}
		return butler.NewHouseholdGateway(turn.Member, seams), nil
	})
	cloud := &parityWitness{}
	bs := &butlerlive.Butlers{
		DB: db, Ledger: ledger, Gateways: gateways,
		Providers:       func(runtimedb.Butler) (provider.Provider, error) { return cloud, nil },
		UseAgentGateway: true,
	}
	gw := &agentgateway.Gateway{DB: db, Ledger: ledger, Gateways: gateways, Clock: bs.ClockLine}
	bs.AgentClient = agentgateway.InProcess(gw)
	bs.WithPrincipal = func(ctx context.Context, name string) context.Context {
		rt, err := db.GetButlerRuntimeByName(name)
		if err != nil || rt.Principal == "" {
			return ctx
		}
		return agentgateway.WithPrincipal(ctx, rt.Principal)
	}

	_, err = bs.Serve(context.Background(), butlerlive.Turn{Butler: "memory-agent", Member: "sam", Text: "Hello"})
	if err == nil {
		t.Fatal("unavailable local model silently degraded under the gateway path")
	}
	if cloud.called {
		t.Fatal("cloud fallback received local-only content under the gateway path")
	}
}
