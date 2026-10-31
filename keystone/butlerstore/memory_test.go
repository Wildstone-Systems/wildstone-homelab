package butlerstore

// The write half of memory, tested against a real store (ADR-W030
// A3/A3.1, ADR-W031 A4).
//
// The butler package's tests prove what a model cannot type. These
// prove what happens to the record afterwards: who ends up owning it,
// who can read it back, and what a room does and does not change.

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestPersonalRecallFollowsPersonAcrossAgents(t *testing.T) {
	db := memoryDB(t)
	remember(t, testMemorySeams(db, "alfred", "tom", nil), butler.MemoryNote{Kind: runtimedb.MemoryPreference, Content: "Prefers jasmine tea"})
	hits, err := testMemorySeams(db, "nutrition", "tom", nil).Recall(context.Background(), "jasmine", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("cross-Agent Personal recall: %v, %v", hits, err)
	}
}

func TestAgentLocalDerivedFromPersonAIsExcludedForPersonB(t *testing.T) {
	db := memoryDB(t)
	remember(t, testMemorySeams(db, "alfred", "tom", nil), butler.MemoryNote{Kind: runtimedb.MemoryPreference, Content: "Private jasmine preference"})
	records, err := db.RetrieveMemory(runtimedb.MemoryQuery{Reader: "tom", Butler: "alfred"})
	if err != nil || len(records) != 1 {
		t.Fatalf("source: %v, %v", records, err)
	}
	derived := records[0]
	derived.Scope = runtimedb.MemoryScopeAgentLocal
	derived.Content = "Derived jasmine preference"
	seq, err := db.DeriveMemory(derived, []int64{records[0].Seq})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately stale/wider index metadata must not authorize this derivative.
	_, err = db.IndexDocumentInto(runtimedb.IndexDocument{SourceKind: "memory", SourceID: "derived", MemorySeq: seq, Owner: "household", Audience: runtimedb.ButlerAudienceShared, PermissionLabel: "household", Butler: "alfred", Content: derived.Content})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		person string
		want   bool
	}{{"tom", true}, {"sam", false}} {
		seams := testMemorySeams(db, "alfred", tc.person, nil)
		hits, err := seams.Recall(context.Background(), "jasmine", 10)
		if err != nil {
			t.Fatal(err)
		}
		if (len(hits) > 0) != tc.want {
			t.Fatalf("retrieval for %s: %+v", tc.person, hits)
		}
		preamble, err := seams.Preamble(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		assembled := butler.RenderPreamble(tc.person, preamble)
		if strings.Contains(assembled, "jasmine") != tc.want {
			t.Fatalf("assembled context for %s: %q", tc.person, assembled)
		}
	}
}

