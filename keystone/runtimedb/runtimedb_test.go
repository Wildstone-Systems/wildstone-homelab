package runtimedb

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/internal/dbtemplate"
)

// migrated is the schema, built once for this whole test binary. Every
// open below gets its own copy of it rather than replaying the
// migrations again — see the package comment on dbtemplate, and #231
// for what the old arithmetic cost the -race suite.
//
// A stale template is slow, not wrong: Open still runs whatever
// schema_migrations says is missing, so the worst a bad copy can do is
// make a test migrate the rest of the way itself.
//
// Tests ABOUT a migration must not use this. They want the real thing
// running against real prior data, which is what openUnmigrated and
// migrateThrough are for.
var migrated = dbtemplate.New(func(path string) error {
	db, err := Open(path)
	if err != nil {
		return err
	}
	return db.Close()
})

// open returns a private, writable, fully migrated database.
func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(migrated.Path(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrationsApplyOnceAndAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	first, err := db.AppliedMigrations()
	if err != nil {
		t.Fatalf("AppliedMigrations: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("no migrations applied")
	}
	db.Close()

	// Re-opening must not re-apply (AUTOINCREMENT tables would survive
	// anyway, but a re-run would fail on CREATE TABLE).
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer db2.Close()
	second, err := db2.AppliedMigrations()
	if err != nil {
		t.Fatalf("AppliedMigrations after reopen: %v", err)
	}
	if len(second) != len(first) {
		t.Fatalf("migration count changed across reopen: %d != %d", len(second), len(first))
	}
}

func TestEventsAppendAndCursor(t *testing.T) {
	db := open(t)
	seq1, err := db.AppendEvent(Event{Type: "test", Message: "one", Attrs: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	seq2, err := db.AppendEvent(Event{Type: "test", Message: "two"})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if seq2 <= seq1 {
		t.Fatalf("event seqs not increasing: %d then %d", seq1, seq2)
	}
	got, err := db.EventsAfter(seq1, 10)
	if err != nil {
		t.Fatalf("EventsAfter: %v", err)
	}
	if len(got) != 1 || got[0].Message != "two" {
		t.Fatalf("EventsAfter(seq1) = %+v, want just the second event", got)
	}
	all, err := db.EventsAfter(0, 10)
	if err != nil {
		t.Fatalf("EventsAfter(0): %v", err)
	}
	if len(all) != 2 || all[0].Attrs["k"] != "v" {
		t.Fatalf("EventsAfter(0) = %+v, want both events with attrs intact", all)
	}
	if all[0].Time.IsZero() || all[0].Time.Location() != time.UTC {
		t.Fatalf("event time not stamped UTC: %v", all[0].Time)
	}
	if _, err := db.AppendEvent(Event{}); err == nil {
		t.Fatal("AppendEvent with no type should fail")
	}
}

func TestPlanLifecycle(t *testing.T) {
	db := open(t)
	id, err := db.InsertPlan("mika", "", "restart immich", "medium", `{"steps":[]}`)
	if err != nil {
		t.Fatalf("InsertPlan: %v", err)
	}
	if id != "plan-1" {
		t.Fatalf("first plan id = %q, want plan-1", id)
	}

	p, err := db.GetPlan(id)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if p.State != PlanProposed || p.PlanJSON == "" {
		t.Fatalf("fresh plan = %+v, want proposed with JSON", p)
	}

	// Illegal jump: proposed → applied skips approval.
	if err := db.TransitionPlan(id, PlanApplied, ""); err == nil {
		t.Fatal("proposed → applied should be rejected")
	}

	for _, step := range []struct {
		to PlanState
		by string
	}{{PlanApproved, "mika"}, {PlanApplying, ""}, {PlanApplied, ""}} {
		if err := db.TransitionPlan(id, step.to, step.by); err != nil {
			t.Fatalf("transition to %s: %v", step.to, err)
		}
	}
	p, _ = db.GetPlan(id)
	if p.State != PlanApplied || p.ResolvedBy != "mika" || p.ResolvedAt.IsZero() {
		t.Fatalf("applied plan = %+v, want applied resolved by mika", p)
	}

	// Terminal state: nothing transitions out of applied.
	if err := db.TransitionPlan(id, PlanFailed, ""); err == nil {
		t.Fatal("applied → failed should be rejected")
	}

	if _, err := db.GetPlan("plan-999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPlan(absent) = %v, want ErrNotFound", err)
	}
	if _, err := db.GetPlan("plan-01"); err == nil {
		t.Fatal("malformed plan id should be rejected")
	}

	list, err := db.ListPlans(10)
	if err != nil {
		t.Fatalf("ListPlans: %v", err)
	}
	if len(list) != 1 || list[0].PlanJSON != "" {
		t.Fatalf("ListPlans = %+v, want one record without JSON body", list)
	}
}

func TestPrincipalsAndTokens(t *testing.T) {
	db := open(t)
	if err := db.CreatePrincipal("mika", RoleAdmin, ""); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if err := db.CreatePrincipal("mika", RoleAdmin, ""); err == nil {
		t.Fatal("duplicate principal should fail")
	}
	if err := db.CreatePrincipal("x", Role("root"), ""); err == nil {
		t.Fatal("unknown role should fail")
	}

	// Token requires an existing principal (FK enforced).
	if err := db.InsertToken("hash1", "ghost", "", time.Time{}); err == nil {
		t.Fatal("token for absent principal should fail")
	}
	if err := db.InsertToken("hash1", "mika", "guest", time.Time{}); err != nil {
		t.Fatalf("InsertToken: %v", err)
	}
	tok, err := db.GetToken("hash1")
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if tok.Principal != "mika" || tok.ActingFor != "guest" || !tok.ExpiresAt.IsZero() || !tok.RevokedAt.IsZero() {
		t.Fatalf("token = %+v", tok)
	}
	if err := db.TouchToken("hash1"); err != nil {
		t.Fatalf("TouchToken: %v", err)
	}
	tok, _ = db.GetToken("hash1")
	if tok.LastUsed.IsZero() {
		t.Fatal("TouchToken did not stamp last_used")
	}
	if err := db.RevokeToken("hash1"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	tok, _ = db.GetToken("hash1")
	if tok.RevokedAt.IsZero() {
		t.Fatal("RevokeToken did not stamp revoked_at")
	}
	if err := db.RevokeToken("hash1"); err != nil {
		t.Fatalf("re-revoking should be a no-op, got %v", err)
	}
	if err := db.RevokeToken("absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RevokeToken(absent) = %v, want ErrNotFound", err)
	}

	if err := db.SetPrincipalDisabled("mika", true); err != nil {
		t.Fatalf("SetPrincipalDisabled: %v", err)
	}
	p, err := db.GetPrincipal("mika")
	if err != nil || !p.Disabled {
		t.Fatalf("principal after disable = %+v, err %v", p, err)
	}
	if err := db.SetPrincipalDisabled("ghost", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disable absent = %v, want ErrNotFound", err)
	}
}

func TestObservedAndStatefileHead(t *testing.T) {
	db := open(t)
	if err := db.UpsertObserved(Observed{Kind: "Container", Name: "immich", Node: "n1", Healthy: false, Detail: "restarting"}); err != nil {
		t.Fatalf("UpsertObserved: %v", err)
	}
	if err := db.UpsertObserved(Observed{Kind: "Container", Name: "immich", Node: "n1", Healthy: true}); err != nil {
		t.Fatalf("UpsertObserved update: %v", err)
	}
	got, err := db.ListObserved()
	if err != nil {
		t.Fatalf("ListObserved: %v", err)
	}
	if len(got) != 1 || !got[0].Healthy || got[0].Detail != "" {
		t.Fatalf("ListObserved = %+v, want one healthy row with cleared detail", got)
	}

	head, err := db.StatefileHead()
	if err != nil || head != "" {
		t.Fatalf("fresh StatefileHead = %q, %v", head, err)
	}
	if err := db.SetStatefileHead("abc123"); err != nil {
		t.Fatalf("SetStatefileHead: %v", err)
	}
	if err := db.SetStatefileHead("def456"); err != nil {
		t.Fatalf("SetStatefileHead update: %v", err)
	}
	head, _ = db.StatefileHead()
	if head != "def456" {
		t.Fatalf("StatefileHead = %q, want def456", head)
	}
}

func TestAudit(t *testing.T) {
	db := open(t)
	if err := db.AppendAudit(AuditRecord{Principal: "mika", Method: "GET", Path: "/api/v1/whoami", Status: 200}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	if err := db.AppendAudit(AuditRecord{Method: "GET", Path: "/x", Status: 200}); err == nil {
		t.Fatal("audit without principal should fail")
	}
	got, err := db.ListAudit(10)
	if err != nil || len(got) != 1 || got[0].Status != 200 {
		t.Fatalf("ListAudit = %+v, %v", got, err)
	}
}

// TestConcurrentWriters exercises WAL + busy_timeout: concurrent event
// appends from many goroutines must all land (run under -race in CI).
func TestConcurrentWriters(t *testing.T) {
	db := open(t)
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := db.AppendEvent(Event{Type: "concurrent"}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent AppendEvent: %v", err)
	}
	got, err := db.EventsAfter(0, n+1)
	if err != nil {
		t.Fatalf("EventsAfter: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d events, want %d", len(got), n)
	}
}

// TestTransitionPlanUnderConcurrentWrites reproduces the SQLITE_BUSY seen on
// the first reconcile after every approval (friction F7, #29).
//
// TransitionPlan is read-modify-write: it SELECTs the current state to check
// the transition table, then UPDATEs. Under a DEFERRED transaction the SELECT
// only takes a read snapshot, so the UPDATE has to upgrade to the write lock —
// and if another writer committed in between, SQLite fails that upgrade with
// SQLITE_BUSY *immediately*. busy_timeout does not help: retrying an upgrade
// in place can never succeed, so SQLite does not invoke the busy handler at
// all. In production the competing writer is the periodic reconcile tick
// racing the approval poke.
func TestTransitionPlanUnderConcurrentWrites(t *testing.T) {
	db := open(t)

	const plans = 12
	ids := make([]string, 0, plans)
	for i := 0; i < plans; i++ {
		id, err := db.InsertPlan("admin", "", "concurrent transition", "low", `{}`)
		if err != nil {
			t.Fatalf("InsertPlan: %v", err)
		}
		ids = append(ids, id)
	}

	// A competing writer, standing in for the reconcile tick.
	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for {
			select {
			case <-stop:
				return
			default:
				db.AppendEvent(Event{Type: "tick"}) //nolint:errcheck
				time.Sleep(200 * time.Microsecond)
			}
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, plans)
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := db.TransitionPlan(id, PlanApproved, "admin"); err != nil {
				errs <- err
			}
		}(id)
	}
	wg.Wait()
	close(stop)
	writer.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("TransitionPlan raced a concurrent writer: %v", err)
	}
}

func TestNodeInventory(t *testing.T) {
	db := open(t)

	if nodes, err := db.ListNodes(); err != nil || len(nodes) != 0 {
		t.Fatalf("fresh ListNodes = %v, %v; want empty", nodes, err)
	}
	if err := db.RecordNodeJoin("vm-1"); err != nil {
		t.Fatalf("RecordNodeJoin: %v", err)
	}
	nodes, err := db.ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("ListNodes = %v, %v", nodes, err)
	}
	joined := nodes[0].FirstJoinedAt
	if joined.IsZero() || nodes[0].NodeID != "vm-1" {
		t.Fatalf("unexpected node record %+v", nodes[0])
	}

	// A session opening fills in the agent's self-reported build.
	if err := db.RecordNodeSeen("vm-1", "v0.1.0-alpha", 7); err != nil {
		t.Fatalf("RecordNodeSeen: %v", err)
	}
	nodes, err = db.ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("ListNodes after seen = %v, %v", nodes, err)
	}
	if nodes[0].AgentVersion != "v0.1.0-alpha" || nodes[0].ProtocolMinor != 7 {
		t.Fatalf("hello fields not recorded: %+v", nodes[0])
	}
	// Re-joining a rebuilt node must not reset when it entered the fleet.
	if err := db.RecordNodeJoin("vm-1"); err != nil {
		t.Fatalf("re-join: %v", err)
	}
	nodes, _ = db.ListNodes()
	if !nodes[0].FirstJoinedAt.Equal(joined) {
		t.Errorf("re-join reset first_joined_at: %v → %v", joined, nodes[0].FirstJoinedAt)
	}

	// A session for a node with no join row still produces one.
	if err := db.RecordNodeSeen("vm-2", "v0.1.0-alpha", 7); err != nil {
		t.Fatalf("RecordNodeSeen unknown node: %v", err)
	}
	nodes, _ = db.ListNodes()
	if len(nodes) != 2 || nodes[0].NodeID != "vm-1" || nodes[1].NodeID != "vm-2" {
		t.Fatalf("want id-ordered vm-1, vm-2; got %+v", nodes)
	}
}
