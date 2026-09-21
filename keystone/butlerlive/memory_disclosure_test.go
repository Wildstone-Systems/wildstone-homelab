package butlerlive

import (
	"context"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Exercise Serve and capture the provider's actual input, below the HTTP
// ingress guard. The private preference needs no recall call to leak.
func TestGroupMemoryDoesNotReachTheProvider(t *testing.T) {
	for _, group := range []bool{false, true} {
		name := "direct_positive_control"
		if group {
			name = "group_denial"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.db.CreateButler(runtimedb.Butler{Name: "memory-agent", Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "kitchen-box"}, "mika"); err != nil {
				t.Fatal(err)
			}
			if err := f.db.GrantButlerAccess("memory-agent", "sam", "mika"); err != nil {
				t.Fatal(err)
			}
			const note = "Prefers jasmine tea before private appointments"
			if _, err := f.db.RecordMemory(runtimedb.MemoryRecord{Scope: runtimedb.MemoryScopePersonal, Kind: runtimedb.MemoryPreference, Content: note, Owner: "sam", Author: "sam", Butler: "memory-agent", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "household", SourceKind: "conversation", SourceID: "fixture"}); err != nil {
				t.Fatal(err)
			}
			f.butlers.Gateways = GatewayFunc(func(b runtimedb.Butler, turn Turn) (butler.Toolset, error) {
				seams := butler.HouseholdSeams{Memory: butlerstore.MemorySeams(f.db, b.Name, turn.Member, turn.MemoryDestination(), nil)}
				if turn.Group {
					return butler.NewHouseholdGroupGateway(turn.Member, seams), nil
				}
				return butler.NewHouseholdGateway(turn.Member, seams), nil
			})
			_, err := f.butlers.Serve(context.Background(), Turn{Butler: "memory-agent", Member: "sam", Text: "Hello", Group: group, Connection: ConnectionScope{Name: portal.Connection, Permits: portal.Permits}})
			if group {
				if f.local.sawText(note) {
					t.Fatal("private preference reached the provider on a group turn")
				}
			} else if err != nil || !f.local.sawText(note) {
				t.Fatalf("direct provider positive control failed: %v", err)
			}
		})
	}
}

func memoryFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	if _, err := f.db.CreateButler(runtimedb.Butler{Name: "memory-agent", Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "kitchen-box"}, "mika"); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"sam", "mika"} {
		if err := f.db.GrantButlerAccess("memory-agent", who, "mika"); err != nil {
			t.Fatal(err)
		}
	}
	f.butlers.Gateways = GatewayFunc(func(b runtimedb.Butler, turn Turn) (butler.Toolset, error) {
		seams := butler.HouseholdSeams{Memory: butlerstore.MemorySeams(f.db, b.Name, turn.Member, turn.MemoryDestination(), nil)}
		if turn.Group {
			return butler.NewHouseholdGroupGateway(turn.Member, seams), nil
		}
		return butler.NewHouseholdGateway(turn.Member, seams), nil
	})
	return f
}

func putMemory(t *testing.T, f *fixture, owner string, audience runtimedb.ButlerTaskAudience, scope string) int64 {
	t.Helper()
	m := runtimedb.MemoryRecord{Scope: scope, Kind: runtimedb.MemoryPreference, Content: "jasmine private canary", Owner: owner, Author: "sam", Butler: "memory-agent", Audience: audience, PermissionLabel: "household", SourceKind: "conversation", SourceID: "fixture"}
	seq, err := f.db.RecordMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.IndexDocumentInto(runtimedb.IndexDocument{MemorySeq: seq, SourceKind: m.SourceKind, SourceID: m.SourceID, Content: m.Content, Owner: owner, Audience: audience, PermissionLabel: "household"}); err != nil {
		t.Fatal(err)
	}
	return seq
}

