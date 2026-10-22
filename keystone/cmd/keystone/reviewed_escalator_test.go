package main

// ADR-W039 Phase 4 (C5/C6, #340/#341): an escalation is delivered to
// the assigned Agent's runtime as a tool-free run request — recorded
// regardless of runtime type — and served in-process only for native.

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/automation"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// fixedProvider answers every Chat with the same text, never a tool
// call — a Phraser's own "no tools at all" contract needs nothing more.
type fixedProvider struct{ reply string }

func (p *fixedProvider) Name() string { return "fixed-test-provider" }
func (p *fixedProvider) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Text: p.reply}, nil
}

func escalatorFixture(t *testing.T) (*runtimedb.DB, runtimedb.Butler) {
	t.Helper()
	db, err := runtimedb.Open(migrated.Into(t, filepath.Join(t.TempDir(), "keystone.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.UpsertModel(runtimedb.Model{
		Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Readiness: runtimedb.ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateButler(runtimedb.Butler{
		Name: "alfred", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Model: "kitchen-box",
	}, "mika"); err != nil {
		t.Fatal(err)
	}
	b, err := db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	return db, b
}

func TestReviewedEscalatorServesNativeInProcessAndRecordsADelivery(t *testing.T) {
	db, b := escalatorFixture(t)
	if err := db.SetButlerRuntime(b.Seq, runtimedb.ButlerRuntime{Type: agentapi.RuntimeNative}, "mika"); err != nil {
		t.Fatal(err)
	}
	escalate := newReviewedEscalator(db, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})), func(runtimedb.Butler) (provider.Provider, error) {
		return &fixedProvider{reply: "Did you have breakfast?"}, nil
	})

	text, err := escalate(context.Background(), "alfred", automation.EscalationRequest{
		Rule: "breakfast", Instruction: "ask about breakfast", Fallback: "Did you eat?",
	})
	if err != nil {
		t.Fatalf("native escalation returned an error: %v", err)
	}
	if text != "Did you have breakfast?" {
		t.Fatalf("text = %q, want the phrased answer", text)
	}

	deliveries, err := db.ListAgentDeliveries(b.Seq, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].Kind != agentapi.DeliveryEscalation {
		t.Fatalf("deliveries = %+v, want exactly one escalation delivery", deliveries)
	}
}

func TestReviewedEscalatorFallsBackForANonNativeRuntimeButStillRecordsTheDelivery(t *testing.T) {
	db, b := escalatorFixture(t)
	if err := db.SetButlerRuntime(b.Seq, runtimedb.ButlerRuntime{Type: agentapi.RuntimeHermes, Endpoint: "https://hermes.internal/"}, "mika"); err != nil {
		t.Fatal(err)
	}
	called := false
	escalate := newReviewedEscalator(db, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})), func(runtimedb.Butler) (provider.Provider, error) {
		called = true
		return &fixedProvider{reply: "should never be asked"}, nil
	})

	_, err := escalate(context.Background(), "alfred", automation.EscalationRequest{
		Rule: "breakfast", Instruction: "ask about breakfast", Fallback: "Did you eat?",
	})
	if err == nil {
		t.Fatal("a non-native runtime's escalation returned no error, want a refusal so the rule's own wording is used")
	}
	if called {
		t.Fatal("the provider was reached for a non-native runtime; escalation is not served for anything but native yet")
	}

	deliveries, err := db.ListAgentDeliveries(b.Seq, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].Kind != agentapi.DeliveryEscalation {
		t.Fatalf("deliveries = %+v, want the escalation still recorded even though nothing served it synchronously", deliveries)
	}
}

func TestReviewedEscalatorRefusesAnUnreachableAgentWithoutRecordingADelivery(t *testing.T) {
	db, _ := escalatorFixture(t)
	escalate := newReviewedEscalator(db, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})), func(runtimedb.Butler) (provider.Provider, error) {
		t.Fatal("the provider was reached for an unknown agent")
		return nil, nil
	})
	if _, err := escalate(context.Background(), "no-such-agent", automation.EscalationRequest{
		Rule: "breakfast", Instruction: "ask about breakfast", Fallback: "Did you eat?",
	}); err == nil {
		t.Fatal("an unknown agent's escalation returned no error")
	}
}
