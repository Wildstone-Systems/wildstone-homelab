package runtimedb

import (
	"testing"
)

func TestMigrationChainAppliesFromEmpty(t *testing.T) {
	db := open(t)
	assertNoUnusedTranscriptSettings(t, db)
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
