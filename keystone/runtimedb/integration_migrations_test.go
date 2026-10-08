package runtimedb

// Current-segment integration: main's mailbox and feature's workspace histories
// allocated overlapping numeric prefixes independently. The actual full names,
// not the prefixes, identify immutable migrations. Inventories pin both source
// commits without copying or renumbering their SQL.
import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func integrationBaseline(t *testing.T, name string) fstest.MapFS {
	t.Helper()
	manifest, err := os.ReadFile("testdata/integration-" + name + "-migrations.sha256")
	if err != nil {
		t.Fatal(err)
	}
	out := fstest.MapFS{}
	for _, line := range strings.Split(string(manifest), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("invalid migration inventory: %q", line)
		}
		body, err := migrationFS.ReadFile(fields[1])
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%x", sha256.Sum256(body)) != fields[0] {
			t.Fatalf("baseline migration changed: %s", fields[1])
		}
		out[fields[1]] = &fstest.MapFile{Data: body}
	}
	return out
}

func TestCurrentSegmentMigrationCompatibility(t *testing.T) {
	replaysMigrations(t)
	for _, baseline := range []string{"fresh", "main-mailbox", "feature-workspace"} {
		t.Run(baseline, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "integration.db")
			db, err := openUnmigrated(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			var retained map[string]string
			if baseline != "fresh" {
				if err := db.migrateFrom(integrationBaseline(t, baseline), ""); err != nil {
					t.Fatal(err)
				}
				retained = seedIntegrationBaseline(t, db, baseline)
			}
			if err := db.migrate(); err != nil {
				t.Fatal(err)
			}
			assertIntegrationSchema(t, db)
			assertIntegrationRows(t, db, retained)
			if err := db.migrate(); err != nil {
				t.Fatal(err)
			}
			assertIntegrationSchema(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			assertIntegrationSchema(t, db)
			assertIntegrationRows(t, db, retained)
		})
	}
}

func assertIntegrationSchema(t *testing.T, db *DB) {
	t.Helper()
	want, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.AppliedMigrations()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("full filename inventory: got=%v want=%v err=%v", got, want, err)
	}
	var count int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil || count != len(want) {
		t.Fatalf("each full filename once: %d/%d %v", count, len(want), err)
	}
	// Each competing chain must exist, including both migrations at every
	// overlapping 0097–0101 prefix. The inventory equality above checks all names.
	for _, table := range []string{"agent_workspaces", "search_providers", "workspace_task_sources", "workspace_write_sessions", "research_budgets", "integration_secrets", "agent_subscription_schedules", "mail_candidates", "mail_reviews"} {
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("missing %s: %d %v", table, count, err)
		}
	}
	rows, err := db.sql.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violation after combined migration/reopen")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`PRAGMA foreign_keys`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("foreign keys disabled: %d %v", count, err)
	}
}

