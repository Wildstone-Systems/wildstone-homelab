package agentgateway

// Recall's disclosure must be identical to the recall TOOL's, because a
// runtime that asks for structured hits must never see more — or less —
// than the same task's model sees rendered as text (ADR-W039 §3.5,
// Phase 3, #338). Both go through the same handle, the same
// BindMemoryRead and the same Toolset.Permits gate; these tests are the
// end-to-end proof that they agree, across personal, agent_local and
// promoted-household scope, and across a Console-owner task and an
// ordinary one.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// butlerGatewayFunc builds a fresh, unmemoized Toolset per call — what
// cmd/keystone's own factory does in production — for the one test here
// that opens the same Butler as two different members and must not see
// the fixture's per-Butler surface cache answer for the wrong one.
func butlerGatewayFunc(f *fixture) butlerlive.GatewayFunc {
	return func(b runtimedb.Butler, turn butlerlive.Turn) (butler.Toolset, error) {
		return f.surfaceFor(b, turn), nil
	}
}

// recallBoth runs the same search through Recall and through the
// "recall" tool (via Invoke) on the same handle, and returns Recall's
// structured hits alongside the tool's rendered text.
func recallBoth(t *testing.T, g *Gateway, h agentapi.Handle, text string) ([]agentapi.MemoryHit, string) {
	t.Helper()
	ctx := context.Background()
	hits, err := g.Recall(ctx, agentapi.RecallRequest{Handle: h, Text: text})
	if err != nil {
		t.Fatalf("Recall(%q): %v", text, err)
	}
	args, err := json.Marshal(map[string]any{"text": text, "limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	res, err := g.Invoke(ctx, agentapi.InvokeRequest{Handle: h, Tool: "recall", Args: args})
	if err != nil {
		t.Fatalf("Invoke(recall, %q): %v", text, err)
	}
	if res.Outcome != agentapi.OutcomeOK {
		t.Fatalf("Invoke(recall, %q) outcome = %s: %s", text, res.Outcome, res.Reason)
	}
	return hits, res.Content
}

// assertSameDisclosure checks that Recall's structured hits are exactly
// the set of records the tool's rendered text names — same count, and
// every hit's content appears in the tool's own rendering (comparing by
// content rather than a numeric id, since neither MemoryHit nor the
// rendered text carries a memory sequence number to a runtime).
func assertSameDisclosure(t *testing.T, label string, hits []agentapi.MemoryHit, toolText string, wantCount int) {
	t.Helper()
	if len(hits) != wantCount {
		t.Fatalf("%s: Recall returned %d hits, want %d: %+v", label, len(hits), wantCount, hits)
	}
	if wantCount == 0 {
		if !strings.Contains(toolText, "nothing remembered") {
			t.Fatalf("%s: Recall found nothing but the tool said %q", label, toolText)
		}
		return
	}
	for _, h := range hits {
		if !strings.Contains(toolText, h.Content) {
			t.Fatalf("%s: Recall hit %q does not appear in the tool's own text %q", label, h.Content, toolText)
		}
	}
	if got := strings.Count(toolText, "\n- "); got+1 != wantCount && got != wantCount {
		// The tool renders one "- " bullet per hit (the first with no
		// leading newline); either count form covers both.
		t.Fatalf("%s: tool text does not name %d records: %q", label, wantCount, toolText)
	}
}

// TestRecallDisclosesTheSameRecordsAsTheTool covers personal,
// agent_local and promoted-household scope, and a private record's
// absence for a person it is not the speaker's, for the SAME task —
// the core parity guarantee.
func TestRecallDisclosesTheSameRecordsAsTheTool(t *testing.T) {
	f := newFixture(t)
	// The fixture's Gateways caches one wrapped surface per Butler name
	// for the lifetime of the test (fixture_test.go's own note: "so a
	// dispatch count survives the per-call rebuild"), built with
	// whichever member's turn it first sees. This test opens "joan" as
	// two different members, which production never does through one
	// cached instance (cmd/keystone's own factory builds fresh every
	// call) — so it rebuilds fresh per call here too, the same surface
	// with no memoization to go stale across members.
	f.g.Gateways = butlerGatewayFunc(f)
	ctx := context.Background()

	_, samHandle := f.openTask(t, "joan", "sam")
	_, mikaHandle := f.openTask(t, "joan", "mika")

	remember := func(h agentapi.Handle, scope, marker string) {
		t.Helper()
		args, err := json.Marshal(map[string]any{"content": "note about " + marker, "scope": scope})
		if err != nil {
			t.Fatal(err)
		}
		res, err := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: h, Tool: "remember", Args: args})
		if err != nil || res.Outcome != agentapi.OutcomeOK {
			t.Fatalf("remember(%s, %s): %+v, %v", scope, marker, res, err)
		}
	}

	// Personal: sam's own, findable by sam, absent for mika.
	remember(samHandle, "personal", "marker-personal")
	// Agent-local: sam's, scoped to this Agent, absent for mika.
	remember(samHandle, "agent_local", "marker-agentlocal")

	for _, tc := range []struct {
		label  string
		handle agentapi.Handle
		marker string
		want   int
	}{
		{"personal, the speaker", samHandle, "marker-personal", 1},
		{"personal, another person", mikaHandle, "marker-personal", 0},
		{"agent_local, the speaker", samHandle, "marker-agentlocal", 1},
		{"agent_local, another person", mikaHandle, "marker-agentlocal", 0},
	} {
		hits, text := recallBoth(t, f.g, tc.handle, tc.marker)
		assertSameDisclosure(t, tc.label, hits, text, tc.want)
	}

	// Promotion goes through the Person, not through remember (§3.5
	// rule 1) — db.PromotePersonalMemory is the Person-approved control
	// the ADR names, exercised directly as the household dashboard
	// would exercise it.
	records, err := f.db.RetrieveMemory(runtimedb.MemoryQuery{Reader: "sam", Butler: "joan"})
	if err != nil {
		t.Fatal(err)
	}
	var personalSeq int64
	for _, r := range records {
		if strings.Contains(r.Content, "marker-personal") {
			personalSeq = r.Seq
		}
	}
	if personalSeq == 0 {
		t.Fatal("the personal record to promote was not found")
	}
	if _, err := f.db.PromotePersonalMemory(personalSeq, "sam"); err != nil {
		t.Fatal(err)
	}

	// Promoted (household) memory is now visible via either person's
	// task on this Agent, and the two retrieval paths still agree on
	// what it is — that agreement, not which scope wins for the
	// original speaker (butlerstore's own tests cover that), is what
	// this test is for.
	for _, tc := range []struct {
		label  string
		handle agentapi.Handle
	}{
		{"promoted, the original speaker", samHandle},
		{"promoted, another household member", mikaHandle},
	} {
		hits, text := recallBoth(t, f.g, tc.handle, "marker-personal")
		assertSameDisclosure(t, tc.label, hits, text, len(hits))
		if len(hits) == 0 {
			t.Fatalf("%s: the promoted record is not visible on either path", tc.label)
		}
	}
	// Another household member, who has no personal record of their
	// own under this marker, can only be seeing the promoted copy —
	// and both paths agree it is household-scoped.
	mikaHits, _ := recallBoth(t, f.g, mikaHandle, "marker-personal")
	if len(mikaHits) != 1 || mikaHits[0].Scope != agentapi.MemoryScopeHousehold {
		t.Fatalf("another member's view of the promotion = %+v, want one household-scoped hit", mikaHits)
	}
}

