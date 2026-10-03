package butlerstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/research"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

const searchTTL = 15 * time.Minute

var ErrSearchRecord = errors.New("invalid_input: invalid research record")

// SearchScope is resolved from trusted current task/provider policy, never tool
// JSON. Resource includes disclosure/surface scope; Revision is provider config.
type SearchScope struct{ Person, Resource, Provider, Backend, Revision string }

// SearchRecord is retained inside ToolStepResult.Content only after the canonical
// gateway successfully checkpoints and verifies its tool step. It is not authority.
type SearchRecord struct {
	Version  int
	Key      string
	Scope    SearchScope
	StoredAt time.Time
	Result   research.SearchResult
}

func (r SearchRecord) Encode() (string, error) {
	b, err := json.Marshal(r)
	if err != nil || len(b) > 32768 {
		return "", ErrSearchRecord
	}
	return string(b), nil
}

type searchEntry struct {
	task   string
	step   int64
	record SearchRecord
}

// Searches composes S23/S24 with a bounded cache of VERIFIED ledger references.
// Resolve rechecks current provider/tool grants; AuthorizeSource rechecks and
// inherits origin step provenance into the requesting task before any hit. Both
// callbacks are mandatory. They must not log queries or retained result bodies.
// The adapter callback dispatches exactly one attempt; retries call Search again.
type Searches struct {
	Research        *Research
	Resolve         func(context.Context, string) (SearchScope, error)
	AuthorizeSource func(context.Context, string, string, int64) error
	checkEgress     func(context.Context) error // deterministic test seam; production always uses S23
	Now             func() time.Time
	mu              sync.Mutex
	entries         map[string]searchEntry
}

func (s *Searches) egress(ctx context.Context) error {
	if s.checkEgress != nil {
		return s.checkEgress(ctx)
	}
	return runtimedb.CheckResearchEgress(ctx)
}

