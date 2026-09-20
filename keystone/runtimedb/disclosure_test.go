package runtimedb

import "testing"

// Slice 11 (#212), disclosure half. These cover the two branches that
// cannot be reached through the API and that a mutation sweep found
// nothing else asserting: a lookup that FAILS, and a classification
// row that is missing or unreadable.
//
// Both are the same rule as admission's, one object along. A state the
// runtime cannot establish must not be the state in which it permits
// the most — and for both of these, that is exactly the direction the
// obvious implementation fails in.

// TestALookupThatFailsAnswersRoom — IsChatRoom's question is always
// asked in service of "may the house send what it knows here". A
// lookup that errored has not established that this destination is a
// direct chat, so answering false would make a broken database the
// most permissive state there is.
func TestALookupThatFailsAnswersRoom(t *testing.T) {
	db := householdWith(t, "sam")
	at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)

	// The control: a conversation nobody reported as a room is not one,
	// or this test would pass against `return true`.
	if db.IsChatRoom(at.Seq, "direct-1") {
		t.Fatal("an unreported conversation was treated as a room")
	}
	if err := db.NoteChatRoom(at.Seq, "room-1"); err != nil {
		t.Fatal(err)
	}
	if !db.IsChatRoom(at.Seq, "room-1") {
		t.Fatal("a reported room was not recorded")
	}

	// Now the lookup cannot answer at all.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if !db.IsChatRoom(at.Seq, "direct-1") {
		t.Fatal("a failed lookup answered 'not a room', which is the permissive answer")
	}
}

// A room that goes quiet is still a room. Noting one twice moves the
// last-seen stamp and never removes the row, because forgetting would
// silently re-permit delivery into it.
func TestNotingARoomIsIdempotentAndNeverForgets(t *testing.T) {
	db := householdWith(t, "sam")
	at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)
	for i := 0; i < 3; i++ {
		if err := db.NoteChatRoom(at.Seq, "room-1"); err != nil {
			t.Fatalf("note %d: %v", i, err)
		}
	}
	if !db.IsChatRoom(at.Seq, "room-1") {
		t.Fatal("a room stopped being one")
	}
	// And it is scoped to its Connection: the same conversation id on
	// another bot is a different destination, and treating them as one
	// would let one Agent's room silence another Agent's direct chat.
	other := connect(t, db, "joan", "telegram", ButlerSurfaceFamily, "chat-telegram-joan")
	if db.IsChatRoom(other.Seq, "room-1") {
		t.Fatal("a room on one connection marked the same id on another")
	}
}

// TestAnUnreadableClassificationAcceptsNoFallback — §8.3: cross-
// Connection delivery "only when explicitly configured". A missing or
// unreadable classification is not a configuration.
//
// The branch exists because the obvious reading of `accepts_fallback`
// is the integer on its own, and a row that is absent or half-read is
// the one state where nobody has said anything at all. Reading the
// flag alone would make "nobody classified this Connection" the single
// state in which cross-Connection delivery is permitted, which is the
// exact inversion of the rule.
func TestAnUnreadableClassificationAcceptsNoFallback(t *testing.T) {
	// The control: a fully readable row means what it says, in both
	// positions, or everything below passes against `return false`.
	on := scanDisclosure("external", "transport", "unknown", "external_messaging", 1)
	if !on.AcceptsFallback {
		t.Fatal("a readable row marked to accept a fallback does not")
	}
	off := scanDisclosure("external", "transport", "unknown", "external_messaging", 0)
	if off.AcceptsFallback {
		t.Fatal("a readable row not marked to accept one does")
	}

	for _, tc := range []struct {
		name                                  string
		reach, encryption, retention, surface string
	}{
		{"no row at all", "", "", "", ""},
		{"an unreadable reach", "everywhere", "transport", "unknown", "external_messaging"},
		{"an unreadable surface", "external", "transport", "unknown", "a phone"},
		{"a reach that is only nearly right", " local ", "transport", "unknown", "external_messaging"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The flag is SET, which is what makes this a real test:
			// the row is asking to receive another Connection's
			// messages, and the rest of it cannot be read.
			d := scanDisclosure(tc.reach, tc.encryption, tc.retention, tc.surface, 1)
			if d.AcceptsFallback {
				t.Fatalf("a connection with %s was allowed to take another's messages", tc.name)
			}
		})
	}
}

// The two questions the rest of the runtime asks a classification, and
// both answer the strict way when it cannot be read.
func TestAnUnreadableClassificationReadsAsTheStrictAnswer(t *testing.T) {
	none := scanDisclosure("", "", "", "", 0)
	if !none.LeavesTheHouse() {
		t.Fatal("an unclassified connection was treated as staying in the house")
	}
	if !none.KeepsACopy() {
		t.Fatal("an unclassified connection was treated as keeping no copy")
	}
	// 'unknown' retention is an operator who has not checked, and must
	// read the same as unclassified — not as 'none'.
	unknown := scanDisclosure("local", "end_to_end", "unknown", "trusted_ui", 0)
	if !unknown.KeepsACopy() {
		t.Fatal("'unknown' retention was read as 'nobody keeps a copy'")
	}
	// And the one configuration that genuinely is the quiet answer
	// still reads that way, or the assertions above prove only that
	// something always says yes.
	quiet := scanDisclosure("local", "end_to_end", "none", "trusted_ui", 0)
	if quiet.LeavesTheHouse() || quiet.KeepsACopy() {
		t.Fatalf("a local, non-retaining connection reads as %+v", quiet)
	}
}