// Seed retained identities/task data on both historical schemas and representative
// durable mail review/effect or committed workspace publication data. These are
// synthetic metadata fixtures; no household content or credential is copied.
func seedIntegrationBaseline(t *testing.T, db *DB, baseline string) map[string]string {
	t.Helper()
	if err := db.RecordButlerMember("integration-person", RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("integration-person", "fixture-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModel(Model{Name: "integration-model", Kind: ModelLocal, Provider: "ollama", ProviderModel: "fixture", Readiness: ModelReady, Enabled: true}, "fixture-admin"); err != nil {
		t.Fatal(err)
	}
	agent, err := db.CreateButler(Butler{Name: "integration-agent", Directive: "fixture", Surface: ButlerSurfaceAdmin, AgentClass: AgentClassOwner, Model: "integration-model"}, "fixture-admin")
	if err != nil {
		t.Fatal(err)
	}
	person, err := db.GetButlerMember("integration-person")
	if err != nil {
		t.Fatal(err)
	}
	// Seed the historical task columns, before S03 adds origin_conversation.
	res, err := db.sql.Exec(`INSERT INTO butler_tasks(created_at,updated_at,kind,state,author_person_id,owner_person_id,surface,agent_class,audience,objective,completion_criteria,retention,envelope_version,butler) VALUES(?,?,'conversation','pending',?,?,'admin','owner','aud_owner','synthetic integration fixture','fixture retained','manual',1,'integration-agent')`, now(), now(), person.PersonID, person.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	retained := map[string]string{
		`SELECT person_id FROM butler_members WHERE member='integration-person'`:             person.PersonID,
		`SELECT objective FROM butler_tasks WHERE objective='synthetic integration fixture'`: "synthetic integration fixture",
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.sql.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	if baseline == "feature-workspace" {
		w := Workspace{ID: "integration-workspace", PersonID: person.PersonID, AgentID: agent}
		if err := db.CreateWorkspace(w); err != nil {
			t.Fatal(err)
		}
		p := workspaceIntent(t, db, w, task, "integration-report", "integration-r1", "", 12)
		// Historical S02 fixture: execute its legal metadata transitions directly.
		// The current S05 accessor also looks up sessions absent from this baseline.
		for _, state := range []string{"prepared", "bytes_published", "committed"} {
			exec(`UPDATE workspace_publications SET state=? WHERE id=?`, state, p.ID)
		}
		exec(`UPDATE workspace_artifacts SET state='committed',current_revision=? WHERE id=?`, p.RevisionID, p.ArtifactID)
		exec(`UPDATE workspace_reservations SET state='consumed' WHERE id=?`, p.ReservationID)
		exec(`INSERT INTO workspace_receipts(id,publication_id,recorded_at) VALUES('integration-receipt',?,?)`, p.ID, now())
		retained[`SELECT current_revision FROM workspace_artifacts WHERE id='integration-report'`] = "integration-r1"
		retained[`SELECT state FROM workspace_publications WHERE id='publish-integration-r1'`] = "committed"
		retained[`SELECT publication_id FROM workspace_receipts WHERE id='integration-receipt'`] = p.ID
	} else {
		conn, err := db.CreateConnection(IntegrationConnection{Name: "integration-mail", Adapter: "fixture-mail", Ownership: OwnershipPersonal, Owner: "integration-person", CreatedBy: "fixture-admin"})
		if err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO mail_monitor_messages(connection,fingerprint,person_id,state,first_seen_at,processed_at) VALUES(?,'mfp1_integration',?,'candidate',?,?)`, conn, person.PersonID, now(), now())
		exec(`INSERT INTO mail_candidates(id,person_id,connection,source_ref,fingerprint,source,target_calendar,state,created_at,updated_at,expires_at) VALUES('mc_integration',?,?,'fixture-ref','mfp1_integration','ics','fixture-calendar','proposed',?,?,?)`, person.PersonID, conn, now(), now(), now())
		exec(`INSERT INTO mail_candidate_revisions(candidate,revision,title,author,created_at) VALUES('mc_integration',1,'Synthetic meeting','monitor',?)`, now())
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte("synthetic event fields")))
		exec(`INSERT INTO mail_reviews(id,candidate,person_id,revision,kind,target_calendar,fields_digest,authorization_digest,subscription_version,route,availability,issued_at,expires_at,state) VALUES('mr_integration','mc_integration',?,1,'create','fixture-calendar',?,'fixture-authorization',1,'notice','not_checked',?,?,'open')`, person.PersonID, digest, now(), now())
		exec(`INSERT INTO mail_candidate_effects(candidate,revision,review,kind,state,calendar,uid,fields_digest,actor,started_at) VALUES('mc_integration',1,'mr_integration','create','dispatching','fixture-calendar','fixture-event',?,'integration-person',?)`, digest, now())
		retained[`SELECT title FROM mail_candidate_revisions WHERE candidate='mc_integration'`] = "Synthetic meeting"
		retained[`SELECT state FROM mail_reviews WHERE id='mr_integration'`] = "open"
		retained[`SELECT uid FROM mail_candidate_effects WHERE candidate='mc_integration'`] = "fixture-event"
		retained[`SELECT state FROM mail_monitor_messages WHERE fingerprint='mfp1_integration'`] = "candidate"
	}
	return retained
}

func assertIntegrationRows(t *testing.T, db *DB, retained map[string]string) {
	t.Helper()
	for q, want := range retained {
		var got string
		if err := db.sql.QueryRow(q).Scan(&got); err != nil || got != want {
			t.Fatalf("retained record: %q got=%q want=%q err=%v", q, got, want, err)
		}
	}
}
