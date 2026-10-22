package runtimedb

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// Decode the proposed wire field so these tests fail behaviorally on the old
// store, rather than failing to compile before the scope field exists.
func scopedFact(t *testing.T, scope, agent, owner string) MemoryRecord {
	t.Helper()
	m := fact("scope regression fact", owner, ButlerAudienceShared)
	if owner != HouseholdOwner {
		m.Audience = ButlerAudiencePrivate
	}
	m.Butler = agent
	if err := json.Unmarshal([]byte(`{"scope":"`+scope+`"}`), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestForgetSurvivesBackupRestore(t *testing.T) {
	db := butlerFixture(t)
	var originals []int64
	for _, scope := range []string{MemoryScopePersonal, MemoryScopeHousehold, MemoryScopeAgentLocal} {
		owner := "sam"
		if scope == MemoryScopeHousehold {
			owner = HouseholdOwner
		}
		m := scopedFact(t, scope, "health", owner)
		m.Content = scope + " jasmine"
		id, err := db.RecordMemory(m)
		if err != nil {
			t.Fatal(err)
		}
		originals = append(originals, id)
		m.Content = "derived " + m.Content
		first, err := db.DeriveMemory(m, []int64{id})
		if err != nil {
			t.Fatal(err)
		}
		m.Content = "twice " + m.Content
		second, err := db.DeriveMemory(m, []int64{first})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.IndexDocumentInto(IndexDocument{SourceKind: "memory", SourceID: scope, MemorySeq: second, Owner: owner, Audience: m.Audience, PermissionLabel: "household", Content: m.Content}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if _, err := db.sql.Exec(`VACUUM INTO ?`, snapshot); err != nil {
		t.Fatal(err)
	}
	for _, id := range originals {
		if err := db.ForgetMemory(id, "sam"); err != nil {
			t.Fatal(err)
		}
	}
	restored, err := Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.EnterRestoreQuarantine("scope restore test", time.Now()); err != nil {
		t.Fatal(err)
	}
	rows, err := db.sql.Query(`SELECT subject_kind,subject_id,action,at,actor,detail FROM deletion_journal WHERE subject_kind='memory'`)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		var kind, id, action, at, actor, detail string
		if err := rows.Scan(&kind, &id, &action, &at, &actor, &detail); err != nil {
			t.Fatal(err)
		}
		if _, err := restored.sql.Exec(`INSERT INTO deletion_journal(subject_kind,subject_id,action,at,actor,detail) VALUES(?,?,?,?,?,?)`, kind, id, action, at, actor, detail); err != nil {
			t.Fatal(err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if count == 0 {
		t.Fatal("forget did not durably journal its decision")
	}
	if err := restored.AssertJournalCurrent("mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.ReconcileRestore(); err != nil {
		t.Fatal(err)
	}
	if err := restored.ReleaseRestoreQuarantine("mika"); err != nil {
		t.Fatal(err)
	}
	got, err := restored.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "health"})
	if err != nil || len(got) != 0 {
		t.Fatalf("restored memory resurfaced: %v, %v", got, err)
	}
	hits, err := restored.SearchIndex(IndexQuery{Reader: "sam", Butler: "health", Text: "jasmine"})
	if err != nil || len(hits) != 0 {
		t.Fatalf("restored search resurfaced: %v, %v", hits, err)
	}
}

func TestDerivedRecordNeverWidensBeyondStrictestSource(t *testing.T) {
	db := butlerFixture(t)
	sam, _ := db.GetButlerMember("sam")
	adults := groupID(t, db, SeedAdults)
	if err := db.AddPersonToGroup(adults, sam.PersonID, "mika"); err != nil {
		t.Fatal(err)
	}
	group := scopedFact(t, MemoryScopeHousehold, "health", HouseholdOwner)
	group.Audience = ButlerTaskAudience(adultsAudience(t, db))
	a, err := db.RecordMemory(group)
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.RecordMemory(scopedFact(t, MemoryScopePersonal, "health", "sam"))
	if err != nil {
		t.Fatal(err)
	}
	m := scopedFact(t, MemoryScopeAgentLocal, "health", HouseholdOwner)
	id, err := db.DeriveMemory(m, []int64{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RemovePersonFromGroup(adults, sam.PersonID); err != nil {
		t.Fatal(err)
	}
	got, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "health"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.Seq == id {
			t.Fatal("private derivative bypassed its source's current group audience")
		}
	}
	exported, err := db.ExportMemoryFor("sam")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range exported {
		if r.Seq == id {
			t.Fatal("export bypassed inherited group constraint")
		}
	}
}

func TestHouseholdMemoryIsReadableAcrossAgents(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	newButler(t, db, "nutrition", "cloud")
	id, err := db.RecordMemory(scopedFact(t, "household", "health", HouseholdOwner))
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "nutrition"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Seq != id {
		t.Fatalf("Household memory did not cross Agents: %+v", got)
	}
}

func TestAgentLocalIsUnreadableByAnotherAgentByDefault(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	newButler(t, db, "nutrition", "cloud")
	_, err := db.RecordMemory(scopedFact(t, "agent_local", "health", HouseholdOwner))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		agent string
		want  int
	}{{"health", 1}, {"nutrition", 0}} {
		got, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: tc.agent})
		if err != nil || len(got) != tc.want {
			t.Fatalf("%s: got %v, %v", tc.agent, got, err)
		}
	}
}

func TestPersonalMemoryFollowsItsPersonAcrossAgents(t *testing.T) {
	db := butlerFixture(t)
	_, err := db.RecordMemory(scopedFact(t, "personal", "health", "sam"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		person string
		want   int
	}{{"sam", 1}, {"mika", 0}} {
		got, err := db.RetrieveMemory(MemoryQuery{Reader: tc.person, Butler: "nutrition"})
		if err != nil || len(got) != tc.want {
			t.Fatalf("%s: got %v, %v", tc.person, got, err)
		}
	}
}

func TestRetiredAgentsHouseholdMemoryRemainsReachable(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	_, err := db.RecordMemory(scopedFact(t, "household", "health", HouseholdOwner))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RetireButler("health", "mika"); err != nil {
		t.Fatal(err)
	}
	got, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "nutrition"})
	if err != nil || len(got) != 1 {
		t.Fatalf("retirement stranded memory: %v, %v", got, err)
	}
}

func TestPersonalToHouseholdPromotionRequiresTheOwningPerson(t *testing.T) {
	db := butlerFixture(t)
	id, err := db.RecordMemory(scopedFact(t, "personal", "health", "sam"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.PromotePersonalMemory(id, "mika"); err == nil {
		t.Fatal("an Owner approved someone else's Personal memory")
	}
	promoted, err := db.PromotePersonalMemory(id, "sam")
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetMemory(promoted)
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != MemoryScopeHousehold || got.SourceID != "bs-1" || got.Author != "sam" {
		t.Fatalf("lost promotion provenance: %+v", got)
	}
	entries, err := db.PendingMemoryDigest(false)
	if err != nil || len(entries) != 1 || entries[0].Memory != promoted {
		t.Fatalf("approved Household copy missing from digest: %+v, %v", entries, err)
	}
	if _, err := db.PromotePersonalMemory(id, "sam"); err == nil {
		t.Fatal("promotion replay created another copy")
	}
	if err := db.ForgetMemory(id, "sam"); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetMemory(promoted)
	if err != nil || got.Eligible() {
		t.Fatalf("forgotten source left a promoted copy: %+v, %v", got, err)
	}
}

func TestAgentDeletionTransfersLocalMemoryWithProvenance(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	newButler(t, db, "nutrition", "cloud")
	id, err := db.RecordMemory(scopedFact(t, "agent_local", "health", "sam"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RetireButlerWithMemoryTransfer("health", "nutrition", "sam"); err == nil {
		t.Fatal("Member transferred Agent memory")
	}
	if err := db.RetireButlerWithMemoryTransfer("health", "nutrition", "mika"); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetMemory(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Butler != "nutrition" || got.SourceID != "bs-1" || got.Owner != "sam" {
		t.Fatalf("transfer changed provenance or ownership: %+v", got)
	}
	var source, destination string
	if err := db.sql.QueryRow(`SELECT source_agent,destination_agent FROM memory_scope_events WHERE memory=?`, id).Scan(&source, &destination); err != nil {
		t.Fatal(err)
	}
	if source != "health" || destination != "nutrition" {
		t.Fatal("transfer history lost")
	}
	records, err := db.RetrieveMemory(MemoryQuery{Reader: "mika", Butler: "nutrition"})
	if err != nil || len(records) != 0 {
		t.Fatal("transfer widened private audience", records, err)
	}
}

func TestAgentMemoryRestrictionOnlyNarrows(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	_, err := db.RecordMemory(scopedFact(t, MemoryScopeHousehold, "nutrition", HouseholdOwner))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAgentMemoryRestriction("health", MemoryScopeHousehold, true, "sam"); err == nil {
		t.Fatal("Member changed Agent policy")
	}
	if err := db.SetAgentMemoryRestriction("health", MemoryScopeHousehold, true, "mika"); err != nil {
		t.Fatal(err)
	}
	got, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "health"})
	if err != nil || len(got) != 0 {
		t.Fatalf("Agent restriction ignored: %v, %v", got, err)
	}
	if err := db.SetAgentMemoryRestriction("health", MemoryScopeHousehold, false, "mika"); err != nil {
		t.Fatal(err)
	}
	got, err = db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "health"})
	if err != nil || len(got) != 1 {
		t.Fatalf("removing restriction lost default: %v, %v", got, err)
	}
	_, err = db.RecordMemory(scopedFact(t, MemoryScopePersonal, "nutrition", "mika"))
	if err != nil {
		t.Fatal(err)
	}
	got, err = db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "health"})
	if err != nil || len(got) != 1 {
		t.Fatalf("Agent eligibility widened Person visibility: %v, %v", got, err)
	}
}

