package indexworker_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/indexworker"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

type embedFunc func(context.Context, string) ([]float32, error)

func (f embedFunc) Embed(ctx context.Context, text string) ([]float32, error) { return f(ctx, text) }

func fixture(t *testing.T) (*runtimedb.DB, runtimedb.CoordinatorLease, int64) {
	t.Helper()
	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.UpsertModel(runtimedb.Model{Name: "fixture-model", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "embed-fixture", Endpoint: "http://example.internal", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err = db.RecordButlerMember("owner", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err = db.AdmitButlerMember("owner", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateButler(runtimedb.Butler{Name: "memory-agent", Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "fixture-model"}, "owner"); err != nil {
		t.Fatal(err)
	}
	seq, err := db.RecordMemory(runtimedb.MemoryRecord{Kind: runtimedb.MemoryPreference, Content: "synthetic reminder about the blue bin", Owner: "owner", Author: "owner", Butler: "memory-agent", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "memory-agent", SourceKind: "conversation", SourceID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := db.AcquireCoordinator("fixture-coordinator")
	if err != nil {
		t.Fatal(err)
	}
	return db, lease, seq
}

func resolver(embed provider.Embedder) indexworker.Resolver {
	return func(*runtimedb.DB, string, *runtimedb.PrivacyTurn) (provider.Embedder, string, string, error) {
		return embed, "ollama/embed-fixture", "1", nil
	}
}

func TestWorkerPublishesCurrentAuthorizedMemory(t *testing.T) {
	db, lease, _ := fixture(t)
	w := &indexworker.Worker{DB: db, Lease: lease, Resolve: resolver(embedFunc(func(context.Context, string) ([]float32, error) { return []float32{1, 0, 0}, nil }))}
	if ok, err := w.RunOnce(context.Background()); err != nil || !ok {
		t.Fatalf("RunOnce = %t, %v", ok, err)
	}
	hits, err := db.SearchIndex(runtimedb.IndexQuery{Reader: "owner", Butler: "memory-agent", Text: "unmatched", Embedding: []float32{1, 0, 0}})
	if err != nil || len(hits) != 1 {
		t.Fatalf("semantic hits = %+v, %v", hits, err)
	}
}

func TestForgetDuringEmbeddingPreventsPublication(t *testing.T) {
	db, lease, memory := fixture(t)
	w := &indexworker.Worker{DB: db, Lease: lease, Resolve: resolver(embedFunc(func(context.Context, string) ([]float32, error) {
		if err := db.ForgetMemory(memory, "owner"); err != nil {
			t.Fatal(err)
		}
		return []float32{1, 0, 0}, nil
	}))}
	if ok, err := w.RunOnce(context.Background()); err == nil || !ok {
		t.Fatalf("RunOnce after forget = %t, %v", ok, err)
	}
	hits, err := db.SearchIndex(runtimedb.IndexQuery{Reader: "owner", Butler: "memory-agent", Text: "blue", Embedding: []float32{1, 0, 0}})
	if err != nil || len(hits) != 0 {
		t.Fatalf("forgotten source republished: %+v, %v", hits, err)
	}
}

func TestCoordinatorLossPreventsPublication(t *testing.T) {
	db, lease, _ := fixture(t)
	w := &indexworker.Worker{DB: db, Lease: lease, Resolve: resolver(embedFunc(func(context.Context, string) ([]float32, error) {
		if _, err := db.PromoteCoordinator("replacement-coordinator"); err != nil {
			t.Fatal(err)
		}
		return []float32{1, 0, 0}, nil
	}))}
	if ok, err := w.RunOnce(context.Background()); err == nil || !ok {
		t.Fatalf("RunOnce after promotion = %t, %v", ok, err)
	}
}

func TestFailingJobStopsAfterMaxAttempts(t *testing.T) {
	db, lease, _ := fixture(t)
	w := &indexworker.Worker{DB: db, Lease: lease, Resolve: resolver(embedFunc(func(context.Context, string) ([]float32, error) {
		return nil, errors.New("synthetic provider failure")
	})), RetryDelay: time.Nanosecond}
	for i := 0; i < 5; i++ {
		time.Sleep(time.Millisecond)
		if ok, err := w.RunOnce(context.Background()); err == nil || !ok {
			t.Fatalf("attempt %d = %t, %v", i+1, ok, err)
		}
	}
	time.Sleep(time.Millisecond)
	if ok, err := w.RunOnce(context.Background()); err != nil || ok {
		t.Fatalf("job still claimable after max attempts: %t, %v", ok, err)
	}
}

func neverEmbed(t *testing.T) indexworker.Resolver {
	return resolver(embedFunc(func(context.Context, string) ([]float32, error) {
		t.Fatal("memory content reached the provider")
		return nil, nil
	}))
}

func TestRestrictedAgentNeverReceivesMemory(t *testing.T) {
	db, lease, memory := fixture(t)
	m, err := db.GetMemory(memory)
	if err != nil {
		t.Fatal(err)
	}
	if m.Scope != runtimedb.MemoryScopePersonal && m.Scope != runtimedb.MemoryScopeHousehold {
		t.Skipf("fixture scope %q cannot be restricted", m.Scope)
	}
	if err := db.SetAgentMemoryRestriction("memory-agent", m.Scope, true, "owner"); err != nil {
		t.Fatal(err)
	}
	w := &indexworker.Worker{DB: db, Lease: lease, Resolve: neverEmbed(t)}
	if ok, err := w.RunOnce(context.Background()); err != nil || !ok {
		t.Fatalf("RunOnce = %t, %v", ok, err)
	}
}

func TestRestoreQuarantineHoldsIndexing(t *testing.T) {
	db, lease, _ := fixture(t)
	if err := db.EnterRestoreQuarantine("synthetic restore", time.Now()); err != nil {
		t.Fatal(err)
	}
	w := &indexworker.Worker{DB: db, Lease: lease, Resolve: neverEmbed(t)}
	if ok, err := w.RunOnce(context.Background()); err == nil || !ok {
		t.Fatalf("RunOnce during quarantine = %t, %v", ok, err)
	}
}