func assertMemoryPaths(t *testing.T, f *fixture, person string, want bool) {
	t.Helper()
	turn := Turn{Butler: "memory-agent", Member: person, Text: "Hello", Connection: ConnectionScope{Name: portal.Connection, Permits: portal.Permits}}
	t.Run("retrieval", func(t *testing.T) {
		seams := butlerstore.MemorySeams(f.db, "memory-agent", person, turn.MemoryDestination(), nil)
		hits, err := seams.Recall(context.Background(), "jasmine", 10)
		if err != nil {
			t.Fatal(err)
		}
		if (len(hits) > 0) != want {
			t.Fatalf("recall for %s: %+v", person, hits)
		}
	})
	t.Run("assembled_provider_context", func(t *testing.T) {
		f.local.saw = nil
		_, err := f.butlers.Serve(context.Background(), turn)
		if err != nil {
			t.Fatal(err)
		}
		if f.local.sawText("jasmine private canary") != want {
			t.Fatalf("provider memory for %s: want %v, got %v", person, want, f.local.saw)
		}
	})
}

func TestSharedAgentReturnsNothingFromAnotherPersonsPersonalMemory(t *testing.T) {
	f := memoryFixture(t)
	putMemory(t, f, "sam", runtimedb.ButlerAudiencePrivate, runtimedb.MemoryScopePersonal)
	assertMemoryPaths(t, f, "sam", true)
	assertMemoryPaths(t, f, "mika", false)
}

func TestRestrictedHouseholdRecordIsExcludedForTheWrongAudience(t *testing.T) {
	f := memoryFixture(t)
	g, err := f.db.CreateGroup("Memory readers")
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.AddPersonToGroup(g.ID, p.PersonID, "mika"); err != nil {
		t.Fatal(err)
	}
	a, err := f.db.CreateAudienceForGroups("Memory readers", g.ID)
	if err != nil {
		t.Fatal(err)
	}
	putMemory(t, f, "household", runtimedb.ButlerTaskAudience(a.ID), runtimedb.MemoryScopeHousehold)
	assertMemoryPaths(t, f, "sam", true)
	assertMemoryPaths(t, f, "mika", false)
	if err = f.db.RemovePersonFromGroup(g.ID, p.PersonID); err != nil {
		t.Fatal(err)
	}
	assertMemoryPaths(t, f, "sam", false)
}

func TestPerAgentRestrictionNeverWidensTheActivePersonCheck(t *testing.T) {
	f := memoryFixture(t)
	g, err := f.db.CreateGroup("Restricted readers")
	if err != nil {
		t.Fatal(err)
	}
	person, err := f.db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.AddPersonToGroup(g.ID, person.PersonID, "mika"); err != nil {
		t.Fatal(err)
	}
	audience, err := f.db.CreateAudienceForGroups("Restricted household record", g.ID)
	if err != nil {
		t.Fatal(err)
	}
	putMemory(t, f, "household", runtimedb.ButlerTaskAudience(audience.ID), runtimedb.MemoryScopeHousehold)
	for _, restricted := range []bool{true, false} {
		if err := f.db.SetAgentMemoryRestriction("memory-agent", runtimedb.MemoryScopeHousehold, restricted, "mika"); err != nil {
			t.Fatal(err)
		}
		assertMemoryPaths(t, f, "mika", false)
		assertMemoryPaths(t, f, "sam", !restricted)
	}
}

func TestAdmittedAgentUsesTheAdmittingPersonsPersonalMemory(t *testing.T) {
	f := memoryFixture(t)
	// The source Agent differs from the answering Agent. Personal follows Person.
	m := runtimedb.MemoryRecord{Kind: runtimedb.MemoryPreference, Content: "jasmine private canary", Owner: "sam", Author: "sam", Butler: "alfred", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "household", SourceKind: "conversation", SourceID: "fixture"}
	seq, err := f.db.RecordMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.IndexDocumentInto(runtimedb.IndexDocument{MemorySeq: seq, SourceKind: m.SourceKind, SourceID: m.SourceID, Content: m.Content, Owner: m.Owner, Audience: m.Audience, PermissionLabel: "household"}); err != nil {
		t.Fatal(err)
	}
	assertMemoryPaths(t, f, "sam", true)
}

