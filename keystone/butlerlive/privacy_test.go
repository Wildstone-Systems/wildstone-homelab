package butlerlive

import (
	"context"
	"errors"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/portal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestLocalOnlyFailureIsVisibleNotSilent(t *testing.T) {
	f := memoryFixture(t)
	seq := putMemory(t, f, "sam", runtimedb.ButlerAudiencePrivate, runtimedb.MemoryScopePersonal)
	if err := f.db.SetMemoryPrivacy("sam", seq, runtimedb.PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.db.SetButlerRoute("memory-agent", "cloud", "mika"); err != nil {
		t.Fatal(err)
	}
	_, err := f.butlers.Serve(context.Background(), Turn{Butler: "memory-agent", Member: "sam", Text: "Hello", Connection: ConnectionScope{Name: portal.Connection, Permits: portal.Permits}})
	if !errors.Is(err, runtimedb.ErrPrivacy) {
		t.Fatalf("expected visible privacy failure: %v", err)
	}
	if f.cloud.sawAnything() {
		t.Fatal("cloud provider was called")
	}
}
func TestLocalOnlyNeverFallsBackToCloud(t *testing.T) {
	f := memoryFixture(t)
	seq := putMemory(t, f, "sam", runtimedb.ButlerAudiencePrivate, runtimedb.MemoryScopePersonal)
	if err := f.db.SetMemoryPrivacy("sam", seq, runtimedb.PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetModelEnabled("kitchen-box", false, "mika"); err != nil {
		t.Fatal(err)
	}
	_, err := f.butlers.Serve(context.Background(), Turn{Butler: "memory-agent", Member: "sam", Text: "Hello", Connection: ConnectionScope{Name: portal.Connection, Permits: portal.Permits}})
	if err == nil {
		t.Fatal("unavailable local model silently degraded")
	}
	if f.cloud.sawAnything() {
		t.Fatal("cloud fallback received local-only content")
	}
}

func TestPrivacyPositiveControlAndResumedHistory(t *testing.T) {
	f := memoryFixture(t)
	seq := putMemory(t, f, "sam", runtimedb.ButlerAudiencePrivate, runtimedb.MemoryScopePersonal)
	turn := Turn{Butler: "memory-agent", Member: "sam", Text: "Hello", Connection: ConnectionScope{Name: portal.Connection, Permits: portal.Permits}}
	ans, err := f.butlers.Serve(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if !f.local.sawText("jasmine private canary") {
		t.Fatal("positive control never received memory")
	}
	session, err := f.db.InsertButlerSession(runtimedb.ButlerSession{Principal: "butler", ActingFor: "sam", Butler: "memory-agent", Surface: runtimedb.ButlerSurfaceFamily, Mode: runtimedb.ButlerModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.SetButlerTaskSession(ans.TaskID, session); err != nil {
		t.Fatal(err)
	}
	if err = f.db.SetMemoryPrivacy("sam", seq, runtimedb.PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.db.SetButlerRoute("memory-agent", "cloud", "mika"); err != nil {
		t.Fatal(err)
	}
	if err = f.db.ForgetMemory(seq, "sam"); err != nil {
		t.Fatal(err)
	}
	turn.SessionID = session
	_, err = f.butlers.Serve(context.Background(), turn)
	if !errors.Is(err, runtimedb.ErrPrivacy) {
		t.Fatalf("resumed history allowed: %v", err)
	}
	if f.cloud.sawAnything() {
		t.Fatal("resumed privacy content sent to cloud")
	}
}

type privacyChangingProvider struct{ change func() error }

func (p privacyChangingProvider) Name() string { return "test" }
func (p privacyChangingProvider) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	if err := p.change(); err != nil {
		return nil, err
	}
	return &provider.ChatResponse{Text: "jasmine private canary"}, nil
}
func TestPrivacyChangeDuringInferenceBlocksDelivery(t *testing.T) {
	f := memoryFixture(t)
	seq := putMemory(t, f, "sam", runtimedb.ButlerAudiencePrivate, runtimedb.MemoryScopePersonal)
	ctx, guard, err := f.db.BeginPrivacyTurn(context.Background(), runtimedb.ModelCloud, runtimedb.MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	runtimedb.TrackMemoryPrivacy(ctx, seq, nil)
	p := privacyProvider{Provider: privacyChangingProvider{change: func() error { return f.db.SetMemoryPrivacy("sam", seq, runtimedb.PrivacyPolicy{LocalOnly: true}) }}, privacy: guard}
	response, err := p.Chat(ctx, provider.ChatRequest{})
	if !errors.Is(err, runtimedb.ErrPrivacy) || response != nil {
		t.Fatalf("changed policy accepted output: %+v %v", response, err)
	}
}

func TestProviderHTTPRetryRechecksLivePrivacy(t *testing.T) {
	f := memoryFixture(t)
	seq := putMemory(t, f, "sam", runtimedb.ButlerAudiencePrivate, runtimedb.MemoryScopePersonal)
	ctx, guard, err := f.db.BeginPrivacyTurn(context.Background(), runtimedb.ModelCloud, runtimedb.MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	runtimedb.TrackMemoryPrivacy(ctx, seq, nil)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			if err := f.db.SetMemoryPrivacy("sam", seq, runtimedb.PrivacyPolicy{LocalOnly: true}); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	p := privacyProvider{Provider: provider.NewOpenAIProvider(server.URL, ""), privacy: guard}
	_, err = p.Chat(ctx, provider.ChatRequest{Messages: []provider.Message{{Role: "user", Text: "jasmine private canary"}}})
	if !errors.Is(err, runtimedb.ErrPrivacy) {
		t.Fatalf("privacy change ignored: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("restricted content sent in %d HTTP attempts", calls.Load())
	}
}
