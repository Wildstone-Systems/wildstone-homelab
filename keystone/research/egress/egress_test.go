package egress

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/research"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/research/publicfetch"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

type fakeAdapters struct{ searches, fetches int }

func (*fakeAdapters) Capabilities() research.Capabilities { return research.Capabilities{} }
func (f *fakeAdapters) Search(context.Context, research.SearchRequest) (research.SearchResult, error) {
	f.searches++
	return research.SearchResult{}, nil
}
func (f *fakeAdapters) Fetch(context.Context, string) (publicfetch.Page, error) {
	f.fetches++
	return publicfetch.Page{}, nil
}

func TestAdaptersReceiveNothingOnDeniedEgress(t *testing.T) {
	db, err := runtimedb.Open(migrated.Into(t, filepath.Join(t.TempDir(), "runtime.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	check(db.RecordButlerMember("fixture-owner", runtimedb.RoleOwner, "", ""))
	check(db.AdmitButlerMember("fixture-owner", "root-admin"))
	check(db.UpsertModel(runtimedb.Model{Name: "fixture-local", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "fixture", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"))
	_, err = db.CreateButler(runtimedb.Butler{Name: "fixture-agent", Directive: "fixture", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Model: "fixture-local"}, "root-admin")
	check(err)
	task, err := db.InsertButlerTask(runtimedb.ButlerTask{Kind: runtimedb.ButlerTaskConversation, Author: "fixture-owner", Owner: "fixture-owner", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Audience: runtimedb.ButlerAudiencePrivate, ObjectiveKind: runtimedb.ObjectiveInformational, Objective: "fixture research", CompletionCriteria: "fixture result", OriginConnection: "console"})
	check(err)
	check(db.SetTaskButler(task, "fixture-agent"))
	turn := func() context.Context {
		ctx, p, err := db.BeginPrivacyTurn(context.Background(), runtimedb.ModelLocal, runtimedb.MemoryDestination{BuiltIn: true, ConsoleOwner: true}, "")
		check(err)
		check(p.SetTask(task))
		return ctx
	}
	ctx := turn()
	adapter := &fakeAdapters{}
	invoke := func(ctx context.Context, denied bool) {
		t.Helper()
		beforeSearch, beforeFetch := adapter.searches, adapter.fetches
		_, searchErr := Search(ctx, adapter, research.SearchRequest{Query: "sanitized public documentation"})
		_, fetchErr := Fetch(ctx, adapter, "https://example.com/sanitized-public-documentation")
		if denied {
			if searchErr == nil || fetchErr == nil || adapter.searches != beforeSearch || adapter.fetches != beforeFetch {
				t.Fatalf("denied request reached adapter: %v %v searches=%d fetches=%d", searchErr, fetchErr, adapter.searches, adapter.fetches)
			}
		} else {
			check(searchErr)
			check(fetchErr)
			if adapter.searches != beforeSearch+1 || adapter.fetches != beforeFetch+1 {
				t.Fatal("allowed request was not dispatched")
			}
		}
	}
	invoke(context.Background(), true)
	// Console administration does not substitute for current source access.
	invoke(ctx, true)
	check(db.GrantButlerAccess("fixture-agent", "fixture-owner", "root-admin"))
	invoke(ctx, false)
	memory, err := db.RecordMemory(runtimedb.MemoryRecord{Scope: runtimedb.MemoryScopePersonal, Kind: runtimedb.MemoryFact, Content: "synthetic protected material", Owner: "fixture-owner", Author: "fixture-owner", Butler: "fixture-agent", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "household", SourceKind: "conversation", SourceID: "fixture"})
	check(err)
	check(runtimedb.TrackMemoryPrivacy(ctx, memory, nil))
	invoke(ctx, false)
	for _, policy := range []runtimedb.PrivacyPolicy{{LocalOnly: true}, {TrustedOnly: true}} {
		check(db.SetMemoryPrivacy("fixture-owner", memory, policy))
		invoke(ctx, true)
		invoke(turn(), true)
	}
	check(db.SetMemoryPrivacy("fixture-owner", memory, runtimedb.PrivacyPolicy{}))
	invoke(ctx, false)
	check(db.RevokeButlerAccess("fixture-agent", "fixture-owner", "root-admin"))
	invoke(ctx, true)
	resumed := turn()
	invoke(resumed, true)
	if runtimedb.TrackMemoryPrivacy(ctx, 0, errors.New("synthetic read fault")) == nil {
		t.Fatal("read fault was not latched")
	}
	invoke(ctx, true)
	check(db.Close())
	invoke(resumed, true)
}
