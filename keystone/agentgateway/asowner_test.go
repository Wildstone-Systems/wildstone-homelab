package agentgateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// A Console-owner turn gets the owner disclosure path — the existing
// trusted access-list exemption memoryBoundaryClause carries
// (ADR-W031 A10) — and an otherwise identical non-owner turn does not
// (ADR-W039 Phase 2b, #337): AsOwner is recorded on the task at open
// (migration 0083) and read back by turnFor into the Turn a resolved
// handle stands for; before this it was not derivable from the handle
// at all, and every gateway-served turn saw the non-owner path only.
//
// Mirrors keystone/butlerlive's own
// TestConsoleOwnerMemoryRetainsTheExistingAgentAccessExemption: the
// owner's OWN memory, recorded directly (never through the remember
// tool, which refuses anything but personal/agent_local scope and so
// could never produce the record this exemption is about), with the
// Agent's own access grant to the owner revoked.
func TestAConsoleOwnerTurnGetsTheOwnerDisclosurePathAndANonOwnerTurnDoesNot(t *testing.T) {
	const content = "the owner peppermint preference"

	// Each case gets its own fixture, on purpose: newFixture's Gateways
	// closure caches one Toolset (and the MemorySeams bound into it)
	// per Butler name for the lifetime of the test, so a dispatch count
	// survives a per-call rebuild (fixture_test.go's own comment) — but
	// that means a SECOND turn against the same Butler in the same
	// fixture would be served by the FIRST turn's already-bound
	// destination, which is exactly the variable this test needs to
	// change between its two cases.
	recallFor := func(t *testing.T, asOwner bool) agentapi.InvokeResult {
		t.Helper()
		f := newFixture(t)
		own := runtimedb.MemoryRecord{
			Kind: runtimedb.MemoryPreference, Content: content,
			Owner: "mika", Author: "mika", Butler: "joan", Scope: runtimedb.MemoryScopePersonal,
			Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "household",
			SourceKind: "conversation", SourceID: "fixture",
		}
		seq, err := f.db.RecordMemory(own)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.IndexDocumentInto(runtimedb.IndexDocument{
			MemorySeq: seq, SourceKind: own.SourceKind, SourceID: own.SourceID,
			Content: own.Content, Owner: own.Owner, Audience: own.Audience, PermissionLabel: own.PermissionLabel,
		}); err != nil {
			t.Fatal(err)
		}
		// The owner has no explicit grant to talk to this family Agent
		// at all — the exact shape the existing exemption exists for.
		if err := f.db.RevokeButlerAccess("joan", "mika", "mika"); err != nil {
			t.Fatal(err)
		}
		_, h := f.openTaskAs(t, "joan", "mika", asOwner, "")
		recall, _ := json.Marshal(map[string]any{"text": "peppermint"})
		res, err := f.g.Invoke(context.Background(), agentapi.InvokeRequest{Handle: h, Tool: "recall", Args: recall})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		return res
	}

	// A non-owner turn: ungranted, so the ordinary access-list path in
	// memoryBoundaryClause refuses and the record is withheld.
	plain := recallFor(t, false)
	if plain.Outcome != agentapi.OutcomeOK {
		t.Fatalf("non-owner recall = %+v; want ok (recall itself always runs; the record is what is withheld)", plain)
	}
	if strings.Contains(plain.Content, content) {
		t.Fatal("an ungranted, non-owner turn saw the owner's own memory through an Agent it has no access to")
	}

	// The identical turn, but AsOwner — the Console exemption a
	// request carries, recorded on the task at open and read back from
	// the handle by turnFor. The owner's own record is now visible.
	owner := recallFor(t, true)
	if owner.Outcome != agentapi.OutcomeOK || !strings.Contains(owner.Content, content) {
		t.Fatalf("owner recall = %+v; want the record the ownerAccess exemption discloses", owner)
	}
}

// SystemExtra, the other request-time fact migration 0083 carries,
// reaches the assembled prompt through the same handle round trip.
func TestSystemExtraReachesTheManifestPromptFromTheHandle(t *testing.T) {
	f := newFixture(t)
	const note = "This is a group chat; address the room, not one person."
	_, h := f.openTaskAs(t, "alfred", "mika", false, note)
	m, err := f.g.Manifest(context.Background(), h)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if !strings.Contains(m.SystemPrompt, note) {
		t.Fatalf("SystemExtra recorded on the task at open did not reach the manifest's prompt:\n%s", m.SystemPrompt)
	}
}