func TestMissingDestinationAndRevokedPersonDenyMemory(t *testing.T) {
	f := memoryFixture(t)
	putMemory(t, f, "sam", runtimedb.ButlerAudiencePrivate, runtimedb.MemoryScopePersonal)
	for _, d := range []runtimedb.MemoryDestination{{}, {BuiltIn: true, Group: true}, {ConnectionRef: 999}, {BuiltIn: true, ConnectionRef: 999}} {
		s := butlerstore.MemorySeams(f.db, "memory-agent", "sam", d, nil)
		hits, err := s.Recall(context.Background(), "jasmine", 10)
		if err != nil || len(hits) != 0 {
			t.Fatalf("destination %+v: %v %v", d, hits, err)
		}
		p, err := s.Preamble(context.Background())
		if err != nil || len(p) != 0 {
			t.Fatalf("preamble destination %+v: %v %v", d, p, err)
		}
	}
	// Reuse the same closure after permission changes, not a reconstructed one.
	s := butlerstore.MemorySeams(f.db, "memory-agent", "sam", runtimedb.MemoryDestination{BuiltIn: true}, nil)
	if err := f.db.DisableButlerMember("sam", "mika"); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Recall(context.Background(), "jasmine", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("disabled person: %v %v", hits, err)
	}
	p, err := s.Preamble(context.Background())
	if err != nil || len(p) != 0 {
		t.Fatalf("disabled preamble: %v %v", p, err)
	}
}

