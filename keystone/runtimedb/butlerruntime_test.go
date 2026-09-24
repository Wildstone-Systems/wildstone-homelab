package runtimedb

// Phase 1's evidence (ADR-W039 §3.3, §3.7, §4.1): every Agent has a
// runtime record, an Agent that predates the record is on the native
// runtime, a runtime principal is bound to one Agent for its life, and
// health moves only through the closed set.

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

func runtimeFixture(t *testing.T) (*DB, Butler) {
	t.Helper()
	db := open(t)
	if err := db.UpsertModel(Model{
		Name: "kitchen-box", Kind: ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Readiness: ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	newButler(t, db, "alfred", "kitchen-box")
	b, err := db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	return db, b
}

// An Agent made before 0081 comes out of the migration on the native
// runtime and connected — built from a real pre-0081 database rather
// than asserted about the SQL, because the question is what happens to
// a household that already has Agents.
func TestMigrationBackfillsANativeRuntimeForExistingAgents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	old, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.migrateThrough("migrations/0080_shared_calendars.sql"); err != nil {
		t.Fatalf("migrating to the point before 0081: %v", err)
	}
	if err := old.UpsertModel(Model{
		Name: "kitchen-box", Kind: ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Readiness: ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	newButler(t, old, "alfred", "kitchen-box")
	newButler(t, old, "hobson", "kitchen-box")
	if err := old.RetireButler("hobson", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := old.GetButlerRuntime(1); !errors.Is(err, ErrNotFound) {
		// Sanity: the table does not exist yet, so nothing can be read.
		// A found row here would mean the test migrated too far.
		if err == nil {
			t.Fatal("a runtime record exists before 0081 ran")
		}
	}
	if err := old.migrate(); err != nil {
		t.Fatalf("migrating the rest of the way: %v", err)
	}

	alfred, err := old.GetButlerRuntimeByName("alfred")
	if err != nil {
		t.Fatalf("alfred has no runtime after the backfill: %v", err)
	}
	if alfred.Type != agentapi.RuntimeNative {
		t.Errorf("alfred's runtime is %q, want native", alfred.Type)
	}
	if alfred.Health != agentapi.HealthConnected {
		t.Errorf("alfred's native runtime is %q, want connected — it is in-process and has nothing to register", alfred.Health)
	}
	if alfred.Principal != "" {
		t.Errorf("the migration synthesised a principal %q; a native credential is minted, not synthesised (§3.3)", alfred.Principal)
	}
	if alfred.Delivery != agentapi.DeliveryPush || alfred.ExecutionProfile != "platform-only" {
		t.Errorf("backfilled defaults = delivery %q, profile %q", alfred.Delivery, alfred.ExecutionProfile)
	}
	if alfred.Endpoint != "" || !alfred.LastHeartbeat.IsZero() {
		t.Errorf("a native runtime carries an endpoint %q or a heartbeat %v", alfred.Endpoint, alfred.LastHeartbeat)
	}

	hobson, err := old.GetButlerRuntimeByName("hobson")
	if err != nil {
		t.Fatal(err)
	}
	if hobson.Health != agentapi.HealthRevoked {
		t.Errorf("a retired agent's runtime is %q, want revoked — nothing may act for it", hobson.Health)
	}
}

// A new Agent gets a runtime record from the platform, and reading it
// by name and by seq are the same read.
func TestARuntimeRecordIsWrittenAndReadPerAgent(t *testing.T) {
	db, b := runtimeFixture(t)
	if _, err := db.GetButlerRuntime(b.Seq); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a fresh agent already has a runtime record (err=%v); the platform writes it, not the migration", err)
	}
	if err := db.SetButlerRuntime(b.Seq, ButlerRuntime{Type: agentapi.RuntimeNative}, "mika"); err != nil {
		t.Fatal(err)
	}
	bySeq, err := db.GetButlerRuntime(b.Seq)
	if err != nil {
		t.Fatal(err)
	}
	byName, err := db.GetButlerRuntimeByName("alfred")
	if err != nil {
		t.Fatal(err)
	}
	if bySeq != byName {
		t.Fatalf("by seq %+v, by name %+v", bySeq, byName)
	}
	if bySeq.Health != agentapi.HealthUnregistered {
		t.Errorf("a configured runtime starts %q, want unregistered — health is observed, never configured", bySeq.Health)
	}
	if bySeq.UpdatedBy != "mika" || bySeq.UpdatedAt.IsZero() {
		t.Errorf("the write is not stamped: %+v", bySeq)
	}

	// Replacing the runtime (§3.7) keeps the Agent and rewrites the
	// configured half only.
	if err := db.SetButlerRuntime(b.Seq, ButlerRuntime{
		Type: agentapi.RuntimeHermes, Endpoint: "https://hermes-alfred.internal/",
		Delivery: agentapi.DeliveryPull, ProcessingClass: agentapi.ProcessingLocalOnly,
		ExecutionProfile: "sandboxed",
	}, "mika"); err != nil {
		t.Fatal(err)
	}
	rt, err := db.GetButlerRuntime(b.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Type != agentapi.RuntimeHermes || rt.Endpoint != "https://hermes-alfred.internal/" ||
		rt.Delivery != agentapi.DeliveryPull || rt.ProcessingClass != agentapi.ProcessingLocalOnly ||
		rt.ExecutionProfile != "sandboxed" {
		t.Errorf("replacing the runtime did not take: %+v", rt)
	}
	rec := rt.Record()
	if rec.Type != agentapi.RuntimeHermes || rec.Negotiated.Delivery != agentapi.DeliveryPull ||
		rec.Negotiated.ProcessingClass != agentapi.ProcessingLocalOnly || rec.ExecutionProfileRef != "sandboxed" {
		t.Errorf("the API record does not carry the stored facts: %+v", rec)
	}

	// The closed sets are closed.
	for _, bad := range []ButlerRuntime{
		{Type: "chatgpt"},
		{Type: agentapi.RuntimeNative, Endpoint: "https://somewhere/"},
		{Type: agentapi.RuntimeHermes, Delivery: "carrier-pigeon"},
		{Type: agentapi.RuntimeHermes, ProcessingClass: "wherever"},
	} {
		if err := db.SetButlerRuntime(b.Seq, bad, "mika"); err == nil {
			t.Errorf("SetButlerRuntime(%+v) accepted", bad)
		}
	}
	if err := db.SetButlerRuntime(b.Seq, ButlerRuntime{Type: agentapi.RuntimeNative}, ""); err == nil {
		t.Error("a runtime change with nobody named as its author was accepted")
	}
}

// Health moves through the closed set, a heartbeat is a sign of life
// and not a health claim, and a revoked runtime cannot heartbeat.
func TestRuntimeHealthTransitions(t *testing.T) {
	db, b := runtimeFixture(t)
	if err := db.SetRuntimeHealth(b.Seq, agentapi.HealthConnected); !errors.Is(err, ErrNotFound) {
		t.Fatalf("setting health on an agent with no runtime = %v, want not found", err)
	}
	if err := db.SetButlerRuntime(b.Seq, ButlerRuntime{Type: agentapi.RuntimeNative}, "mika"); err != nil {
		t.Fatal(err)
	}
	for _, h := range []agentapi.RuntimeHealth{
		agentapi.HealthConnected, agentapi.HealthDegraded, agentapi.HealthOffline, agentapi.HealthConnected,
	} {
		if err := db.SetRuntimeHealth(b.Seq, h); err != nil {
			t.Fatalf("SetRuntimeHealth(%s): %v", h, err)
		}
		if rt, _ := db.GetButlerRuntime(b.Seq); rt.Health != h {
			t.Fatalf("health is %q after setting %q", rt.Health, h)
		}
	}
	if err := db.SetRuntimeHealth(b.Seq, "fine"); err == nil {
		t.Error("an invented health state was accepted")
	}

	if err := db.TouchRuntimeHeartbeat(b.Seq); err != nil {
		t.Fatal(err)
	}
	rt, _ := db.GetButlerRuntime(b.Seq)
	if rt.LastHeartbeat.IsZero() {
		t.Error("the heartbeat was not recorded")
	}
	if rt.Health != agentapi.HealthConnected {
		t.Errorf("a heartbeat changed health to %q; it carries no health claim", rt.Health)
	}
	if rec := rt.Record(); rec.LastHeartbeat.IsZero() || rec.Health != agentapi.HealthConnected {
		t.Errorf("the API record lost the heartbeat or health: %+v", rec)
	}

	if err := db.SetRuntimeHealth(b.Seq, agentapi.HealthRevoked); err != nil {
		t.Fatal(err)
	}
	if err := db.TouchRuntimeHeartbeat(b.Seq); !errors.Is(err, ErrNotFound) {
		t.Errorf("a revoked runtime's heartbeat = %v, want refused", err)
	}
}

// The identity binding (§4.1): principal → Agent, immutable for the
// principal's life. Rebinding is refused whether asked through the
// accessor or written to the row.
func TestARuntimePrincipalIsBoundToOneAgentForLife(t *testing.T) {
	db, alfred := runtimeFixture(t)
	newButler(t, db, "hobson", "kitchen-box")
	hobson, err := db.GetButler("hobson")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []Butler{alfred, hobson} {
		if err := db.SetButlerRuntime(b.Seq, ButlerRuntime{Type: agentapi.RuntimeNative}, "mika"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreatePrincipal("runtime-alfred-1", RoleButler, ""); err != nil {
		t.Fatal(err)
	}

	// Unknown principal, and one that is not a runtime principal.
	if _, ok, err := db.RuntimePrincipalButler("runtime-alfred-1"); err != nil || ok {
		t.Fatalf("an unbound principal resolves to an agent (ok=%v err=%v)", ok, err)
	}
	if _, ok, err := db.RuntimePrincipalButler("nobody"); err != nil || ok {
		t.Fatalf("an unknown principal resolves to an agent (ok=%v err=%v)", ok, err)
	}
	if err := db.BindRuntimePrincipal("nobody", alfred.Seq); !errors.Is(err, ErrNotFound) {
		t.Errorf("binding a principal nobody minted = %v, want not found", err)
	}

	if err := db.BindRuntimePrincipal("runtime-alfred-1", alfred.Seq); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.RuntimePrincipalButler("runtime-alfred-1")
	if err != nil || !ok || got != alfred.Seq {
		t.Fatalf("resolved to %d (ok=%v err=%v), want alfred (%d)", got, ok, err, alfred.Seq)
	}
	if rt, _ := db.GetButlerRuntime(alfred.Seq); rt.Principal != "runtime-alfred-1" {
		t.Errorf("binding did not make the principal the current credential: %q", rt.Principal)
	}
	if rec := (ButlerRuntime{Principal: "runtime-alfred-1"}).Record(); rec.CredentialRef != "runtime-alfred-1" {
		t.Errorf("credentialRef = %q", rec.CredentialRef)
	}

	// Same agent again converges; another agent is refused.
	if err := db.BindRuntimePrincipal("runtime-alfred-1", alfred.Seq); err != nil {
		t.Errorf("re-binding to the same agent = %v, want a no-op", err)
	}
	if err := db.BindRuntimePrincipal("runtime-alfred-1", hobson.Seq); !errors.Is(err, ErrRuntimePrincipalBound) {
		t.Errorf("binding alfred's runtime principal to hobson = %v, want %v", err, ErrRuntimePrincipalBound)
	}
	if got, _, _ := db.RuntimePrincipalButler("runtime-alfred-1"); got != alfred.Seq {
		t.Errorf("after the refused rebind the principal resolves to %d, want alfred", got)
	}
	if _, err := db.sql.Exec(`UPDATE runtime_principals SET butler = ? WHERE principal = ?`,
		hobson.Seq, "runtime-alfred-1"); err == nil {
		t.Error("the row itself accepted a rebind; the trigger is the guard against anything that bypasses the accessor")
	}

	// A credential that is not butler-role cannot be a runtime.
	if err := db.CreatePrincipal("someone-admin", RoleAdmin, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.BindRuntimePrincipal("someone-admin", hobson.Seq); err == nil {
		t.Error("an admin principal was bound as a runtime; a runtime never holds more than the butler role (ADR-W024 A5)")
	}

	// Rotation is a second binding to the same agent, and history keeps
	// both.
	if err := db.CreatePrincipal("runtime-alfred-2", RoleButler, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.BindRuntimePrincipal("runtime-alfred-2", alfred.Seq); err != nil {
		t.Fatal(err)
	}
	all, err := db.ListRuntimePrincipals(alfred.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0] != "runtime-alfred-1" || all[1] != "runtime-alfred-2" {
		t.Errorf("history = %v, want both credentials oldest first", all)
	}
	if rt, _ := db.GetButlerRuntime(alfred.Seq); rt.Principal != "runtime-alfred-2" {
		t.Errorf("the current credential is %q after rotation", rt.Principal)
	}
	if none, _ := db.ListRuntimePrincipals(hobson.Seq); len(none) != 0 {
		t.Errorf("hobson lists %v", none)
	}
}