func TestInvalidScopeAndCorrectionCannotPromote(t *testing.T) {
	db := butlerFixture(t)
	m := scopedFact(t, MemoryScopePersonal, "health", "sam")
	id, err := db.RecordMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE butler_memory SET scope='household' WHERE seq=?`, id); err == nil {
		t.Fatal("direct update promoted Personal memory")
	}
	m.Scope = MemoryScopeHousehold
	m.Owner = HouseholdOwner
	m.Content = "corrected scope fact"
	corrected, err := db.CorrectMemory(id, m, "sam")
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetMemory(corrected)
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != MemoryScopePersonal || got.Owner != "sam" {
		t.Fatalf("correction promoted memory: %+v", got)
	}
	m.Scope = "session"
	if _, err := db.RecordMemory(m); err == nil {
		t.Fatal("session transcript accepted as durable memory")
	}
}

// Audience metadata alone cannot grant access to a narrower memory scope.
func TestPersonalScopeCannotLeakThroughExportOrDigest(t *testing.T) {
	db := butlerFixture(t)
	m := scopedFact(t, MemoryScopePersonal, "health", "sam")
	m.Audience = ButlerAudienceShared
	id, err := db.RecordMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := db.ExportMemoryFor("mika")
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range exported {
		if got.Seq == id {
			t.Error("export disclosed another Person's Personal memory")
		}
	}
	local := scopedFact(t, MemoryScopeAgentLocal, "health", HouseholdOwner)
	local.Content = "agent local digest secret"
	if _, err := db.RecordMemory(local); err != nil {
		t.Fatal(err)
	}
	entries, err := db.PendingMemoryDigest(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("household digest disclosed scoped memory: %+v", entries)
	}
}

func TestCorrectionReadsScopeAfterConcurrentTransfer(t *testing.T) {
	db := butlerFixture(t)
	id, err := db.RecordMemory(scopedFact(t, MemoryScopeAgentLocal, "health", "sam"))
	if err != nil {
		t.Fatal(err)
	}
	// Hold the transfer's write lock while correction begins. WAL readers can
	// still see the old Agent, so reading before BEGIN would copy stale scope.
	transfer, err := db.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer transfer.Rollback()
	if _, err := transfer.Exec(`UPDATE butler_memory SET butler='nutrition' WHERE seq=?`, id); err != nil {
		t.Fatal(err)
	}
	type result struct {
		seq int64
		err error
	}
	done := make(chan result, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		seq, err := db.CorrectMemory(id, MemoryRecord{Content: "corrected jasmine preference"}, "sam")
		done <- result{seq, err}
	}()
	<-started
	select {
	case r := <-done:
		t.Fatalf("correction did not wait for transfer: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	if err := transfer.Commit(); err != nil {
		t.Fatal(err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("correction did not finish after transfer")
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	got, err := db.GetMemory(r.seq)
	if err != nil || got.Butler != "nutrition" {
		t.Fatalf("correction recreated memory under the previous Agent: %+v, %v", got, err)
	}
}

func TestPromotionDedupPreservesScopeFallbacks(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	m := scopedFact(t, MemoryScopePersonal, "health", "sam")
	m.Content = "jasmine preference"
	source, err := db.RecordMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := db.PromotePersonalMemory(source, "sam")
	if err != nil {
		t.Fatal(err)
	}
	check := func(wantRecord, wantHit int64) {
		t.Helper()
		records, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "health", Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if wantRecord == 0 {
			if len(records) != 0 {
				t.Errorf("restricted retrieval: %+v", records)
			}
		} else if len(records) != 1 || records[0].Seq != wantRecord {
			t.Errorf("retrieval want %d: %+v", wantRecord, records)
		}
		hits, err := db.SearchIndex(IndexQuery{Reader: "sam", Butler: "health", Text: "jasmine", Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if wantHit == 0 {
			if len(hits) != 0 {
				t.Errorf("restricted search: %+v", hits)
			}
		} else if len(hits) != 1 || hits[0].MemorySeq != wantHit {
			t.Errorf("search want %d: %+v", wantHit, hits)
		}
	}
	// Promotion is indexed even if the original note was not.
	check(source, promoted)
	if _, err := db.IndexDocumentInto(IndexDocument{SourceKind: "memory", SourceID: "source-index", MemorySeq: source, Owner: "sam", Audience: ButlerAudiencePrivate, PermissionLabel: "health", Butler: "health", Content: m.Content}); err != nil {
		t.Fatal(err)
	}
	check(source, source)
	exported, err := db.ExportMemoryFor("sam")
	if err != nil || len(exported) != 2 {
		t.Fatalf("export lost provenance: %+v %v", exported, err)
	}
	restrict := func(scope string, denied bool) {
		t.Helper()
		if err := db.SetAgentMemoryRestriction("health", scope, denied, "mika"); err != nil {
			t.Fatal(err)
		}
	}
	restrict(MemoryScopePersonal, true)
	check(promoted, promoted)
	restrict(MemoryScopeHousehold, true)
	check(0, 0)
	restrict(MemoryScopePersonal, false)
	check(source, source)
	restrict(MemoryScopeHousehold, false)
	if err := db.ForgetMemory(promoted, "sam"); err != nil {
		t.Fatal(err)
	}
	check(source, source)
}

// The vector path de-duplicates a promoted copy in Go: the Personal source
// wins only when it is itself a viable semantic candidate; otherwise the
// promoted copy is the answer.
func TestSemanticPromotionDedupFallsBackToPromotedCopy(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	m := scopedFact(t, MemoryScopePersonal, "health", "sam")
	m.Content = "jasmine preference"
	source, err := db.RecordMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := db.PromotePersonalMemory(source, "sam")
	if err != nil {
		t.Fatal(err)
	}
	vec := []float32{1, 0, 0}
	index := func(id string, mem int64, owner string, aud ButlerTaskAudience, emb []float32) {
		t.Helper()
		if _, err := db.IndexDocumentInto(IndexDocument{SourceKind: "memory", SourceID: id, MemorySeq: mem,
			Owner: owner, Audience: aud, PermissionLabel: "health", Butler: "health",
			Content: m.Content, Embedding: emb, ObservedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	// Source is indexed with an incompatible embedding; the promoted copy matches.
	index("src", source, "sam", ButlerAudiencePrivate, []float32{0, 1, 0})
	index("promo", promoted, "household", ButlerAudienceShared, vec)
	hits, err := db.SearchIndex(IndexQuery{Reader: "sam", Butler: "health", Text: "tea", Embedding: vec, Limit: 5})
	if err != nil || len(hits) != 1 || hits[0].MemorySeq != promoted {
		t.Fatalf("hits=%+v err=%v, want only the promoted copy %d", hits, err, promoted)
	}
	// Once the source also matches, it replaces the copy.
	index("src", source, "sam", ButlerAudiencePrivate, vec)
	hits, err = db.SearchIndex(IndexQuery{Reader: "sam", Butler: "health", Text: "tea", Embedding: vec, Limit: 5})
	if err != nil || len(hits) != 1 || hits[0].MemorySeq != source {
		t.Fatalf("hits=%+v err=%v, want only the source %d", hits, err, source)
	}
}