func TestVerifiedRecallRechecksCurrentRecordPermission(t *testing.T) {
	f := memoryFixture(t)
	putMemory(t, f, "sam", runtimedb.ButlerAudiencePrivate, runtimedb.MemoryScopePersonal)
	f.local.toolCalls = []provider.ToolCall{{ID: "recall-1", Name: "recall", Input: map[string]any{"text": "jasmine"}}}
	f.local.dieAfter = 1
	turn := Turn{Butler: "memory-agent", Member: "sam", Text: "Recall jasmine", Connection: ConnectionScope{Name: portal.Connection, Permits: portal.Permits}}
	first, _ := f.butlers.Serve(context.Background(), turn)
	if len(first.Calls) == 0 || !strings.Contains(first.Calls[0].Result, "jasmine private canary") {
		t.Fatalf("first recall control: %+v", first)
	}
	session, err := f.db.InsertButlerSession(runtimedb.ButlerSession{Principal: "butler", ActingFor: "sam", Butler: "memory-agent", Mode: runtimedb.ButlerModeFull, Surface: runtimedb.ButlerSurfaceFamily})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.SetButlerTaskSession(first.TaskID, session); err != nil {
		t.Fatal(err)
	}
	if err = f.db.SetAgentMemoryRestriction("memory-agent", runtimedb.MemoryScopePersonal, true, "mika"); err != nil {
		t.Fatal(err)
	}
	f.local.revive()
	f.local.saw = nil
	turn.SessionID = session
	second, err := f.butlers.Serve(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Resumed || len(second.Calls) == 0 {
		t.Fatalf("did not resume recall: %+v", second)
	}
	for _, c := range second.Calls {
		if strings.Contains(c.Result, "jasmine private canary") {
			t.Fatal("verified recall bypassed current restriction")
		}
	}
	if f.local.sawText("jasmine private canary") {
		t.Fatal("revoked memory reached resumed provider")
	}
}

func TestExternalDisclosureIsAppliedBeforeTheProvider(t *testing.T) {
	for _, compatible := range []bool{true, false} {
		t.Run(map[bool]string{true: "compatible", false: "incompatible"}[compatible], func(t *testing.T) {
			f := memoryFixture(t)
			ref, err := f.db.AddButlerAddress("memory-agent", "telegram", "memory-bot", "", "mika")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.db.ExecForTest(`UPDATE connection_disclosure SET retention='provider' WHERE connection=?`, ref); err != nil {
				t.Fatal(err)
			}
			at, err := f.db.ChatConnectionBySeq(ref)
			if err != nil {
				t.Fatal(err)
			}
			task, err := f.db.InsertButlerTask(runtimedb.ButlerTask{Kind: runtimedb.ButlerTaskConversation, Author: "sam", Owner: "sam", Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Audience: runtimedb.ButlerAudiencePrivate, Objective: "remember", OriginConnection: at.Name(), OriginConnectionRef: ref})
			if err != nil {
				t.Fatal(err)
			}
			seams := butlerstore.MemorySeams(f.db, "memory-agent", "sam", runtimedb.MemoryDestination{ConnectionRef: ref}, nil)
			if err = seams.Remember(context.Background(), butler.MemoryNote{Kind: runtimedb.MemoryPreference, Content: "jasmine external canary", SourceKind: "conversation", SourceID: task}); err != nil {
				t.Fatal(err)
			}
			if !compatible {
				if _, err = f.db.ExecForTest(`UPDATE connection_disclosure SET encryption='end_to_end' WHERE connection=?`, ref); err != nil {
					t.Fatal(err)
				}
			}
			turn := Turn{Butler: "memory-agent", Member: "sam", Text: "Hello", Connection: ConnectionScope{Name: at.Name(), Ref: ref, Permits: portal.Permits}}
			_, err = f.butlers.Serve(context.Background(), turn)
			if err != nil {
				t.Fatal(err)
			}
			if f.local.sawText("jasmine external canary") != compatible {
				t.Fatalf("provider compatibility=%v: %v", compatible, f.local.saw)
			}
			hits, err := seams.Recall(context.Background(), "jasmine", 10)
			if err != nil || (len(hits) > 0) != compatible {
				t.Fatalf("recall compatibility=%v: %v %v", compatible, hits, err)
			}
		})
	}
}

func TestConsoleOwnerMemoryRetainsTheExistingAgentAccessExemption(t *testing.T) {
	f := memoryFixture(t)
	putMemory(t, f, "sam", runtimedb.ButlerAudiencePrivate, runtimedb.MemoryScopePersonal)
	own := runtimedb.MemoryRecord{Kind: runtimedb.MemoryPreference, Content: "owner's peppermint preference", Owner: "mika", Author: "mika", Butler: "alfred", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "household", SourceKind: "conversation", SourceID: "fixture"}
	if _, err := f.db.RecordMemory(own); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RevokeButlerAccess("memory-agent", "mika", "mika"); err != nil {
		t.Fatal(err)
	}
	turn := Turn{Butler: "memory-agent", Member: "mika", AsOwner: true, Text: "Hello", Connection: ConnectionScope{Name: ConsoleConnection}}
	_, err := f.butlers.Serve(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if !f.local.sawText(own.Content) {
		t.Fatal("Console owner lost their own memory solely because the Agent has no explicit access grant")
	}
	if f.local.sawText("jasmine private canary") {
		t.Fatal("Console ownership widened another Person's memory audience")
	}
}

func TestConsoleMemoryExemptionRequiresOwnerRole(t *testing.T) {
	f := memoryFixture(t)
	putMemory(t, f, "sam", runtimedb.ButlerAudiencePrivate, runtimedb.MemoryScopePersonal)
	if err := f.db.RevokeButlerAccess("memory-agent", "sam", "mika"); err != nil {
		t.Fatal(err)
	}
	seams := butlerstore.MemorySeams(f.db, "memory-agent", "sam", runtimedb.MemoryDestination{BuiltIn: true, ConsoleOwner: true}, nil)
	hits, err := seams.Recall(context.Background(), "jasmine", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("Member used Console exemption: %v %v", hits, err)
	}
	preamble, err := seams.Preamble(context.Background())
	if err != nil || len(preamble) != 0 {
		t.Fatalf("Member preamble used Console exemption: %v %v", preamble, err)
	}
}
