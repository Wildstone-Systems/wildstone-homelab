package runtimedb

// P6's evidence (ADR-W030): the same admitted adult can explicitly hand
// a recent conversation between Household and Operator while destination
// authority is recomputed; unlinked, disabled and group identities fail
// closed; no privileged context crosses the handoff.

import (
	"errors"
	"testing"
)

func admitted(t *testing.T, db *DB, member, profile string) {
	t.Helper()
	if err := db.RecordButlerMember(member, profile, ""); err != nil {
		t.Fatalf("RecordButlerMember(%s): %v", member, err)
	}
	if err := db.AdmitButlerMember(member, "root-admin"); err != nil {
		t.Fatalf("AdmitButlerMember(%s): %v", member, err)
	}
}

func TestAdmissionIsRuntimeStateNotAName(t *testing.T) {
	db := butlerTestDB(t)

	// An identity nobody linked resolves to nobody. Equal names are not
	// proof of identity (ADR-W030 A2.1).
	if _, err := db.ResolveSurfaceIdentity(LinkAPIPrincipal, "mika"); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("an unlinked principal resolved: %v", err)
	}
	admitted(t, db, "mika", ProfileOperator)
	// Recorded and admitted, but still not linked to anything.
	if _, err := db.ResolveSurfaceIdentity(LinkAPIPrincipal, "mika"); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("an admitted member with no link resolved: %v", err)
	}

	if err := db.LinkSurfaceIdentity(LinkAPIPrincipal, "mika", "mika", "root-admin"); err != nil {
		t.Fatalf("LinkSurfaceIdentity: %v", err)
	}
	m, err := db.ResolveSurfaceIdentity(LinkAPIPrincipal, "mika")
	if err != nil {
		t.Fatalf("ResolveSurfaceIdentity: %v", err)
	}
	if m.Member != "mika" || m.Profile != ProfileOperator {
		t.Fatalf("resolved = %+v", m)
	}
	// A link needs an approving administrator, and Butler cannot be one
	// by simply being passed as the string.
	if err := db.LinkSurfaceIdentity(LinkChatBinding, "42", "mika", ""); err == nil {
		t.Fatal("a link was written with no approving administrator")
	}
	// One live link per external identity: a chat account belongs to one
	// person at a time.
	admitted(t, db, "sam", ProfileHousehold)
	if err := db.LinkSurfaceIdentity(LinkAPIPrincipal, "mika", "sam", "root-admin"); err == nil {
		t.Fatal("one external identity was linked to two members")
	}
}

