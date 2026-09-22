package runtimedb

import (
	"path/filepath"
	"testing"
)

func TestMigrationChainAppliesFromEmpty(t *testing.T) {
	db := open(t)
	assertNoUnusedTranscriptSettings(t, db)
}

func TestMigrationChainAppliesFromPreEpicSnapshot(t *testing.T) {
	old, err := openUnmigrated(filepath.Join(t.TempDir(), "pre-epic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { old.Close() })
	if err := old.migrateThrough("migrations/0038_index_conversations.sql"); err != nil {
		t.Fatal(err)
	}
	// A household row written under the old schema, with non-default 0038
	// values. A no-row upgrade would not prove the applied-history path.
	if _, err := old.sql.Exec(`INSERT INTO butler_runtime_settings
 (id,household_rounds,operator_rounds,max_tool_calls,stall_seconds,retry_limit,
  observation_ttl_seconds,quiet_start_min,quiet_end_min,timezone,updated_at,
  updated_by,index_conversations,transcript_retention_days)
 VALUES(1,16,24,64,60,2,0,1320,420,'America/Chicago','2026-09-01T00:00:00Z',
  'owner',0,30)`); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := old.sql.QueryRow(`SELECT count(*) FROM butler_runtime_settings WHERE id=1 AND index_conversations=0 AND transcript_retention_days=30`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("pre-migration settings fixture missing: rows=%d err=%v", rows, err)
	}
	if err := old.migrate(); err != nil {
		t.Fatal(err)
	}
	assertNoUnusedTranscriptSettings(t, old)
	settings, stored, err := old.GetButlerSettings()
	if err != nil || !stored || settings.Timezone != "America/Chicago" {
		t.Fatalf("unrelated household settings changed: %+v stored=%t err=%v", settings, stored, err)
	}
}

func assertNoUnusedTranscriptSettings(t *testing.T, db *DB) {
	t.Helper()
	for _, name := range []string{"index_conversations", "transcript_retention_days"} {
		var count int
		if err := db.sql.QueryRow(`SELECT count(*) FROM pragma_table_info('butler_runtime_settings') WHERE name=?`, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("unused setting %s remains in schema", name)
		}
	}
	var integrity string
	if err := db.sql.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check: %s", integrity)
	}
}
