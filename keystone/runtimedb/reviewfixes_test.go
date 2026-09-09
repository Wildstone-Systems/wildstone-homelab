package runtimedb

// Regressions found in review of the ADR-W030 Part C branch. Each of
// these was a real defect, so each gets a test that fails without its
// fix rather than a note in a commit message.

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// An export is a read governed by current permissions (A3/A10). Your
// own forgotten records are yours to keep a copy of; other people's
// are not, or "forget that" would hold only until somebody exported.
func TestExportDoesNotReturnOtherPeoplesForgottenRecords(t *testing.T) {
	db := butlerTestDB(t)

	mine, err := db.RecordMemory(MemoryRecord{
		Kind: MemoryFact, Subject: "sam", Content: "sam sleeps badly on Sundays",
		Audience: ButlerAudiencePrivate, PermissionLabel: "sam", Owner: "sam",
		Author: "sam", SourceKind: "conversation", SourceID: "e-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := db.RecordMemory(MemoryRecord{
		Kind: MemoryFact, Subject: "household", Content: "the alarm code is 4821",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "household",
		Author: "mika", SourceKind: "conversation", SourceID: "e-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ForgetMemory(mine, "sam"); err != nil {
		t.Fatal(err)
	}
	if err := db.ForgetMemory(theirs, "mika"); err != nil {
		t.Fatal(err)
	}

	records, err := db.ExportMemoryFor("sam")
	if err != nil {
		t.Fatal(err)
	}
	var sawOwn, sawForgottenShared bool
	for _, r := range records {
		if r.Seq == mine {
			sawOwn = true
		}
		if r.Seq == theirs {
			sawForgottenShared = true
		}
	}
	if !sawOwn {
		t.Fatal("an export withheld the person's own forgotten record, which is theirs to have")
	}
	if sawForgottenShared {
		t.Fatal("an export handed back shared material that the household had forgotten")
	}
}

// A "proof" for a node that was never fenced must not delete a live
// placement: the epoch is never bumped, so the node it was running on
// is still holding a credential nothing revoked.
func TestProvingRemovalOfALivePlacementIsRefused(t *testing.T) {
	db := butlerTestDB(t)
	if _, err := db.CreateConnection(IntegrationConnection{
		Name: "house-calendar", Adapter: "caldav", Ownership: OwnershipHousehold,
		Owner: "household", CreatedBy: "root-admin",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPlacement("house-calendar", "node-a", PlacementPrimary, 1, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPlacementPrepared("house-calendar", "node-a", true, true, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.PlaceConnection("house-calendar", 0); err != nil {
		t.Fatal(err)
	}

	complete, err := db.ProvePlacementRemoval("house-calendar", "node-a", true, true, true)
	if complete || !errors.Is(err, ErrNotFound) {
		t.Fatalf("a live placement was removed by a proof that was never asked for: complete=%v err=%v", complete, err)
	}
	placements, err := db.ListPlacements("house-calendar")
	if err != nil {
		t.Fatal(err)
	}
	if len(placements) != 1 {
		t.Fatalf("the live placement was deleted: %+v", placements)
	}
}

// Placing the adapter somewhere healthy does not clear a removal that
// was never proven. A connection with a credential possibly still on a
// machine nobody watches is degraded, however well the new node answers.
func TestPlacementDoesNotClearAnUnprovenRemoval(t *testing.T) {
	db := butlerTestDB(t)
	if _, err := db.CreateConnection(IntegrationConnection{
		Name: "house-calendar", Adapter: "caldav", Ownership: OwnershipHousehold,
		Owner: "household", CreatedBy: "root-admin",
	}); err != nil {
		t.Fatal(err)
	}
	for i, node := range []string{"node-a", "node-b"} {
		if err := db.AddPlacement("house-calendar", node, PlacementFallback, i+1, "root-admin"); err != nil {
			t.Fatal(err)
		}
		if err := db.MarkPlacementPrepared("house-calendar", node, true, true, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := db.PlaceConnection("house-calendar", 0); err != nil {
		t.Fatal(err)
	}
	// node-a is being taken out and only PART of it is proven.
	if err := db.BeginPlacementRemoval("house-calendar", "node-a"); err != nil {
		t.Fatal(err)
	}
	if complete, err := db.ProvePlacementRemoval("house-calendar", "node-a", true, false, false); err != nil || complete {
		t.Fatalf("a partial proof completed the removal: complete=%v err=%v", complete, err)
	}

	ready, err := db.MarkReadyUnlessRemoving("house-calendar")
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("the connection reported Ready while a removal was still unproven")
	}
	conn, err := db.GetConnection("house-calendar")
	if err != nil {
		t.Fatal(err)
	}
	if conn.State != ConnectionDegraded {
		t.Fatalf("connection state = %q, want degraded", conn.State)
	}
	if !strings.Contains(conn.DegradedReason, "node-a") {
		t.Fatalf("the degraded reason does not name the node: %q", conn.DegradedReason)
	}
}

// MaxConcurrency is what stops a single local GPU being oversubscribed.
// Counting and then inserting is two statements with a gap; concurrent
// callers must not both get the one slot.
func TestRouteCapacityHoldsUnderConcurrentSelection(t *testing.T) {
	db := butlerTestDB(t)
	if err := db.UpsertModelRoute(ModelRoute{
		Name: "kitchen-box", Kind: RouteLocal, Provider: "ollama", Model: "llama",
		Readiness: RouteReady, MaxConcurrency: 1, Priority: 10, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}

	const callers = 8
	var wg sync.WaitGroup
	got := make([]bool, callers)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := db.SelectModelRoute(RouteRequest{
				Locality: DataLocalOnly, Holder: "task", Job: JobInteractive,
			})
			got[i] = err == nil
		}(i)
	}
	wg.Wait()

	won := 0
	for _, ok := range got {
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d callers got the one slot on a MaxConcurrency=1 route", won)
	}
}

// Escalation goes somewhere ELSE. A retry that lands back on the route
// that just failed fails the same way.
func TestEscalationDoesNotReturnToARouteAlreadyTried(t *testing.T) {
	db := butlerTestDB(t)
	for _, r := range []ModelRoute{
		{Name: "kitchen-box", Kind: RouteLocal, Provider: "ollama", Model: "llama",
			Readiness: RouteReady, MaxConcurrency: 4, Priority: 10, Enabled: true},
		{Name: "cloud", Kind: RouteCloud, Provider: "anthropic", Model: "sonnet",
			Readiness: RouteReady, MaxConcurrency: 4, Priority: 200, Enabled: true},
	} {
		if err := db.UpsertModelRoute(r, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}

	first, err := db.SelectModelRoute(RouteRequest{
		Locality: DataCloudAllowed, Holder: "task-1", Job: JobInteractive,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The call failed and the slot went back, so capacity alone would
	// hand the same route straight back.
	db.ReleaseModelRoute(first.LeaseSeq)

	second, err := db.SelectModelRoute(RouteRequest{
		Locality: DataCloudAllowed, Holder: "task-1", Job: JobInteractive,
		Attempt: 1, Tried: []string{first.Route.Name},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Route.Name == first.Route.Name {
		t.Fatalf("the escalation went back to %s, which had just failed", first.Route.Name)
	}
	if !second.Escalated {
		t.Fatal("an escalated selection did not report itself as one")
	}

	// With every route tried, it refuses rather than looping.
	if _, err := db.SelectModelRoute(RouteRequest{
		Locality: DataCloudAllowed, Holder: "task-1", Job: JobInteractive,
		Attempt: 2, Tried: []string{"kitchen-box", "cloud"},
	}); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("selection found a route after every one had been tried: %v", err)
	}
}

// Turning a tool on says the tool EXISTS here. It does not say this
// piece of work may use it: a grant-required tool still needs its
// grant, or an administrator's enable becomes a way around the
// per-work authorization that made it grant-required (A7).
func TestEnablingAToolDoesNotBypassARequiredGrant(t *testing.T) {
	db := butlerTestDB(t)
	if err := db.SetToolControl(ProfileHousehold, "send_message", ToolEnabled,
		"the family surface may message", "root-admin"); err != nil {
		t.Fatal(err)
	}

	a, err := db.ResolveToolAuthority(ToolQuery{
		Tool: "send_message", Profile: ProfileHousehold, Structural: true,
		GrantRequired: true, TaskID: "bt-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Allowed {
		t.Fatal("an enabled tool was allowed without the grant it requires")
	}
	if !a.Requestable {
		t.Fatal("the refusal did not leave the capability requestable")
	}

	// A tool that needs no grant is still allowed by the enable.
	b, err := db.ResolveToolAuthority(ToolQuery{
		Tool: "send_message", Profile: ProfileHousehold, Structural: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !b.Allowed || b.Source != "enabled" {
		t.Fatalf("an enabled tool that needs no grant was refused: %+v", b)
	}
}
