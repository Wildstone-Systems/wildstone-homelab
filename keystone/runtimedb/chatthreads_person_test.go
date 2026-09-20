package runtimedb

import "testing"

// TestTwoPeopleInOneConversationKeepSeparateThreads pins the invariant
// Codex reported on #109, at the level where it now lives.
//
// WHY IT MOVED. It used to be asserted through the chat API, by sending
// four group messages from two members and checking each got their own
// session. Slice 11's disclosure half refuses group destinations
// (design §8.3), so that route no longer reaches a session at all —
// and an invariant whose only test stops exercising it is an invariant
// on its way to being deleted by somebody who finds the key "redundant".
//
// The rule: a thread pointer is keyed on (connection, conversation,
// person), not on the conversation. With one pointer per conversation,
// two people talking in the same place would rewrite each other's, and
// BOTH would lose their thread on every alternate message.
//
// It is not only about rooms. Any transport where two admitted people
// can share a conversation identifier has this shape, and the schema
// is what makes it safe rather than the caller remembering.
func TestTwoPeopleInOneConversationKeepSeparateThreads(t *testing.T) {
	db := householdWith(t, "alice", "bob")
	at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)

	if err := db.SetChatThreadSession(at, "shared-1", "alice", "sess-alice"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetChatThreadSession(at, "shared-1", "bob", "sess-bob"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ who, want string }{
		{"alice", "sess-alice"},
		{"bob", "sess-bob"},
	} {
		got, ok, err := db.ChatThreadSession(at, "shared-1", tc.who)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || got != tc.want {
			t.Fatalf("%s's thread in the shared conversation = %q (found=%v), want %q -- "+
				"the other speaker took it", tc.who, got, ok, tc.want)
		}
	}

	// And one person's second message moves only their own pointer.
	if err := db.SetChatThreadSession(at, "shared-1", "alice", "sess-alice-2"); err != nil {
		t.Fatal(err)
	}
	if got, _, err := db.ChatThreadSession(at, "shared-1", "bob"); err != nil || got != "sess-bob" {
		t.Fatalf("bob's thread = %q after alice spoke again, want it untouched (err=%v)", got, err)
	}

	// Clearing is per person too, for the same reason.
	if err := db.ClearChatThread(at, "shared-1", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.ChatThreadSession(at, "shared-1", "alice"); err != nil || ok {
		t.Fatalf("alice's thread survived being cleared (found=%v, err=%v)", ok, err)
	}
	if got, ok, err := db.ChatThreadSession(at, "shared-1", "bob"); err != nil || !ok || got != "sess-bob" {
		t.Fatalf("clearing alice's thread took bob's: %q (found=%v, err=%v)", got, ok, err)
	}
}
