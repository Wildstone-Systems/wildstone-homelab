package runtimedb

import (
	"path/filepath"
	"testing"
)

func disclosureFixture(t *testing.T) (*DB, Connection, string) {
	t.Helper()
	db := butlerFixture(t)
	at, task := disclosureSetup(t, db)
	return db, at, task
}

func disclosureSetup(t *testing.T, db *DB) (Connection, string) {
	t.Helper()
	at := connect(t, db, "memory-agent", "telegram", ButlerSurfaceFamily)
	if err := db.GrantButlerAccess("memory-agent", "sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE connection_disclosure SET retention='provider' WHERE connection=?`, at.Seq); err != nil {
		t.Fatal(err)
	}
	task, err := db.InsertButlerTask(ButlerTask{Kind: ButlerTaskConversation, Author: "sam", Owner: "sam", Surface: ButlerSurfaceFamily, AgentClass: AgentClassMember, Audience: ButlerAudiencePrivate, Objective: "remember", OriginConnection: at.Name(), OriginConnectionRef: at.Seq})
	if err != nil {
		t.Fatal(err)
	}
	return at, task
}

func disclosedMemory(t *testing.T, db *DB, task string) int64 {
	t.Helper()
	m := scopedFact(t, MemoryScopePersonal, "memory-agent", "sam")
	m.Kind = MemoryPreference
	m.Content = "jasmine disclosure canary"
	m.SourceKind = "conversation"
	m.SourceID = task
	seq, err := db.RecordMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	indexDisclosure(t, db, seq)
	return seq
}

func indexDisclosure(t *testing.T, db *DB, seq int64) {
	t.Helper()
	m, err := db.GetMemory(seq)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.IndexDocumentInto(IndexDocument{MemorySeq: seq, SourceKind: m.SourceKind, SourceID: m.SourceID, Content: m.Content, Owner: m.Owner, Audience: m.Audience, PermissionLabel: m.PermissionLabel, Butler: m.Butler})
	if err != nil {
		t.Fatal(err)
	}
}

func disclosurePaths(t *testing.T, db *DB, read *MemoryReadContext, want int) {
	t.Helper()
	t.Run("retrieval", func(t *testing.T) {
		hits, err := db.SearchIndex(IndexQuery{Reader: "sam", Butler: "memory-agent", Text: "jasmine", Limit: 1, ReadContext: read})
		if err != nil || len(hits) != want {
			t.Fatalf("recall got %d want %d: %v", len(hits), want, err)
		}
	})
	t.Run("context", func(t *testing.T) {
		hits, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "memory-agent", Kind: MemoryPreference, Limit: 1, ReadContext: read})
		if err != nil || len(hits) != want {
			t.Fatalf("context got %d want %d: %v", len(hits), want, err)
		}
	})
}

