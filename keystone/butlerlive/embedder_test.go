package butlerlive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func embedderFixture(t *testing.T, endpoint string, kinds ...string) (*runtimedb.DB, *runtimedb.PrivacyTurn) {
	t.Helper()
	kind := runtimedb.ModelLocal
	if len(kinds) > 0 {
		kind = kinds[0]
	}
	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = db.UpsertModel(runtimedb.Model{Name: "assigned-route", Kind: kind, Provider: "ollama", ProviderModel: "embed-model", Endpoint: endpoint, Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err = db.RecordButlerMember("owner", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err = db.AdmitButlerMember("owner", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateButler(runtimedb.Butler{Name: "memory-agent", Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "assigned-route"}, "owner"); err != nil {
		t.Fatal(err)
	}
	_, privacy, err := db.BeginPrivacyTurn(context.Background(), kind, runtimedb.MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	return db, privacy
}

func TestResolvedEmbedderRechecksPrivacyBeforeRetry(t *testing.T) {
	var calls atomic.Int32
	var db *runtimedb.DB
	var memory int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			if err := db.SetMemoryPrivacy("owner", memory, runtimedb.PrivacyPolicy{LocalOnly: true}); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"embeddings":[[1,2,3]]}`)
	}))
	defer server.Close()
	db, _ = embedderFixture(t, server.URL, runtimedb.ModelCloud)
	var err error
	memory, err = db.RecordMemory(runtimedb.MemoryRecord{
		Kind: runtimedb.MemoryPreference, Content: "synthetic private memory",
		Owner: "owner", Author: "owner", Butler: "memory-agent",
		Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "household",
		SourceKind: "conversation", SourceID: "synthetic-fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	privacyContext, tracked, err := db.BeginPrivacyTurn(context.Background(), runtimedb.ModelCloud, runtimedb.MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = runtimedb.TrackMemoryPrivacy(privacyContext, memory, nil); err != nil {
		t.Fatal(err)
	}
	embedder, err := ResolveEmbedder(db, "memory-agent", tracked)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = embedder.Embed(context.Background(), "synthetic memory"); !errors.Is(err, runtimedb.ErrPrivacy) || calls.Load() != 1 {
		t.Fatalf("changed privacy: err=%v calls=%d", err, calls.Load())
	}
}

func TestResolveEmbedderUsesOnlyExactAgentRoute(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"embeddings":[[1,2,3]]}`)
	}))
	defer server.Close()
	db, privacy := embedderFixture(t, server.URL)

	embedder, err := ResolveEmbedder(db, "memory-agent", privacy)
	if err != nil {
		t.Fatal(err)
	}
	if vector, err := embedder.Embed(context.Background(), "synthetic memory"); err != nil || len(vector) != 3 || calls.Load() != 1 {
		t.Fatalf("embed = %v, %v, calls=%d", vector, err, calls.Load())
	}
	if _, err = ResolveEmbedder(db, "missing-agent", privacy); !errors.Is(err, runtimedb.ErrNotFound) {
		t.Fatalf("missing agent = %v", err)
	}
}

func TestResolvedEmbedderFailsClosedWhenAssignmentChanges(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"embeddings":[[1,2,3]]}`)
	}))
	defer server.Close()
	db, privacy := embedderFixture(t, server.URL)
	embedder, err := ResolveEmbedder(db, "memory-agent", privacy)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetModelEnabled("assigned-route", false, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err = embedder.Embed(context.Background(), "synthetic memory"); !errors.Is(err, ErrEmbeddingAssignmentChanged) || calls.Load() != 0 {
		t.Fatalf("disabled assignment: err=%v calls=%d", err, calls.Load())
	}
}

func TestResolvedEmbedderRechecksAssignmentBeforeRetry(t *testing.T) {
	var calls atomic.Int32
	var db *runtimedb.DB
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			if err := db.SetModelEnabled("assigned-route", false, "owner"); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"embeddings":[[1,2,3]]}`)
	}))
	defer server.Close()
	var privacy *runtimedb.PrivacyTurn
	db, privacy = embedderFixture(t, server.URL)
	embedder, err := ResolveEmbedder(db, "memory-agent", privacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = embedder.Embed(context.Background(), "synthetic memory"); !errors.Is(err, ErrEmbeddingAssignmentChanged) || calls.Load() != 1 {
		t.Fatalf("changed retry assignment: err=%v calls=%d", err, calls.Load())
	}
}

func TestResolveEmbedderDistinguishesMissingConfigurationAndPrivacy(t *testing.T) {
	db, privacy := embedderFixture(t, "http://example.internal")
	if _, err := ResolveEmbedder(db, "memory-agent", nil); err != nil {
		t.Fatalf("resolution should not consume content: %v", err)
	}
	embedder, err := ResolveEmbedder(db, "memory-agent", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = embedder.Embed(context.Background(), "synthetic memory"); !errors.Is(err, runtimedb.ErrPrivacy) {
		t.Fatalf("missing privacy guard = %v", err)
	}
	if err = db.UpsertModel(runtimedb.Model{Name: "cloud-route", Kind: runtimedb.ModelCloud, Provider: "anthropic", ProviderModel: "model", Readiness: runtimedb.ModelReady, Enabled: true}, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = db.SetButlerRoute("memory-agent", "cloud-route", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveEmbedder(db, "memory-agent", privacy); !errors.Is(err, provider.ErrEmbedderNotConfigured) {
		t.Fatalf("unsupported assigned route = %v", err)
	}
}