func (s *Searches) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func searchKey(scope SearchScope, request research.SearchRequest) (string, error) {
	for _, v := range []string{scope.Person, scope.Resource, scope.Provider, scope.Backend, scope.Revision} {
		if v == "" || len(v) > 1024 || !utf8.ValidString(v) {
			return "", ErrSearchRecord
		}
	}
	if request.Limit == 0 {
		request.Limit = 5
	}
	if request.Query == "" || len(request.Query) > 1024 || !utf8.ValidString(request.Query) || request.Limit < 1 || request.Limit > 5 {
		return "", ErrSearchRecord
	}
	for _, v := range []*string{request.Filters.Region, request.Filters.Language} {
		if v != nil && (*v == "" || len(*v) > 1024 || !utf8.ValidString(*v)) {
			return "", ErrSearchRecord
		}
	}
	if v := request.Filters.RecencyDays; v != nil && (*v < 1 || *v > 36500) {
		return "", ErrSearchRecord
	}
	b, err := json.Marshal(struct {
		Scope   SearchScope
		Request research.SearchRequest
	}{scope, request})
	if err != nil || len(b) > 8192 {
		return "", ErrSearchRecord
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func (s *Searches) fresh(r SearchRecord, key string, scope SearchScope) bool {
	n := s.now()
	return r.Version == 1 && r.Key == key && r.Scope == scope && !r.StoredAt.IsZero() && !n.Before(r.StoredAt) && n.Before(r.StoredAt.Add(searchTTL)) && r.Result.Backend == scope.Backend && r.Result.Provider != "" && !r.Result.RetrievedAt.IsZero() && len(r.Result.Items) <= 5
}

func (s *Searches) recorded(task string, step int64, key string, scope SearchScope) (SearchRecord, int64, bool, error) {
	t, err := s.Research.Ledger.db.GetButlerTask(task)
	if err != nil {
		return SearchRecord{}, 0, false, err
	}
	if t.Owner != scope.Person {
		return SearchRecord{}, 0, false, ErrAuthorityWithdrawn
	}
	steps, err := s.Research.Ledger.Steps(task)
	if err != nil {
		return SearchRecord{}, 0, false, err
	}
	for i := len(steps) - 1; i >= 0; i-- {
		v := steps[i]
		if (step != 0 && v.Seq != step) || v.Kind != "tool" || v.Name != "web_search" || v.State != butler.LedgerStepVerified || v.VerifiedAt.IsZero() {
			continue
		}
		var envelope butler.ToolStepResult
		if json.Unmarshal([]byte(v.Result), &envelope) != nil || envelope.Version != 1 || envelope.Fingerprint == "" || envelope.Rejected || envelope.EffectPending || envelope.EffectConfirmed || envelope.ProposalConfirmed {
			continue
		}
		var r SearchRecord
		if len(envelope.Content) > 32768 || json.Unmarshal([]byte(envelope.Content), &r) != nil || !s.fresh(r, key, scope) || r.StoredAt.After(v.VerifiedAt) {
			continue
		}
		return r, v.Seq, true, nil
	}
	return SearchRecord{}, 0, false, nil
}

// Search returns a record for canonical checkpointing. Fresh dispatches are NOT
// cached here: cancellation/crash before ledger verification must spend again.
// Restart can reuse only verified same-task records, never unknown reservations.
// cacheHit does not replenish or spend upstream totals; Invoke still charges its
// normal bounded call. Retrieval dates and evidence are returned unchanged.
func (s *Searches) Search(ctx context.Context, task string, attempt int64, request research.SearchRequest, dispatch func(context.Context, research.SearchRequest) (research.SearchResult, error)) (record SearchRecord, cacheHit bool, err error) {
	if s == nil || s.Research == nil || s.Resolve == nil || s.AuthorizeSource == nil || dispatch == nil {
		return record, false, ErrAuthorityWithdrawn
	}
	err = s.Research.ReadRetained(ctx, task, func(context.Context) error { return nil })
	if err != nil {
		return record, false, err
	}
	scope, err := s.Resolve(ctx, task)
	if err != nil {
		return record, false, ErrAuthorityWithdrawn
	}
	t, err := s.Research.Ledger.db.GetButlerTask(task)
	if err != nil || t.Owner != scope.Person {
		return record, false, ErrAuthorityWithdrawn
	}
	key, err := searchKey(scope, request)
	if err != nil {
		return record, false, err
	}
	// Recheck the authoritative origin receipt, not just process memory.
	s.mu.Lock()
	e, present := s.entries[key]
	s.mu.Unlock()
	if present {
		record, _, cacheHit, err = s.recorded(e.task, e.step, key, scope)
	}
	if err != nil {
		return SearchRecord{}, false, err
	}
	if !cacheHit {
		record, e.step, cacheHit, err = s.recorded(task, 0, key, scope)
		e.task = task
		if err != nil {
			return SearchRecord{}, false, err
		}
	}
	if cacheHit {
		if err = s.AuthorizeSource(ctx, task, e.task, e.step); err != nil {
			return SearchRecord{}, false, ErrAuthorityWithdrawn
		}
		if err = s.egress(ctx); err != nil {
			return SearchRecord{}, false, err
		}
		if err = s.Research.ReadRetained(ctx, task, func(context.Context) error { return nil }); err != nil {
			return SearchRecord{}, false, err
		}
		current, resolveErr := s.Resolve(ctx, task)
		if resolveErr != nil || current != scope {
			return SearchRecord{}, false, ErrAuthorityWithdrawn
		}
		if err = ctx.Err(); err != nil {
			return SearchRecord{}, false, err
		}
		s.mu.Lock()
		if s.entries == nil {
			s.entries = map[string]searchEntry{}
		}
		for k, v := range s.entries {
			if !s.fresh(v.record, k, v.record.Scope) {
				delete(s.entries, k)
			}
		}
		if len(s.entries) < 128 {
			e.record = record
			s.entries[key] = e
		}
		s.mu.Unlock()
		return record, true, nil
	}
	if err = s.egress(ctx); err != nil {
		return record, false, err
	}
	var result research.SearchResult
	_, err = s.Research.Attempt(ctx, task, attempt, "search", func(c context.Context) error {
		current, err := s.Resolve(c, task)
		if err != nil || current != scope {
			return ErrAuthorityWithdrawn
		}
		if err := s.egress(c); err != nil {
			return err
		}
		var callErr error
		result, callErr = dispatch(c, request)
		return callErr
	})
	if err != nil {
		return SearchRecord{}, false, err
	}
	// Results cannot escape if policy changed during I/O; successful accounting
	// remains spent even when publication is refused.
	if err = s.Research.ReadRetained(ctx, task, func(context.Context) error { return nil }); err != nil {
		return SearchRecord{}, false, err
	}
	current, resolveErr := s.Resolve(ctx, task)
	if resolveErr != nil || current != scope {
		return SearchRecord{}, false, ErrAuthorityWithdrawn
	}
	if err = s.egress(ctx); err != nil {
		return SearchRecord{}, false, err
	}
	if err = ctx.Err(); err != nil {
		return SearchRecord{}, false, err
	}
	record = SearchRecord{Version: 1, Key: key, Scope: scope, StoredAt: s.now(), Result: result}
	if !s.fresh(record, key, scope) {
		return SearchRecord{}, false, ErrSearchRecord
	}
	if _, err = record.Encode(); err != nil {
		return SearchRecord{}, false, err
	}
	return record, false, nil
}