func TestDisclosurePolicyAppliesOnTopOfAudience(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"unknown retention", `UPDATE connection_disclosure SET retention='unknown'`},
		{"changed encryption", `UPDATE connection_disclosure SET encryption='end_to_end'`},
		{"changed reach", `UPDATE connection_disclosure SET reach='local'`},
		{"changed surface", `UPDATE connection_disclosure SET surface='trusted_ui'`},
		{"revoked connection", `UPDATE butler_addresses SET revoked_at='revoked'`},
		{"missing classification", `DELETE FROM connection_disclosure`},
		{"disabled agent", `UPDATE butlers SET enabled=0`},
		{"narrowed admission", `UPDATE connection_admission SET admits='owners'`},
		{"group admission", `UPDATE connection_admission SET subject_kind='group'`},
		{"revoked agent access", `UPDATE butler_access SET revoked_at='revoked'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, at, task := disclosureFixture(t)
			disclosedMemory(t, db, task)
			read := db.BindMemoryRead("sam", "memory-agent", MemoryDestination{ConnectionRef: at.Seq})
			disclosurePaths(t, db, read, 1)
			if _, err := db.sql.Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
			disclosurePaths(t, db, read, 0)
		})
	}
}

func TestOneFilterGovernsRetrievalAndContextAssembly(t *testing.T) {
	db, at, task := disclosureFixture(t)
	disclosedMemory(t, db, task)
	// A newer inaccessible record must not crowd the authorized one out of
	// either LIMIT 1 query. These independently exercise the two callers.
	disclosedMemory(t, db, "unknown-source")
	read := db.BindMemoryRead("sam", "memory-agent", MemoryDestination{ConnectionRef: at.Seq})
	disclosurePaths(t, db, read, 1)
	if err := db.SetAgentMemoryRestriction("memory-agent", MemoryScopePersonal, true, "mika"); err != nil {
		t.Fatal(err)
	}
	disclosurePaths(t, db, read, 0)
}

func TestMemoryDisclosureCannotBeLaunderedThroughLineage(t *testing.T) {
	for _, operation := range []string{"derive", "correct", "promote", "transfer"} {
		t.Run(operation, func(t *testing.T) {
			db, at, task := disclosureFixture(t)
			var source int64
			if operation == "transfer" {
				local := scopedFact(t, MemoryScopeAgentLocal, "memory-agent", "sam")
				local.Kind = MemoryPreference
				local.Content = "jasmine local canary"
				var err error
				source, err = db.RecordMemory(local)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				source = disclosedMemory(t, db, "unknown-private-origin")
			}
			m, err := db.GetMemory(source)
			if err != nil {
				t.Fatal(err)
			}
			m.SourceID = task
			m.Content = "jasmine transformed canary"
			var seq int64
			switch operation {
			case "derive":
				seq, err = db.DeriveMemory(m, []int64{source})
			case "correct":
				seq, err = db.CorrectMemory(source, m, "sam")
			case "promote":
				seq, err = db.PromotePersonalMemory(source, "sam")
			case "transfer":
				// A local source moves Agents but keeps the origin boundary.
				newButler(t, db, "recipient", "kitchen-box")
				err = db.RetireButlerWithMemoryTransfer("memory-agent", "recipient", "mika")
				seq = source
			}
			if err != nil {
				t.Fatal(err)
			}
			indexDisclosure(t, db, seq)
			if operation == "transfer" {
				if err = db.GrantButlerAccess("recipient", "sam", "mika"); err != nil {
					t.Fatal(err)
				}
				destination := connect(t, db, "recipient", "matrix", ButlerSurfaceFamily, "recipient-bot")
				if _, err = db.sql.Exec(`UPDATE connection_disclosure SET retention='provider' WHERE connection=?`, destination.Seq); err != nil {
					t.Fatal(err)
				}
				for _, trusted := range []bool{true, false} {
					d := MemoryDestination{ConnectionRef: destination.Seq}
					want := 0
					if trusted {
						d = MemoryDestination{BuiltIn: true}
						want = 1
					}
					r := db.BindMemoryRead("sam", "recipient", d)
					hits, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "recipient", ReadContext: r})
					if err != nil || len(hits) != want {
						t.Fatalf("transferred context trusted=%v: %v %v", trusted, hits, err)
					}
					index, err := db.SearchIndex(IndexQuery{Reader: "sam", Butler: "recipient", ReadContext: r, Text: "jasmine"})
					if err != nil || len(index) != want {
						t.Fatalf("transferred recall trusted=%v: %v %v", trusted, index, err)
					}
				}
				return
			}
			disclosurePaths(t, db, db.BindMemoryRead("sam", "memory-agent", MemoryDestination{ConnectionRef: at.Seq}), 0)
			disclosurePaths(t, db, db.BindMemoryRead("sam", "memory-agent", MemoryDestination{BuiltIn: true}), 1)
		})
	}
}

func TestRememberedContinuationInheritsAllHistoryOrigins(t *testing.T) {
	db, at, _ := disclosureFixture(t)
	session, err := db.InsertButlerSession(ButlerSession{Principal: "butler", ActingFor: "sam", Butler: "memory-agent", Surface: ButlerSurfaceFamily, Mode: ButlerModeFull, InboundConnection: "console"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.NoteButlerSessionConnection(session, "inbound", at.Name(), at.Seq); err != nil {
		t.Fatal(err)
	}
	task, err := db.InsertButlerTask(ButlerTask{Kind: ButlerTaskConversation, Author: "sam", Owner: "sam", Surface: ButlerSurfaceFamily, AgentClass: AgentClassMember, Audience: ButlerAudiencePrivate, Objective: "remember", OriginConnection: at.Name(), OriginConnectionRef: at.Seq, SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	disclosedMemory(t, db, task)
	disclosurePaths(t, db, db.BindMemoryRead("sam", "memory-agent", MemoryDestination{ConnectionRef: at.Seq}), 0)
	disclosurePaths(t, db, db.BindMemoryRead("sam", "memory-agent", MemoryDestination{BuiltIn: true}), 1)
}

func TestStandaloneIndexedDocumentCannotBypassDisclosure(t *testing.T) {
	db, at, _ := disclosureFixture(t)
	_, err := db.IndexDocumentInto(IndexDocument{SourceKind: "document", SourceID: "a", Content: "jasmine unclassified document", Owner: "sam", Audience: ButlerAudiencePrivate, PermissionLabel: "household"})
	if err != nil {
		t.Fatal(err)
	}
	for _, trusted := range []bool{true, false} {
		d := MemoryDestination{BuiltIn: true}
		want := 1
		if !trusted {
			d = MemoryDestination{ConnectionRef: at.Seq}
			want = 0
		}
		hits, err := db.SearchIndex(IndexQuery{Reader: "sam", Butler: "memory-agent", Text: "jasmine", ReadContext: db.BindMemoryRead("sam", "memory-agent", d)})
		if err != nil || len(hits) != want {
			t.Fatalf("document trusted=%v: %v %v", trusted, hits, err)
		}
	}
}

func TestMemoryDisclosureMigrationCapturesLegacyOrigins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "before.db")
	old, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err = old.migrateThrough("migrations/0067_memory_scopes.sql"); err != nil {
		t.Fatal(err)
	}
	if err = old.UpsertModel(Model{Name: "kitchen-box", Kind: ModelLocal, Provider: "ollama", ProviderModel: "llama", Readiness: ModelReady, Enabled: true}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ name, role string }{{"mika", RoleOwner}, {"sam", RoleMember}} {
		if err = old.RecordButlerMember(p.name, p.role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err = old.AdmitButlerMember(p.name, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	// Write the historical task shape, not the current store's newer columns.
	at := connect(t, old, "memory-agent", "telegram", ButlerSurfaceFamily)
	if err := old.GrantButlerAccess("memory-agent", "sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := old.sql.Exec(`UPDATE connection_disclosure SET retention='provider' WHERE connection=?`, at.Seq); err != nil {
		t.Fatal(err)
	}
	person, err := resolvePersonID(old.sql, "sam")
	if err != nil {
		t.Fatal(err)
	}
	result, err := old.sql.Exec(`INSERT INTO butler_tasks(created_at,updated_at,kind,state,author_person_id,owner_person_id,surface,agent_class,audience,objective,completion_criteria,retention,envelope_version,origin_connection,origin_connection_ref) VALUES(?,?,'conversation','pending',?,?,'family','member','aud_owner','remember','','manual',1,?,?)`, now(), now(), person, person, at.Name(), at.Seq)
	if err != nil {
		t.Fatal(err)
	}
	taskSeq, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	task := butlerTaskID(taskSeq)
	seq := disclosedMemory(t, old, task)
	unknown := disclosedMemory(t, old, "legacy-unresolvable")
	if err = old.migrate(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = old.sql.QueryRow(`SELECT count(*) FROM memory_disclosure_origins WHERE memory IN (?,?)`, seq, unknown).Scan(&count); err != nil || count != 2 {
		t.Fatalf("backfill count=%d err=%v", count, err)
	}
	read := old.BindMemoryRead("sam", "memory-agent", MemoryDestination{ConnectionRef: at.Seq})
	disclosurePaths(t, old, read, 1)
	if _, err = old.sql.Exec(`UPDATE connection_disclosure SET encryption='end_to_end'`); err != nil {
		t.Fatal(err)
	}
	disclosurePaths(t, old, read, 0)
	if err = old.migrate(); err != nil {
		t.Fatal(err)
	}
	var after int
	if err = old.sql.QueryRow(`SELECT count(*) FROM memory_disclosure_origins`).Scan(&after); err != nil || after != count {
		t.Fatalf("migration was not idempotent: %d %v", after, err)
	}
}

func TestCompatibleExternalConnectionsCanShareAuthorizedMemory(t *testing.T) {
	db, _, task := disclosureFixture(t)
	disclosedMemory(t, db, task)
	other := connect(t, db, "memory-agent", "matrix", ButlerSurfaceFamily, "other-memory-bot")
	if _, err := db.sql.Exec(`UPDATE connection_disclosure SET retention='provider' WHERE connection=?`, other.Seq); err != nil {
		t.Fatal(err)
	}
	read := db.BindMemoryRead("sam", "memory-agent", MemoryDestination{ConnectionRef: other.Seq})
	disclosurePaths(t, db, read, 1)
	if _, err := db.sql.Exec(`UPDATE connection_disclosure SET encryption='end_to_end' WHERE connection=?`, other.Seq); err != nil {
		t.Fatal(err)
	}
	disclosurePaths(t, db, read, 0)
}

// A person's own verified one-to-one chat receives their memory wherever it
// was recorded, as it continues their conversations (agent-system-design
// §7). Without a binding the classification rule decides, and a room is
// never verified.
func TestAVerifiedChatReceivesMemoryRecordedElsewhere(t *testing.T) {
	db, at, _ := disclosureFixture(t)
	// Remembered on the trusted Console, not on this Connection.
	console, err := db.InsertButlerTask(ButlerTask{Kind: ButlerTaskConversation, Author: "sam", Owner: "sam",
		Surface: ButlerSurfaceFamily, AgentClass: AgentClassMember, Audience: ButlerAudiencePrivate,
		Objective: "remember", OriginConnection: "console"})
	if err != nil {
		t.Fatal(err)
	}
	disclosedMemory(t, db, console)
	destination := MemoryDestination{ConnectionRef: at.Seq}

	t.Run("unbound", func(t *testing.T) {
		disclosurePaths(t, db, db.BindMemoryRead("sam", "memory-agent", destination), 0)
	})
	bind(t, db, at, "sam", "sam-chat")
	t.Run("verified", func(t *testing.T) {
		disclosurePaths(t, db, db.BindMemoryRead("sam", "memory-agent", destination), 1)
	})
	t.Run("room", func(t *testing.T) {
		room := destination
		room.Group = true
		disclosurePaths(t, db, db.BindMemoryRead("sam", "memory-agent", room), 0)
	})
}
