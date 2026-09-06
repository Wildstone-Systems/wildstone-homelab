package runtimedb

import (
	"path/filepath"
	"testing"
)

// testDB opens a real database in a temp dir, so the migration under
// test actually runs rather than being assumed.
func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// ADR-W017 A9: provider config is runtime state. These assert the two
// properties the amendment rests on.

func TestButlerConfigAbsentIsNotAnError(t *testing.T) {
	db := testDB(t)
	// A deployment that has never opened the Console has no stored
	// config and is correctly running on its startup flags. Reporting
	// that as an error would make the normal state look broken.
	c, ok, err := db.GetButlerConfig()
	if err != nil {
		t.Fatalf("absent config reported as an error: %v", err)
	}
	if ok || c.Provider != "" {
		t.Errorf("want absent, got ok=%v %+v", ok, c)
	}
}

func TestButlerConfigRoundTrips(t *testing.T) {
	db := testDB(t)
	want := ButlerConfig{Provider: "ollama", Model: "qwen3.5:4b", Endpoint: "http://192.168.1.99:11434", UpdatedBy: "admin"}
	if err := db.SetButlerConfig(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.GetButlerConfig()
	if err != nil || !ok {
		t.Fatalf("not stored: ok=%v err=%v", ok, err)
	}
	if got.Provider != want.Provider || got.Model != want.Model || got.Endpoint != want.Endpoint {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("updated_at not recorded; an operator cannot tell when this was last changed")
	}
}

// The property that keeps a degraded mode meaningful: a probe verdict
// describes one provider and model, so changing either must discard it.
// Carrying it over would let a model inherit a pass it never earned.
func TestChangingConfigClearsTheProbeVerdict(t *testing.T) {
	db := testDB(t)
	if err := db.SetButlerConfig(ButlerConfig{Provider: "ollama", Model: "small"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordButlerProbe("full", "passed"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerConfig(ButlerConfig{Provider: "ollama", Model: "different"}); err != nil {
		t.Fatal(err)
	}
	got, _, err := db.GetButlerConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.ProbeMode != "" || !got.ProbedAt.IsZero() {
		t.Errorf("a new model inherited the previous model's probe verdict: mode=%q at=%v", got.ProbeMode, got.ProbedAt)
	}
}

// One deployment, one Butler configuration. A second row would be a
// second answer to a question that has one.
func TestButlerConfigHoldsOneRow(t *testing.T) {
	db := testDB(t)
	for _, m := range []string{"a", "b", "c"} {
		if err := db.SetButlerConfig(ButlerConfig{Provider: "ollama", Model: m}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := db.sql.QueryRow(`SELECT count(*) FROM butler_config`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("butler_config holds %d rows; it must hold exactly one", n)
	}
	got, _, _ := db.GetButlerConfig()
	if got.Model != "c" {
		t.Errorf("the latest write did not win: %q", got.Model)
	}
}
