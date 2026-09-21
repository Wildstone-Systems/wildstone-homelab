package butlerstore

import (
	"context"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// F-156-08 at the real gateway/store boundary. Slice 11 already rejects
// rooms at normal ingress; that outer rejection must not stand in for
// destination-aware memory authorization at recall and context assembly.
func TestRecallDoesNotReachAGroupRoom(t *testing.T) {
	db, authority := authorityDB(t)
	ctx := context.Background()
	const privateNote = "Prefers jasmine tea before private appointments"
	memory := testMemorySeams(db, "alfred", "sam", nil)
	remember(t, memory, butler.MemoryNote{Kind: runtimedb.MemoryPreference, Content: privateNote})
	seams := butler.HouseholdSeams{Memory: memory}
	direct := butler.NewHouseholdGateway("sam", seams)
	room := butler.NewHouseholdGroupGateway("sam", seams)
	for _, gw := range []*butler.HouseholdGateway{direct, room} {
		gw.SetAuthorizer(authority)
		gw.SetCaller(butler.CallerPersonFacing, "sam", "test_connection")
	}
	// Positive controls prevent an unrelated admission refusal or unwired seam
	// from making either negative assertion pass without testing disclosure.
	directResult, err := direct.Dispatch(ctx, "recall", map[string]any{"text": "jasmine"})
	if err != nil || directResult.Rejected || !strings.Contains(directResult.Content, privateNote) {
		t.Fatalf("direct recall control: %+v, %v", directResult, err)
	}
	if !strings.Contains(direct.MemoryPreamble(ctx), privateNote) {
		t.Fatal("direct preamble control did not contain the preference")
	}
	t.Run("retrieval", func(t *testing.T) {
		result, err := room.Dispatch(ctx, "recall", map[string]any{"text": "jasmine"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(result.Content, privateNote) {
			t.Fatalf("private memory reached the group recall result: %s", result.Content)
		}
	})
	t.Run("assembled_context", func(t *testing.T) {
		// This is the gateway method called by butlerlive.systemFor before inference.
		if prompt := room.MemoryPreamble(ctx); strings.Contains(prompt, privateNote) {
			t.Fatalf("private memory reached the group preamble: %s", prompt)
		}
	})
}
