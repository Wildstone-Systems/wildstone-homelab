package butlerstore

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/research"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestSearchCachePartitions(t *testing.T) {
	scope := SearchScope{"person", "private/agent", "provider", "duckduckgo", "revision"}
	request := research.SearchRequest{Query: "synthetic query"}
	key, err := searchKey(scope, request)
	if err != nil {
		t.Fatal(err)
	}
	withDefault := request
	withDefault.Limit = 5
	if k, _ := searchKey(scope, withDefault); k != key {
		t.Fatal("default limit differs")
	}
	for i := 0; i < 5; i++ {
		changed := scope
		fields := []*string{&changed.Person, &changed.Resource, &changed.Provider, &changed.Backend, &changed.Revision}
		*fields[i] += "-other"
		if k, _ := searchKey(changed, request); k == key {
			t.Fatalf("partition %d shared", i)
		}
	}
	region, language, days := "us-en", "en", 7
	for _, r := range []research.SearchRequest{{Query: "different"}, {Query: request.Query, Limit: 1}, {Query: request.Query, Filters: research.SearchFilters{Region: &region}}, {Query: request.Query, Filters: research.SearchFilters{Language: &language}}, {Query: request.Query, Filters: research.SearchFilters{RecencyDays: &days}}} {
		if k, _ := searchKey(scope, r); k == key {
			t.Fatal("request partition shared")
		}
	}
}

func TestSearchConcurrentCancellationDoesNotReuseUnverifiedAttempt(t *testing.T) {
	db, l := testLedger(t, filepath.Join(t.TempDir(), "runtime.db"))
	defer db.Close()
	if err := db.AdmitButlerMember("mika", "test-admin"); err != nil {
		t.Fatal(err)
	}
	task := newTask(t, db)
	if err := l.Running(task, "test"); err != nil {
		t.Fatal(err)
	}
	start := func() (int64, int64) {
		t.Helper()
		step, err := l.AppendStep(task, "tool", "web_search", "{}")
		if err != nil {
			t.Fatal(err)
		}
		a, err := l.StartAttempt(step)
		if err != nil {
			t.Fatal(err)
		}
		return step, a.Seq
	}
	_, first := start()
	step, second := start()
	s := &Searches{Research: &Research{Ledger: l, Authorize: func(context.Context, string, string) (runtimedb.ResearchLimits, error) {
		return runtimedb.DefaultResearchLimits(), nil
	}}, Resolve: func(context.Context, string) (SearchScope, error) {
		return SearchScope{"mika", "private", "provider", "duckduckgo", "rev"}, nil
	}, AuthorizeSource: func(context.Context, string, string, int64) error { return nil }, checkEgress: func(context.Context) error { return nil }}
	request := research.SearchRequest{Query: "synthetic shared request"}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	dispatch := func(ctx context.Context, _ research.SearchRequest) (research.SearchResult, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return research.SearchResult{Provider: research.DDGS, Backend: "duckduckgo", RetrievedAt: time.Now()}, ctx.Err()
	}
	flight, cancel := context.WithCancel(context.Background())
	type outcome struct {
		r   SearchRecord
		hit bool
		err error
	}
	failed, success := make(chan outcome, 1), make(chan outcome, 1)
	go func() { r, h, e := s.Search(flight, task, first, request, dispatch); failed <- outcome{r, h, e} }()
	go func() {
		r, h, e := s.Search(context.Background(), task, second, request, dispatch)
		success <- outcome{r, h, e}
	}()
	// Both reservations must dispatch independently: an in-flight response is
	// not verified reuse permission. Cancelling one never cancels the other.
	<-entered
	<-entered
	cancel()
	close(release)
	a, b := <-failed, <-success
	if !errors.Is(a.err, context.Canceled) || a.hit || b.err != nil || b.hit || calls.Load() != 2 {
		t.Fatalf("concurrent outcomes: %+v %+v calls=%d", a, b, calls.Load())
	}
	content, err := b.r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(butler.ToolStepResult{Version: 1, Fingerprint: "web_search", Content: content})
	if err = l.FinishAttempt(second, true, string(encoded), ""); err != nil {
		t.Fatal(err)
	}
	if err = l.SetStepState(step, butler.LedgerStepVerified); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := s.Search(context.Background(), task, 0, request, dispatch); err != nil || !hit || calls.Load() != 2 {
		t.Fatalf("verified survivor: %v %v", hit, err)
	}
	budget, err := db.GetResearchBudget(task)
	if err != nil || budget.SearchesUsed != 2 {
		t.Fatalf("concurrent spend: %+v %v", budget, err)
	}
}