func TestRevocationLandsAtTheNextCheck(t *testing.T) {
	db := butlerTestDB(t)
	admitted(t, db, "sam", ProfileHousehold)
	if err := db.LinkSurfaceIdentity(LinkChatBinding, "7", "sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ResolveChatBinding(7); err != nil {
		t.Fatalf("a live link did not resolve: %v", err)
	}

	t.Run("disable", func(t *testing.T) {
		if err := db.DisableButlerMember("sam", "root-admin"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ResolveChatBinding(7); !errors.Is(err, ErrNotAdmitted) {
			t.Fatalf("a disabled member still resolved: %v", err)
		}
		// And the links went with them, so a Console listing does not
		// show a bound account for someone who has none.
		links, err := db.ListSurfaceLinks("sam")
		if err != nil {
			t.Fatal(err)
		}
		if len(links) != 1 || links[0].RevokedAt.IsZero() {
			t.Fatalf("links after disable = %+v", links)
		}
	})

	t.Run("re-admission is a new decision", func(t *testing.T) {
		before, _ := db.GetButlerMember("sam")
		if err := db.AdmitButlerMember("sam", "root-admin"); err != nil {
			t.Fatal(err)
		}
		after, _ := db.GetButlerMember("sam")
		if after.Generation <= before.Generation {
			t.Fatalf("re-admission did not bump the generation: %d → %d", before.Generation, after.Generation)
		}
		// The old link does not come back with them.
		if _, err := db.ResolveChatBinding(7); !errors.Is(err, ErrNotAdmitted) {
			t.Fatalf("a revoked link revived on re-admission: %v", err)
		}
	})

	t.Run("offboarding denies immediately", func(t *testing.T) {
		if err := db.LinkSurfaceIdentity(LinkChatBinding, "8", "sam", "root-admin"); err != nil {
			t.Fatal(err)
		}
		if err := db.OffboardButlerMember("sam", "root-admin"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ResolveChatBinding(8); !errors.Is(err, ErrNotAdmitted) {
			t.Fatalf("an offboarded member still resolved: %v", err)
		}
		m, _ := db.GetButlerMember("sam")
		if m.Active() || m.OffboardedAt.IsZero() || m.OffboardedBy != "root-admin" {
			t.Fatalf("offboarded member = %+v", m)
		}
	})

	// The generation cannot be rewound to revive withdrawn access.
	if _, err := db.sql.Exec(`UPDATE butler_members SET generation = 1 WHERE member = 'sam'`); err == nil {
		t.Fatal("the member generation was rewound")
	}
}

// A child record exists so a guardian relationship can be recorded, and
// cannot be admitted — the schema refuses it, so no console form or
// future migration can produce one before that surface has its evidence.
func TestChildRecordsExistButCannotBeAdmitted(t *testing.T) {
	db := butlerTestDB(t)
	if err := db.RecordButlerMember("kid", ProfileChild, ""); err == nil {
		t.Fatal("a child record was written with no guardian")
	}
	if err := db.RecordButlerMember("kid", ProfileChild, "mika"); err != nil {
		t.Fatalf("RecordButlerMember(child): %v", err)
	}
	if err := db.AdmitButlerMember("kid", "root-admin"); err == nil {
		t.Fatal("a child was admitted to the Butler")
	}
	if _, err := db.sql.Exec(
		`INSERT INTO butler_members (member, profile, admitted, generation, created_at, guardian)
		 VALUES ('kid2', 'child', 1, 1, 't', 'mika')`); err == nil {
		t.Fatal("an admitted child was inserted directly")
	}
	m, err := db.GetButlerMember("kid")
	if err != nil {
		t.Fatal(err)
	}
	if m.Active() || m.Guardian != "mika" {
		t.Fatalf("child record = %+v", m)
	}
}

// The headline: an operator hands a live conversation from the household
// bot to the operator bot, keeps the thread, and carries no privilege.
func TestHandoffKeepsTheThreadAndCarriesNoPrivilege(t *testing.T) {
	db := butlerTestDB(t)
	admitted(t, db, "mika", ProfileOperator)

	conv, err := db.StartButlerConversation("mika", "the photos thing")
	if err != nil {
		t.Fatalf("StartButlerConversation: %v", err)
	}

	// A household exchange, then an operator one, both in this
	// conversation.
	family, err := db.InsertButlerSession(ButlerSession{
		Principal: "butler", ActingFor: "mika", Provider: "ollama",
		Mode: ButlerModeFull, Surface: ButlerSurfaceFamily, FinalText: "photos has been slow today",
		Turns: []ButlerTurn{
			{Role: ButlerRoleUser, Text: "is photos ok?"},
			{Role: ButlerRoleAssistant, Text: "photos has been slow today",
				TranscriptJSON: `{"calls":[{"name":"get_household_events"}]}`},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := db.InsertButlerSession(ButlerSession{
		Principal: "butler", ActingFor: "mika", Provider: "ollama",
		Mode: ButlerModeFull, Surface: ButlerSurfaceAdmin, FinalText: "immich-server is unhealthy on n2",
		Turns: []ButlerTurn{
			{Role: ButlerRoleUser, Text: "why?"},
			{Role: ButlerRoleAssistant, Text: "immich-server is unhealthy on n2",
				TranscriptJSON: `{"calls":[{"name":"get_health","result":"node n2 secret detail"}]}`,
				PlanIDs:        []string{"plan-9"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{family, admin} {
		if err := db.AttachSessionToConversation(conv, s); err != nil {
			t.Fatalf("AttachSessionToConversation: %v", err)
		}
	}
	// Attaching twice converges rather than failing.
	if err := db.AttachSessionToConversation(conv, family); err != nil {
		t.Fatalf("re-attaching: %v", err)
	}

	// Family → admin: the operator side gets the operator thread and not
	// the household one.
	ctx, err := db.HandoffConversation(conv, ButlerSurfaceFamily, ButlerSurfaceAdmin, admin)
	if err != nil {
		t.Fatalf("HandoffConversation: %v", err)
	}
	if len(ctx.Turns) != 2 || ctx.Withheld != 2 {
		t.Fatalf("carried %d turns, withheld %d: %+v", len(ctx.Turns), ctx.Withheld, ctx.Turns)
	}
	for _, turn := range ctx.Turns {
		if turn.TranscriptJSON != "" || len(turn.PlanIDs) != 0 {
			t.Fatalf("tool traffic crossed the handoff: %+v", turn)
		}
	}

	// The other direction withholds the operator turns, which is what
	// keeps node names and plan ids off the family surface.
	back, err := db.HandoffConversation(conv, ButlerSurfaceAdmin, ButlerSurfaceFamily, admin)
	if err != nil {
		t.Fatalf("handoff back: %v", err)
	}
	for _, turn := range back.Turns {
		if turn.Text == "immich-server is unhealthy on n2" {
			t.Fatal("an operator answer crossed onto the family surface")
		}
	}
	if back.Withheld != 2 {
		t.Fatalf("withheld = %d, want the two operator turns", back.Withheld)
	}

	// Both moves are recorded, with the accounting that makes the claim
	// checkable.
	handoffs, err := db.ListButlerHandoffs(conv)
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffs) != 2 || handoffs[0].Carried != 2 || handoffs[0].Withheld != 2 {
		t.Fatalf("handoffs = %+v", handoffs)
	}
	// And they cannot be rewritten afterwards.
	if _, err := db.sql.Exec(`UPDATE butler_handoffs SET carried = 0`); err == nil {
		t.Fatal("a handoff record was edited")
	}
}

// Household admission does not grant operator access, and a handoff is
// not a way around that.
func TestHouseholdMemberCannotHandOffToTheOperatorBot(t *testing.T) {
	db := butlerTestDB(t)
	admitted(t, db, "sam", ProfileHousehold)
	conv, err := db.StartButlerConversation("sam", "dinner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.HandoffConversation(conv, ButlerSurfaceFamily, ButlerSurfaceAdmin, ""); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("a household member reached the operator bot by handoff: %v", err)
	}
	// And a member who has been disabled cannot hand off at all.
	admitted(t, db, "mika", ProfileOperator)
	mine, err := db.StartButlerConversation("mika", "nodes")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DisableButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.HandoffConversation(mine, ButlerSurfaceFamily, ButlerSurfaceAdmin, ""); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("a disabled member handed off: %v", err)
	}
	// A conversation cannot be started for someone who is not admitted.
	if _, err := db.StartButlerConversation("mika", "again"); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("a disabled member started a conversation: %v", err)
	}
	// And a handoff must actually move between the two bots.
	admitted(t, db, "ops", ProfileOperator)
	same, _ := db.StartButlerConversation("ops", "x")
	if _, err := db.HandoffConversation(same, ButlerSurfaceAdmin, ButlerSurfaceAdmin, ""); err == nil {
		t.Fatal("a handoff to the same surface was accepted")
	}
}

// The handoff carries a thread, not a history.
func TestHandoffIsBounded(t *testing.T) {
	db := butlerTestDB(t)
	admitted(t, db, "mika", ProfileOperator)
	conv, _ := db.StartButlerConversation("mika", "long one")

	var turns []ButlerTurn
	for i := 0; i < MaxHandoffTurns+6; i++ {
		turns = append(turns, ButlerTurn{Role: ButlerRoleUser, Text: "q"})
	}
	sess, err := db.InsertButlerSession(ButlerSession{
		Principal: "butler", ActingFor: "mika", Provider: "ollama",
		Mode: ButlerModeFull, Surface: ButlerSurfaceAdmin, FinalText: "a", Turns: turns,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AttachSessionToConversation(conv, sess); err != nil {
		t.Fatal(err)
	}
	ctx, err := db.HandoffConversation(conv, ButlerSurfaceFamily, ButlerSurfaceAdmin, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.Turns) != MaxHandoffTurns || ctx.Withheld != 6 {
		t.Fatalf("carried %d, withheld %d; want %d and 6", len(ctx.Turns), ctx.Withheld, MaxHandoffTurns)
	}
}