// TestRecallDisclosesTheSameRecordsConsoleOwnerOrNot covers the other
// axis §3.5 rule 3 names: a Console-owner task and an ordinary one.
// Whatever the boundary decides for a given task, Recall and the tool
// must decide it identically — this does not assert which way the
// boundary itself falls (butlerstore's own tests do that), only that
// the two retrieval paths never disagree about it.
func TestRecallDisclosesTheSameRecordsConsoleOwnerOrNot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, owner := f.openTaskAs(t, "joan", "mika", true, "")
	_, plain := f.openTaskAs(t, "joan", "mika", false, "")

	args, err := json.Marshal(map[string]any{"content": "the owner boundary marker", "scope": "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: owner, Tool: "remember", Args: args}); err != nil || res.Outcome != agentapi.OutcomeOK {
		t.Fatalf("remember: %+v, %v", res, err)
	}

	for _, tc := range []struct {
		label  string
		handle agentapi.Handle
	}{
		{"as Console owner", owner},
		{"not as Console owner", plain},
	} {
		hits, text := recallBoth(t, f.g, tc.handle, "owner boundary marker")
		// Both are mika's own record either way (he spoke it), so both
		// tasks see it — the point of this test is that they see it
		// THROUGH THE SAME COUNT on both paths, not a particular count.
		assertSameDisclosure(t, tc.label, hits, text, len(hits))
		if len(hits) != 1 {
			t.Fatalf("%s: hits = %+v, want the speaker's own record once", tc.label, hits)
		}
	}
}

// TestRecallRefusesWhereTheToolRefuses: an operator-surface handle has
// no memory tool at all, and Recall must refuse it exactly as Invoke
// does rather than reaching around the missing seam.
func TestRecallRefusesWhereTheToolRefuses(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, admin := f.openTask(t, "alfred", "mika")

	toolRes, toolErr := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: admin, Tool: "recall", Args: json.RawMessage(`{"text":"anything"}`)})
	if toolErr != nil {
		t.Fatalf("Invoke(recall) on the operator surface errored: %v", toolErr)
	}
	if toolRes.Outcome == agentapi.OutcomeOK {
		t.Fatalf("the operator surface offered recall: %+v", toolRes)
	}

	if _, err := f.g.Recall(ctx, agentapi.RecallRequest{Handle: admin, Text: "anything"}); err == nil {
		t.Fatal("Recall answered on the operator surface, where the tool refuses")
	} else if err.Error() == "" {
		t.Fatal("Recall's refusal carries no reason")
	}
}