func TestSearchVerifiedReuseCancellationTTLAndAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	db, l := testLedger(t, path)
	defer func() { db.Close() }()
	if err := db.AdmitButlerMember("mika", "test-admin"); err != nil {
		t.Fatal(err)
	}
	task := newTask(t, db)
	if err := l.Running(task, "test"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	scope := SearchScope{"mika", "private/agent", "provider", "duckduckgo", "revision"}
	allowed, sourceAllowed, egressAllowed := true, true, true
	calls, sources := 0, 0
	policy := &Research{Ledger: l, Authorize: func(context.Context, string, string) (runtimedb.ResearchLimits, error) {
		if !allowed {
			return runtimedb.ResearchLimits{}, ErrAuthorityWithdrawn
		}
		return runtimedb.ResearchLimits{Searches: 10, Fetches: 20}, nil
	}}
	makeService := func() *Searches {
		return &Searches{Research: policy, Now: func() time.Time { return now }, Resolve: func(context.Context, string) (SearchScope, error) { return scope, nil }, AuthorizeSource: func(context.Context, string, string, int64) error {
			sources++
			if !sourceAllowed {
				return ErrAuthorityWithdrawn
			}
			return nil
		}, checkEgress: func(context.Context) error {
			if !egressAllowed {
				return runtimedb.ErrResearchPrivacy
			}
			return nil
		}}
	}
	s := makeService()
	request := research.SearchRequest{Query: "synthetic protected query"}
	dispatch := func(context.Context, research.SearchRequest) (research.SearchResult, error) {
		calls++
		return research.SearchResult{Provider: research.DDGS, Backend: "duckduckgo", RetrievedAt: now, Items: []research.SearchHit{{Source: research.Source{URL: "https://example.com/", Provider: research.DDGS, Backend: "duckduckgo", RetrievedAt: now}, Snippet: "synthetic protected result"}}}, nil
	}
	start := func() (int64, int64) {
		t.Helper()
		step, err := l.AppendStep(task, "tool", "web_search", "{}")
		if err != nil {
			t.Fatal(err)
		}
		a, err := l.StartAttempt(step)
		if err != nil {
			t.Fatal(err)
		}
		return step, a.Seq
	}
	record := func(step, attempt int64, r SearchRecord) {
		t.Helper()
		content, err := r.Encode()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(butler.ToolStepResult{Version: 1, Fingerprint: "web_search", Content: content})
		if err = l.FinishAttempt(attempt, true, string(b), ""); err != nil {
			t.Fatal(err)
		}
		if err = l.SetStepState(step, butler.LedgerStepVerified); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	step, a := start()
	// Production has no injectable public egress bypass and refuses absent
	// trusted privacy context before an adapter or reservation is reached.
	s.checkEgress = nil
	if _, _, err := s.Search(ctx, task, a, request, dispatch); !errors.Is(err, runtimedb.ErrResearchContext) {
		t.Fatal(err)
	}
	s = makeService()
	allowed = false
	if _, _, err := s.Search(ctx, task, a, request, dispatch); !errors.Is(err, ErrAuthorityWithdrawn) {
		t.Fatal(err)
	}
	allowed = true
	egressAllowed = false
	if _, _, err := s.Search(ctx, task, a, request, dispatch); !errors.Is(err, runtimedb.ErrResearchPrivacy) {
		t.Fatal(err)
	}
	egressAllowed = true
	if calls != 0 {
		t.Fatal("preflight denial dispatched")
	}
	// Cancel during I/O: spend survives, returned bytes cannot be checkpointed.
	flight, cancel := context.WithCancel(ctx)
	if _, _, err := s.Search(flight, task, a, request, func(c context.Context, r research.SearchRequest) (research.SearchResult, error) {
		result, e := dispatch(c, r)
		cancel()
		return result, e
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// An unverified success cannot seed reuse, even in the same process.
	r, hit, err := s.Search(ctx, task, a, request, dispatch)
	if err != nil || hit {
		t.Fatalf("fresh: %v %v", hit, err)
	}
	_, hit, err = s.Search(ctx, task, a, request, dispatch)
	if err != nil || hit || calls != 3 {
		t.Fatalf("unverified reused: %d %v %v", calls, hit, err)
	}
	record(step, a, r)
	_, next := start()
	retained, hit, err := s.Search(ctx, task, next, request, dispatch)
	if err != nil || !hit || calls != 3 || !retained.Result.RetrievedAt.Equal(r.Result.RetrievedAt) {
		t.Fatalf("verified reuse: %v %v", hit, err)
	}
	b, err := db.GetResearchBudget(task)
	if err != nil || b.SearchesUsed != 3 {
		t.Fatalf("budget: %+v %v", b, err)
	}
	for _, deny := range []func(){func() { allowed = false }, func() { sourceAllowed = false }, func() { egressAllowed = false }} {
		deny()
		if _, _, err = s.Search(ctx, task, next, request, dispatch); err == nil {
			t.Fatal("revoked hit escaped")
		}
		allowed, sourceAllowed, egressAllowed = true, true, true
	}
	if calls != 3 {
		t.Fatal("revoked hit dispatched")
	}
	// Process reuse may cross tasks only inside the same authoritative Person
	// partition, with origin-source inheritance and current checks repeated.
	other := newTask(t, db)
	if err = l.Running(other, "test"); err != nil {
		t.Fatal(err)
	}
	_, hit, err = s.Search(ctx, other, 0, request, dispatch)
	if err != nil || !hit || calls != 3 {
		t.Fatalf("same-Person cache: %v %v", hit, err)
	}
	scope.Person = "sam"
	if _, _, err = s.Search(ctx, other, 0, request, dispatch); !errors.Is(err, ErrAuthorityWithdrawn) {
		t.Fatal("spoofed Person escaped", err)
	}
	scope.Person = "mika"
	// Reopen the actual database and discard process cache. Only the verified
	// same-task ledger response survives, with original evidence dates.
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = runtimedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := db.AcquireCoordinator("keystone-a")
	if err != nil {
		t.Fatal(err)
	}
	l = New(db, lease)
	policy.Ledger = l
	s = makeService()
	retained, hit, err = s.Search(ctx, task, next, request, dispatch)
	if err != nil || !hit || calls != 3 || !retained.Result.RetrievedAt.Equal(r.Result.RetrievedAt) {
		t.Fatalf("restart: %v %v", hit, err)
	}
	t.Run("expiry-during-source-authorization", func(t *testing.T) {
		authorize := s.AuthorizeSource
		s.AuthorizeSource = func(ctx context.Context, to, from string, step int64) error {
			err := authorize(ctx, to, from, step)
			// No sleeps: advance from a valid hit to the exact original TTL
			// boundary inside the potentially blocking provenance callback.
			now = r.StoredAt.Add(searchTTL)
			return err
		}
		defer func() { s.AuthorizeSource = authorize }()
		out, hit, err := s.Search(ctx, task, next, request, dispatch)
		if !errors.Is(err, ErrSearchExpired) || hit || out.Version != 0 || len(out.Result.Items) != 0 || calls != 3 {
			t.Fatalf("expired publication: hit=%v error=%v record=%+v calls=%d", hit, err, out, calls)
		}
		s.mu.Lock()
		_, reinserted := s.entries[r.Key]
		s.mu.Unlock()
		if reinserted {
			t.Fatal("expired record retained or reinserted")
		}
		budget, err := db.GetResearchBudget(task)
		if err != nil || budget.SearchesUsed != 3 {
			t.Fatalf("expiry fence spent budget: %+v %v", budget, err)
		}
	})
	now = r.StoredAt.Add(searchTTL)
	_, last := start()
	_, hit, err = s.Search(ctx, task, last, request, dispatch)
	if err != nil || hit || calls != 4 {
		t.Fatalf("TTL: %d %v %v", calls, hit, err)
	}
	if sources < 4 {
		t.Fatal("source policy not rechecked")
	}
	b, err = db.GetResearchBudget(task)
	if err != nil || b.SearchesUsed != 4 {
		t.Fatalf("restart budget: %+v %v", b, err)
	}
	// A provider configuration change cannot reuse the old identity even within
	// TTL; denial of a fresh result after I/O still spends that attempt.
	scope.Revision = "new-revision"
	_, changed := start()
	_, hit, err = s.Search(ctx, task, changed, request, func(c context.Context, r research.SearchRequest) (research.SearchResult, error) {
		out, e := dispatch(c, r)
		allowed = false
		return out, e
	})
	if !errors.Is(err, ErrAuthorityWithdrawn) || hit || calls != 5 {
		t.Fatalf("post-I/O policy: %v %v %d", hit, err, calls)
	}
	allowed = true
	b, err = db.GetResearchBudget(task)
	if err != nil || b.SearchesUsed != 5 {
		t.Fatalf("denied completion spend: %+v %v", b, err)
	}
}
