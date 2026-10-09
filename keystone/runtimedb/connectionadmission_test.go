package runtimedb

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Connection admission (ADR-W033 A2, B9, B10; design §8.3).
//
// A Connection admits a Person. Before migration 0044 an admin-surface
// binding named a Keystone PRINCIPAL — `op`, `root-admin` — because
// principal/chatclaim.go required one, so a chat account reached an
// Owner Agent as a credential with nobody behind it. These tests pin
// what replaced that, and the negative cases are the point: the
// configuration check alone would leave every one of them open.

// bind admits somebody to a Connection the way a redeemed claim does.
func bind(t *testing.T, db *DB, at Connection, handle, account string) ChatBinding {
	t.Helper()
	hash := "hash-" + handle + "-" + at.Butler + "-" + account
	if err := db.InsertChatClaimCode(hash, ChatClaimCode{
		Connection: at.Seq,
		Identity:   handle,
		CreatedBy:  "root-admin",
		ExpiresAt:  time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("issuing a code for %q: %v", handle, err)
	}
	b, err := db.RedeemChatClaimCode(hash, at, account, handle, "conv-1")
	if err != nil {
		t.Fatalf("redeeming for %q: %v", handle, err)
	}
	return b
}

// householdWith is a database with the models a Butler needs and the named
// people on record and admitted.
func householdWith(t *testing.T, handles ...string) *DB {
	t.Helper()
	db := open(t)
	if err := db.UpsertModel(Model{
		Name: "kitchen-box", Kind: ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Readiness: ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	record(t, db, handles...)
	for _, h := range handles {
		if err := db.AdmitButlerMember(h, "root-admin"); err != nil {
			t.Fatalf("admitting %q: %v", h, err)
		}
	}
	return db
}

// An owner keeps their admin-surface Connection; the binding names them
// by identity, so the handle it reads back is whatever they carry now.
func TestARenamedPersonKeepsTheirConnection(t *testing.T) {
	db := householdWith(t, "mika")
	if err := db.SetButlerMemberRole("mika", RoleOwner, "root-admin"); err != nil {
		t.Fatal(err)
	}
	at := connect(t, db, "alfred", "telegram", ButlerSurfaceAdmin)
	bind(t, db, at, "mika", "12345")

	if err := db.RenameButlerMemberHandle("mika", "micah"); err != nil {
		t.Fatalf("renaming: %v", err)
	}
	b, err := db.LookupChatBinding(at, "12345")
	if err != nil {
		t.Fatalf("the connection was lost in a rename: %v", err)
	}
	if b.Identity != "micah" {
		t.Fatalf("binding identity = %q, want the handle they carry now", b.Identity)
	}
}

// The runtime half of B9, and the reason the configuration half is not
// enough on its own: "Owner-Agent Connections admit Owners only.
// Configuration and runtime both enforce this."
//
// Demotion is immediate (A6). A binding issued while somebody was an
// Owner must stop resolving the moment they are not — otherwise the
// Console rule that a demoted Owner loses the control plane holds
// everywhere except the transport they were already talking on.
func TestDemotingAnOwnerClosesTheirAdminConnection(t *testing.T) {
	db := householdWith(t, "mika", "sam")
	for _, h := range []string{"mika", "sam"} {
		if err := db.SetButlerMemberRole(h, RoleOwner, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	at := connect(t, db, "alfred", "telegram", ButlerSurfaceAdmin)
	bind(t, db, at, "sam", "12345")
	if _, err := db.LookupChatBinding(at, "12345"); err != nil {
		t.Fatalf("the fixture never had a working binding: %v", err)
	}

	// mika is the other active Owner, so the Last Owner invariant does
	// not stand in the way of demoting sam.
	if err := db.SetButlerMemberRole("sam", RoleMember, "root-admin"); err != nil {
		t.Fatalf("demoting: %v", err)
	}
	// ErrNotAdmitted, not ErrNotFound. Admission moved out of the
	// lookup's SQL and into Connection.Admits (slice 11, #212), and
	// with it the answer got sharper: the binding is still there, and
	// it is the Connection that now turns sam away. Asserting
	// ErrNotFound here would pin the vaguer answer and forbid the
	// improvement.
	_, err := db.LookupChatBinding(at, "12345")
	if !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("a demoted owner still reaches an owner agent over chat: err = %v", err)
	}
	// And the reason names the ceiling, because "not admitted" with no
	// cause sends an operator looking for a revoked binding that is
	// sitting right there.
	if !strings.Contains(err.Error(), "owners only") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
	// The binding itself is untouched: demotion withdraws admission,
	// it does not revoke what sam claimed. Re-promoting restores the
	// reach without a new claim code, which is the behaviour A6 wants
	// and is only visible because the two answers are distinct.
	if err := db.SetButlerMemberRole("sam", RoleOwner, "root-admin"); err != nil {
		t.Fatalf("re-promoting: %v", err)
	}
	if _, err := db.LookupChatBinding(at, "12345"); err != nil {
		t.Fatalf("a re-promoted owner should reach it again on the same binding: %v", err)
	}
}

// The same for the other two ways a Person stops being admitted, since
// a rule that only catches demotion is not the rule it claims to be.
func TestAnOffboardedPersonReachesNoConnection(t *testing.T) {
	db := householdWith(t, "mika", "sam")
	at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)
	bind(t, db, at, "sam", "12345")
	if _, err := db.LookupChatBinding(at, "12345"); err != nil {
		t.Fatalf("the fixture never had a working binding: %v", err)
	}
	if err := db.OffboardButlerMember("sam", "root-admin"); err != nil {
		t.Fatalf("offboarding: %v", err)
	}
	if _, err := db.LookupChatBinding(at, "12345"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an offboarded person still reaches their agent over chat: err = %v", err)
	}
}

// A family-surface Connection is unaffected by the Owner rule: it
// admits a Member, which is what the great majority of them are. The
// converse matters because the obvious over-correction — admit Owners
// everywhere — would lock the household out of its own agents.
func TestAFamilyConnectionAdmitsAMember(t *testing.T) {
	db := householdWith(t, "mika", "sam")
	at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)
	bind(t, db, at, "sam", "12345")
	b, err := db.LookupChatBinding(at, "12345")
	if err != nil {
		t.Fatalf("a member cannot hold a family connection: %v", err)
	}
	if b.Identity != "sam" {
		t.Fatalf("binding identity = %q, want sam", b.Identity)
	}
}

// A claim code for somebody who is not on record is refused where it is
// written, not where it is redeemed.
func TestAClaimCodeNamesAPersonOnRecord(t *testing.T) {
	db := householdWith(t, "mika")
	at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)
	err := db.InsertChatClaimCode("hash-ghost", ChatClaimCode{
		Connection: at.Seq, Identity: "ghost", CreatedBy: "root-admin",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if !errors.Is(err, ErrNoSuchPerson) {
		t.Fatalf("a code was issued for nobody: err = %v", err)
	}
}

// connect gives an Agent a bot on a transport and returns the
// Connection — the fixture every chat test needs since 0045, because
// admission is to one Agent's Connection rather than to a surface.
//
// The external id is the capability the bot's credential is minted for,
// which is what makes the address and the adapter the same Connection
// seen from two sides (ADR-W031 A7). It defaults to the name the
// catalog ships, chat-<transport>-<surface>, and a caller giving two
// Agents a bot each on one transport passes its own — which is the
// shape B7 describes and the shape the catalog cannot produce yet,
// because the package system has no instancing.
func connect(t *testing.T, db *DB, butler, transport string, surface ButlerSurface, capability ...string) Connection {
	t.Helper()
	// The profile is the ceiling within the surface, and the two have
	// to agree (ADR-W031 A3).
	profile := AgentClassMember
	if surface == ButlerSurfaceAdmin {
		profile = AgentClassOwner
	}
	if _, err := db.GetButler(butler); err != nil {
		if _, err := db.CreateButler(Butler{
			Name: butler, Directive: "look after the house", Surface: surface,
			AgentClass: profile, Model: "kitchen-box",
		}, "mika"); err != nil {
			t.Fatalf("CreateButler(%s): %v", butler, err)
		}
	}
	bot := "chat-" + transport + "-" + string(surface)
	if len(capability) > 0 {
		bot = capability[0]
	}
	if _, err := db.AddButlerAddress(butler, transport, bot, "", "mika"); err != nil {
		t.Fatalf("addressing %s at %s: %v", butler, bot, err)
	}
	at, err := db.ChatConnectionAt(transport, bot)
	if err != nil {
		t.Fatalf("resolving the connection at %s: %v", bot, err)
	}
	return at
}

// An Agent's Connection is its own. Admitting an account to Alfred does
// not admit it to Robert, which a surface could never express: both are
// Owner Agents, so `admin` meant both.
func TestAnAdmissionReachesOneAgentOnly(t *testing.T) {
	db := butlerFixture(t)
	alfred := connect(t, db, "alfred", "telegram", ButlerSurfaceAdmin)
	robert := connect(t, db, "robert", "telegram", ButlerSurfaceAdmin, "chat-telegram-robert")
	if alfred.Seq == robert.Seq {
		t.Fatal("two agents share one connection")
	}

	hash := "hash-alfred"
	if err := db.InsertChatClaimCode(hash, ChatClaimCode{
		Connection: alfred.Seq, Identity: "mika", CreatedBy: "root-admin",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RedeemChatClaimCode(hash, alfred, "12345", "mika", "conv-1"); err != nil {
		t.Fatalf("redeeming for alfred: %v", err)
	}

	if _, err := db.LookupChatBinding(alfred, "12345"); err != nil {
		t.Fatalf("the account is not admitted to the agent it was invited to: %v", err)
	}
	if _, err := db.LookupChatBinding(robert, "12345"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an account invited to alfred reaches robert too: err = %v", err)
	}
}

// And a code is spent on the Agent it names, or on nothing.
//
// Redeeming Alfred's code at Robert's bot used to SUCCEED, because both
// are the admin surface — so the person ended up admitted somewhere
// they were never invited, and the operator who issued the code had no
// way to tell.
func TestACodeForOneAgentIsRefusedAtAnother(t *testing.T) {
	db := butlerFixture(t)
	alfred := connect(t, db, "alfred", "telegram", ButlerSurfaceAdmin)
	robert := connect(t, db, "robert", "telegram", ButlerSurfaceAdmin, "chat-telegram-robert")

	hash := "hash-alfred"
	if err := db.InsertChatClaimCode(hash, ChatClaimCode{
		Connection: alfred.Seq, Identity: "mika", CreatedBy: "root-admin",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RedeemChatClaimCode(hash, robert, "12345", "mika", "conv-1"); !errors.Is(err, ErrClaimCode) {
		t.Fatalf("alfred's code was redeemed at robert's bot: err = %v", err)
	}
	// And it was NOT burned: the person can still use it on the right
	// bot a minute later.
	if _, err := db.RedeemChatClaimCode(hash, alfred, "12345", "mika", "conv-1"); err != nil {
		t.Fatalf("the code was spent by being presented to the wrong agent: %v", err)
	}
}

// The outbound queue is one Agent's. An adapter polling its own
// Connection sees its own replies and no other Agent's — which polling
// by surface could not do.
func TestTheOutboundQueueIsOneAgents(t *testing.T) {
	db := butlerFixture(t)
	if _, err := db.AcquireCoordinator("effect-fixture"); err != nil {
		t.Fatal(err)
	}
	alfred := connect(t, db, "alfred", "telegram", ButlerSurfaceAdmin)
	robert := connect(t, db, "robert", "telegram", ButlerSurfaceAdmin, "chat-telegram-robert")

	for _, q := range []struct {
		at   Connection
		body string
	}{{alfred, "alfred's answer"}, {robert, "robert's answer"}} {
		if _, err := db.EnqueueChatOutbound(ChatOutbound{
			Connection: q.at.Seq, Target: "conv-1", Body: q.body,
			DedupeKey: q.body, ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("queuing for %s: %v", q.at.Butler, err)
		}
	}

	got, err := db.ChatOutboundAfter(alfred, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "alfred's answer" {
		t.Fatalf("alfred's bot polled %d message(s) = %+v, want only its own", len(got), got)
	}
}

func TestWorkspaceDeliveryBindingRechecksConnectionAdmission(t *testing.T) {
	for _, mutation := range []string{
		"UPDATE connection_admission SET admits='owners'",
		"UPDATE connection_admission SET subject_kind='group'",
		"DELETE FROM connection_admission",
	} {
		t.Run(mutation, func(t *testing.T) {
			db := householdWith(t, "sam")
			at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)
			bind(t, db, at, "sam", "synthetic-account")
			if live, err := db.HasLivePersonChatBinding(at.Seq, "sam"); err != nil || !live {
				t.Fatalf("initial binding: live=%v err=%v", live, err)
			}
			if _, err := db.sql.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			if live, err := db.HasLivePersonChatBinding(at.Seq, "sam"); err != nil || live {
				t.Fatalf("changed admission: live=%v err=%v", live, err)
			}
		})
	}
}

// Removing a bot revokes its address, not its bindings. The live
// listing must not present such a binding as a way to reach the Agent;
// the audit listing still shows it.
func TestARemovedBotsBindingsLeaveTheLiveListing(t *testing.T) {
	db := butlerFixture(t)
	alfred := connect(t, db, "alfred", "telegram", ButlerSurfaceAdmin)
	bind(t, db, alfred, "mika", "12345")

	if got, err := db.ListChatBindings(false); err != nil || len(got) != 1 {
		t.Fatalf("live bindings = %+v (err %v), want one", got, err)
	}
	if err := db.RevokeButlerAddress("telegram", alfred.ExternalID, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if got, err := db.ListChatBindings(false); err != nil || len(got) != 0 {
		t.Fatalf("live bindings after removal = %+v (err %v), want none", got, err)
	}
	if got, err := db.ListChatBindings(true); err != nil || len(got) != 1 {
		t.Fatalf("audit bindings after removal = %+v (err %v), want one", got, err)
	}
}