func memoryDB(t *testing.T) *runtimedb.DB {
	t.Helper()
	db, err := runtimedb.Open(migrated.Path(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	record(t, db, "mika", "sam", "tom")
	return db
}

func remember(t *testing.T, seams butler.MemorySeams, n butler.MemoryNote) {
	t.Helper()
	if n.SourceKind == "" {
		n.SourceKind, n.SourceID = butler.MemorySourceConversation, "bt-1"
	}
	if err := seams.Remember(context.Background(), n); err != nil {
		t.Fatalf("Remember: %v", err)
	}
}

// A note is the speaker's and follows them across permitted Agents.
//
// Four decisions land on the record and none of them came from the
// note: the owner is the person who spoke, the audience is private, the
// default scope is Personal, and the confidence
// is stated because somebody stated it.
func TestARememberedNoteBelongsToTheSpeakerAcrossAgents(t *testing.T) {
	db := memoryDB(t)
	seams := testMemorySeams(db, "alfred", "tom", nil)
	remember(t, seams, butler.MemoryNote{
		Kind: runtimedb.MemoryPreference, Subject: "tom", Content: "Prefers tea to coffee.",
	})

	records, err := db.RetrieveMemory(runtimedb.MemoryQuery{Reader: "tom", Butler: "alfred"})
	if err != nil {
		t.Fatalf("RetrieveMemory: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	got := records[0]
	switch {
	case got.Owner != "tom":
		t.Fatalf("owner = %q", got.Owner)
	case got.Audience != runtimedb.ButlerAudiencePrivate:
		t.Fatalf("audience = %q — nothing a conversation says makes a record the household's", got.Audience)
	case got.Butler != "alfred":
		t.Fatalf("creating Agent provenance = %q", got.Butler)
	case got.PermissionLabel != "alfred":
		t.Fatalf("permission label = %q", got.PermissionLabel)
	case got.Confidence != runtimedb.ConfidenceStated:
		t.Fatalf("confidence = %q — a person said this", got.Confidence)
	case got.Author != "tom":
		t.Fatalf("author = %q — the person said it; the Butler only wrote it down", got.Author)
	}

	// Another member does not see it, and neither does another Butler.
	// The filter is the store's, in the SQL that finds candidates; this
	// is the end-to-end proof that the write reached it with the right
	// values on it.
	other, err := db.RetrieveMemory(runtimedb.MemoryQuery{Reader: "sam", Butler: "alfred"})
	if err != nil {
		t.Fatalf("RetrieveMemory: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("sam read tom's private note: %+v", other)
	}
	elsewhere, err := db.RetrieveMemory(runtimedb.MemoryQuery{Reader: "tom", Butler: "nutrition"})
	if err != nil {
		t.Fatalf("RetrieveMemory: %v", err)
	}
	if len(elsewhere) != 1 {
		t.Fatalf("Personal memory did not follow its Person across Agents: %+v", elsewhere)
	}
}

// A3.1: a group statement belongs to its speaker, with the room
// recorded separately.
//
// "I dislike cilantro" said in the kitchen is Tom's preference. The
// room is the SOURCE — which is what makes it answerable later that
// this was said in front of people — and it is not the owner, not the
// audience, and not a fact about anybody else in it.
func TestAGroupStatementIsTheSpeakersWithTheRoomAsItsSource(t *testing.T) {
	db := memoryDB(t)
	remember(t, testMemorySeams(db, "alfred", "tom", nil), butler.MemoryNote{
		Kind: runtimedb.MemoryPreference, Subject: "tom", Content: "Dislikes cilantro.",
		SourceKind: butler.MemorySourceRoom, SourceID: "bt-9",
	})
	records, _ := db.RetrieveMemory(runtimedb.MemoryQuery{Reader: "tom", Butler: "alfred"})
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	if records[0].Owner != "tom" || records[0].Audience != runtimedb.ButlerAudiencePrivate {
		t.Fatalf("a statement in a room became the household's: %+v", records[0])
	}
	if records[0].SourceKind != butler.MemorySourceRoom {
		t.Fatalf("the room was not recorded: %q", records[0].SourceKind)
	}
	// And it stays out of everyone else's Butler, which is the failure
	// the rule is actually about: a room is where the temptation to
	// call something "the household's" comes from.
	for _, member := range []string{"sam", "kit"} {
		got, _ := db.RetrieveMemory(runtimedb.MemoryQuery{Reader: member, Butler: "alfred"})
		if len(got) != 0 {
			t.Fatalf("%s read what tom said in the room: %+v", member, got)
		}
	}
}

// A3.1: missing permission metadata fails closed — and here it fails
// closed by being ABSENT.
//
// A session that cannot name the reader or the Butler gets no seams at
// all, so the gateway registers neither tool. "Memory is off here" and
// "I could not do that" are different answers, and a person is owed the
// first one rather than a tool that refuses every call.
func TestASessionThatCannotNameItsScopeGetsNoMemoryAtAll(t *testing.T) {
	db := memoryDB(t)
	for _, c := range []struct{ butlerName, member string }{
		{"", "tom"}, {"alfred", ""}, {"", ""},
	} {
		seams := testMemorySeams(db, c.butlerName, c.member, nil)
		if seams.Remember != nil || seams.Recall != nil || seams.Preamble != nil {
			t.Fatalf("butler=%q member=%q was given memory anyway", c.butlerName, c.member)
		}
	}
	if testMemorySeams(nil, "alfred", "tom", nil).Remember != nil {
		t.Fatal("memory was wired with no store behind it")
	}
	// And provenance is refused at the seam rather than defaulted, so a
	// caller that forgets the conversation writes nothing.
	seams := testMemorySeams(db, "alfred", "tom", nil)
	if err := seams.Remember(context.Background(), butler.MemoryNote{
		Kind: runtimedb.MemoryFact, Content: "no source",
	}); err == nil {
		t.Fatal("a note with no source was stored")
	}
	records, _ := db.RetrieveMemory(runtimedb.MemoryQuery{Reader: "tom", Butler: runtimedb.AllButlers})
	if len(records) != 0 {
		t.Fatalf("records = %+v", records)
	}
}

// A note is findable the moment it is kept, and it comes back knowing
// what it is.
//
// Indexing is a queued job for documents, where the slow part is the
// embedding. A note is one row and no producer resolves the embedder yet: queueing it
// would mean telling somebody it is remembered while recall cannot find
// it. And the hit carries the record's subject and kind, because a
// free-floating sentence answers no question about "my" anything.
func TestARememberedNoteIsFoundByRecallAndKnowsWhatItIs(t *testing.T) {
	db := memoryDB(t)
	seams := testMemorySeams(db, "alfred", "tom", nil)
	remember(t, seams, butler.MemoryNote{
		Kind: runtimedb.MemoryFact, Subject: "tom", Content: "The boiler was serviced in March.",
	})

	hits, err := seams.Recall(context.Background(), "boiler", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].Subject != "tom" || hits[0].Kind != runtimedb.MemoryFact {
		t.Fatalf("a hit came back not knowing whose it is or what it is: %+v", hits[0])
	}
	// Somebody else's Butler searching the same words finds nothing.
	sams, err := testMemorySeams(db, "alfred", "sam", nil).Recall(context.Background(), "boiler", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(sams) != 0 {
		t.Fatalf("sam searched and found tom's note: %+v", sams)
	}
}

// Forgetting sticks, and it sticks in both places.
//
// The record stops being retrievable AND its searchable copy goes with
// it, because the chunk points at the record — which is the whole
// reason the write links them. Then saying it again does not put it
// back: the store suppresses the source, and the seam reports that as
// something a session can say out loud.
func TestForgettingARememberedNoteRemovesItFromBothAndItDoesNotComeBack(t *testing.T) {
	db := memoryDB(t)
	seams := testMemorySeams(db, "alfred", "tom", nil)
	remember(t, seams, butler.MemoryNote{
		Kind: runtimedb.MemoryFact, Subject: "tom", Content: "The spare key is under the third pot.",
	})
	records, _ := db.RetrieveMemory(runtimedb.MemoryQuery{Reader: "tom", Butler: "alfred"})
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	if err := db.ForgetMemory(records[0].Seq, "tom"); err != nil {
		t.Fatalf("ForgetMemory: %v", err)
	}

	if left, _ := db.RetrieveMemory(runtimedb.MemoryQuery{Reader: "tom", Butler: "alfred"}); len(left) != 0 {
		t.Fatalf("a forgotten note is still retrievable: %+v", left)
	}
	hits, err := seams.Recall(context.Background(), "spare key", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("a forgotten note is still a search hit: %+v", hits)
	}

	err = seams.Remember(context.Background(), butler.MemoryNote{
		Kind: runtimedb.MemoryFact, Subject: "tom", Content: "The spare key is under the third pot.",
		SourceKind: butler.MemorySourceConversation, SourceID: "bt-1",
	})
	if err == nil {
		t.Fatal("a forgotten note was learned again from the same conversation")
	}
	if err != butler.ErrForgotten {
		t.Fatalf("err = %v — the session cannot say the true thing about a generic failure", err)
	}
}

// A preference reaches the next session unasked; a fact does not.
//
// The two are different questions (A3): a preference only shapes an
// answer if it is present before the model speaks, and a fact is an
// answer to something nobody has asked yet, so it waits to be searched.
func TestAPreferenceIsInjectedAndAFactIsSearched(t *testing.T) {
	db := memoryDB(t)
	seams := testMemorySeams(db, "alfred", "tom", nil)
	remember(t, seams, butler.MemoryNote{
		Kind: runtimedb.MemoryPreference, Subject: "tom", Content: "Prefers tea to coffee.",
	})
	remember(t, seams, butler.MemoryNote{
		Kind: runtimedb.MemoryFact, Subject: "tom", Content: "The boiler was serviced in March.",
	})

	hits, err := seams.Preamble(context.Background())
	if err != nil {
		t.Fatalf("Preamble: %v", err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Content, "tea") {
		t.Fatalf("preamble = %+v", hits)
	}
	// And it renders with the subject on it, which is what connects a
	// record about "tom" to a question from "you".
	if block := butler.RenderPreamble("tom", hits); !strings.Contains(block, "tom: Prefers tea to coffee.") {
		t.Fatalf("preamble block = %q", block)
	}
}

func TestAutonomousAgentLocalNoteStaysWithAgentAndPerson(t *testing.T) {
	db := memoryDB(t)
	remember(t, testMemorySeams(db, "alfred", "tom", nil), butler.MemoryNote{Scope: runtimedb.MemoryScopeAgentLocal, Kind: runtimedb.MemoryPreference, Content: "Local jasmine preference"})
	for _, tc := range []struct {
		agent, person string
		want          int
	}{{"alfred", "tom", 1}, {"nutrition", "tom", 0}, {"alfred", "sam", 0}} {
		seams := testMemorySeams(db, tc.agent, tc.person, nil)
		hits, err := seams.Recall(context.Background(), "jasmine", 10)
		if err != nil || len(hits) != tc.want {
			t.Fatalf("%s/%s recall: %+v, %v", tc.agent, tc.person, hits, err)
		}
		preamble, err := seams.Preamble(context.Background())
		if err != nil || len(preamble) != tc.want {
			t.Fatalf("%s/%s preamble: %+v, %v", tc.agent, tc.person, preamble, err)
		}
	}
}

func TestPromotedMemoryAppearsOnceInRecallAndContext(t *testing.T) {
	db := memoryDB(t)
	if err := db.AdmitButlerMember("tom", "root-admin"); err != nil {
		t.Fatal(err)
	}
	remember(t, testMemorySeams(db, "alfred", "tom", nil), butler.MemoryNote{Kind: runtimedb.MemoryPreference, Content: "Prefers jasmine tea"})
	records, err := db.RetrieveMemory(runtimedb.MemoryQuery{Reader: "tom", Butler: "alfred"})
	if err != nil || len(records) != 1 {
		t.Fatalf("source: %+v %v", records, err)
	}
	if _, err := db.PromotePersonalMemory(records[0].Seq, "tom"); err != nil {
		t.Fatal(err)
	}
	for _, person := range []string{"tom", "sam"} {
		seams := testMemorySeams(db, "nutrition", person, nil)
		hits, err := seams.Recall(context.Background(), "jasmine", 10)
		if err != nil || len(hits) != 1 {
			t.Errorf("%s recall duplicates: %+v %v", person, hits, err)
		}
		preamble, err := seams.Preamble(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(butler.RenderPreamble(person, preamble), "Prefers jasmine tea"); got != 1 {
			t.Errorf("%s context contains note %d times", person, got)
		}
	}
}

func testMemorySeams(db *runtimedb.DB, agent, person string, log *slog.Logger) butler.MemorySeams {
	return memorySeams(db, agent, person, nil, log)
}
