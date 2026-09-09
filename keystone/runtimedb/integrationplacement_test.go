package runtimedb

// P11's evidence (ADR-W030): failover moves only the lease among
// approved prepared nodes and never duplicates work or creates a
// container during an outage; no eligible node produces a wait;
// personal secrets never enter Keystone; a referenced secret cannot be
// erased; and incomplete cleanup leaves the connection visibly degraded.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func connectionFixture(t *testing.T, db *DB) {
	t.Helper()
	if _, err := db.CreateConnection(IntegrationConnection{
		Name: "calendar", Adapter: "caldav", Ownership: OwnershipHousehold,
		Owner: "household", CreatedBy: "root-admin",
	}); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
}

func prepare(t *testing.T, db *DB, node string) {
	t.Helper()
	if err := db.MarkPlacementPrepared("calendar", node, true, true, true); err != nil {
		t.Fatalf("MarkPlacementPrepared(%s): %v", node, err)
	}
}

// A declared node is not an eligible one. Adding a fallback is a Plan,
// and it is incomplete until the container is running, the secret is
// provisioned, and readiness is verified.
func TestDeclaredIsNotEligible(t *testing.T) {
	db := butlerTestDB(t)
	connectionFixture(t, db)
	if err := db.AddPlacement("calendar", "n1", PlacementPrimary, 0, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.PlaceConnection("calendar", 0); !errors.Is(err, ErrNoEligibleNode) {
		t.Fatalf("an unprepared node took the lease: %v", err)
	}
	// Each of the three facts alone is not enough.
	for _, tc := range [][3]bool{{true, false, false}, {true, true, false}, {false, true, true}} {
		if err := db.MarkPlacementPrepared("calendar", "n1", tc[0], tc[1], tc[2]); err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.PlaceConnection("calendar", 0); !errors.Is(err, ErrNoEligibleNode) {
			t.Fatalf("a partly prepared node (%v) took the lease", tc)
		}
	}
	prepare(t, db, "n1")
	got, moved, err := db.PlaceConnection("calendar", 0)
	if err != nil || !moved || got.Node != "n1" {
		t.Fatalf("PlaceConnection = %+v, %t, %v", got, moved, err)
	}
}

// Failover moves the lease among prepared nodes, one holder at a time,
// and fences the node it left.
func TestFailoverMovesOnlyTheLease(t *testing.T) {
	db := butlerTestDB(t)
	connectionFixture(t, db)
	for _, p := range []struct {
		node, role string
		rank       int
	}{{"n1", PlacementPrimary, 0}, {"n2", PlacementFallback, 1}, {"n3", PlacementFallback, 2}} {
		if err := db.AddPlacement("calendar", p.node, p.role, p.rank, "root-admin"); err != nil {
			t.Fatal(err)
		}
		prepare(t, db, p.node)
	}
	first, _, err := db.PlaceConnection("calendar", 0)
	if err != nil || first.Node != "n1" {
		t.Fatalf("initial placement = %+v, %v", first, err)
	}
	before, _ := db.GetConnection("calendar")

	// The primary goes down: its container stops.
	if err := db.MarkPlacementPrepared("calendar", "n1", false, true, true); err != nil {
		t.Fatal(err)
	}
	moved, changed, err := db.PlaceConnection("calendar", 0)
	if err != nil || !changed {
		t.Fatalf("failover = %+v, %t, %v", moved, changed, err)
	}
	if moved.Node != "n2" {
		t.Fatalf("failover chose %s, want the first prepared fallback", moved.Node)
	}
	// Exactly one holder, always.
	placements, _ := db.ListPlacements("calendar")
	holders := 0
	for _, p := range placements {
		if p.Holder {
			holders++
		}
	}
	if holders != 1 {
		t.Fatalf("%d nodes hold the lease; two adapters would claim the same work", holders)
	}
	// The lease moved, so the old node is fenced: a result it was
	// mid-way through will be refused.
	after, _ := db.GetConnection("calendar")
	if after.LeaseEpoch <= before.LeaseEpoch {
		t.Fatalf("the lease epoch did not advance on failover: %d → %d", before.LeaseEpoch, after.LeaseEpoch)
	}
	// Nothing was created: the eligible set is exactly what was declared
	// and prepared beforehand.
	if len(placements) != 3 {
		t.Fatalf("failover changed the declared set: %+v", placements)
	}
}

// Failback waits for the primary to settle, so a flapping node does not
// drag the lease back and forth.
func TestFailbackWaitsForStabilization(t *testing.T) {
	db := butlerTestDB(t)
	connectionFixture(t, db)
	for _, p := range []struct {
		node, role string
		rank       int
	}{{"n1", PlacementPrimary, 0}, {"n2", PlacementFallback, 1}} {
		if err := db.AddPlacement("calendar", p.node, p.role, p.rank, "root-admin"); err != nil {
			t.Fatal(err)
		}
		prepare(t, db, p.node)
	}
	if _, _, err := db.PlaceConnection("calendar", 0); err != nil {
		t.Fatal(err)
	}
	// n1 falls over, n2 takes it.
	if err := db.MarkPlacementPrepared("calendar", "n1", false, true, true); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := db.PlaceConnection("calendar", 0); got.Node != "n2" {
		t.Fatalf("failover went to %s", got.Node)
	}
	// n1 comes back. With a stabilization window it does not get the
	// lease straight back.
	prepare(t, db, "n1")
	got, changed, err := db.PlaceConnection("calendar", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if changed || got.Node != "n2" {
		t.Fatalf("the lease went back to an unsettled primary: %+v", got)
	}
	// With no window to wait for, it does.
	got, changed, err = db.PlaceConnection("calendar", time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || got.Node != "n1" {
		t.Fatalf("a settled primary did not get the lease back: %+v", got)
	}
}

// When nothing is eligible, dependent work waits. Nothing improvises.
func TestNoEligibleNodeIsAWait(t *testing.T) {
	db := butlerTestDB(t)
	connectionFixture(t, db)
	if err := db.AddPlacement("calendar", "n1", PlacementPrimary, 0, "root-admin"); err != nil {
		t.Fatal(err)
	}
	prepare(t, db, "n1")
	if _, _, err := db.PlaceConnection("calendar", 0); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPlacementPrepared("calendar", "n1", false, false, false); err != nil {
		t.Fatal(err)
	}
	_, _, err := db.PlaceConnection("calendar", 0)
	if !errors.Is(err, ErrNoEligibleNode) {
		t.Fatalf("an outage produced %v, want a wait", err)
	}
	// The declared set is untouched: no node was invented to cover it.
	placements, _ := db.ListPlacements("calendar")
	if len(placements) != 1 || placements[0].Node != "n1" {
		t.Fatalf("an outage changed the declared set: %+v", placements)
	}
}

// Removal fences first and must then be PROVEN. Incomplete cleanup
// leaves the connection visibly degraded.
func TestRemovalIsFencedThenProven(t *testing.T) {
	db := butlerTestDB(t)
	connectionFixture(t, db)
	for _, p := range []struct {
		node, role string
		rank       int
	}{{"n1", PlacementPrimary, 0}, {"n2", PlacementFallback, 1}} {
		if err := db.AddPlacement("calendar", p.node, p.role, p.rank, "root-admin"); err != nil {
			t.Fatal(err)
		}
		prepare(t, db, p.node)
	}
	if _, _, err := db.PlaceConnection("calendar", 0); err != nil {
		t.Fatal(err)
	}
	before, _ := db.GetConnection("calendar")

	if err := db.BeginPlacementRemoval("calendar", "n1"); err != nil {
		t.Fatalf("BeginPlacementRemoval: %v", err)
	}
	// Fenced immediately, before any cleanup has happened.
	after, _ := db.GetConnection("calendar")
	if after.LeaseEpoch <= before.LeaseEpoch {
		t.Fatal("removal did not fence the adapter before cleaning up")
	}
	if after.State != ConnectionDegraded || after.DegradedReason == "" {
		t.Fatalf("a half-removed adapter is not visibly degraded: %+v", after)
	}

	// Partial proof is not proof.
	for _, tc := range [][3]bool{{true, false, false}, {true, true, false}, {false, true, true}} {
		complete, err := db.ProvePlacementRemoval("calendar", "n1", tc[0], tc[1], tc[2])
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			t.Fatalf("removal reported complete with %v proven", tc)
		}
		conn, _ := db.GetConnection("calendar")
		if conn.State != ConnectionDegraded {
			t.Fatalf("an incomplete removal left the connection %q", conn.State)
		}
		if conn.DegradedReason == "" {
			t.Fatal("a degraded connection with no reason is one nobody fixes")
		}
	}

	complete, err := db.ProvePlacementRemoval("calendar", "n1", true, true, true)
	if err != nil || !complete {
		t.Fatalf("full proof = %t, %v", complete, err)
	}
	placements, _ := db.ListPlacements("calendar")
	if len(placements) != 1 || placements[0].Node != "n2" {
		t.Fatalf("after removal: %+v", placements)
	}
	conn, _ := db.GetConnection("calendar")
	if conn.State != ConnectionReady {
		t.Fatalf("a proven removal left the connection %q: %s", conn.State, conn.DegradedReason)
	}
}

// Removing the last node leaves the connection unavailable rather than
// pretending it is fine.
func TestRemovingTheLastNodeLeavesItUnavailable(t *testing.T) {
	db := butlerTestDB(t)
	connectionFixture(t, db)
	if err := db.AddPlacement("calendar", "n1", PlacementPrimary, 0, "root-admin"); err != nil {
		t.Fatal(err)
	}
	prepare(t, db, "n1")
	if _, _, err := db.PlaceConnection("calendar", 0); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginPlacementRemoval("calendar", "n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ProvePlacementRemoval("calendar", "n1", true, true, true); err != nil {
		t.Fatal(err)
	}
	conn, _ := db.GetConnection("calendar")
	if conn.State != ConnectionUnavailable {
		t.Fatalf("a connection with nowhere to run reports %q", conn.State)
	}
}

// A credential's VALUE never reaches Keystone, and a reference that
// looks like one is refused.
func TestSecretsAreReferencesNotValues(t *testing.T) {
	db := butlerTestDB(t)
	connectionFixture(t, db)

	for name, ref := range map[string]string{
		"a URL with a password": "https://sam:hunter2@calendar.example.com/dav",
		"a user:pass pair":      "sam:hunter2@example.com",
		"something with spaces": "sam's app password",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.RecordConnectionSecret(ConnectionSecret{
				Connection: "calendar", Member: "sam", SecretRef: ref,
				UpstreamReadOnly: true, CreatedBy: "sam",
			}); err == nil {
				t.Fatal("a credential-shaped value was accepted as a reference")
			} else if strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("the refusal echoed the credential back: %v", err)
			}
		})
	}

	seq, err := db.RecordConnectionSecret(ConnectionSecret{
		Connection: "calendar", Member: "sam", SecretRef: "calendar-sam-caldav",
		UpstreamReadOnly: true, CreatedBy: "sam",
	})
	if err != nil {
		t.Fatalf("RecordConnectionSecret: %v", err)
	}

	// A credential that is not read-only upstream must disclose what
	// else it can do.
	if _, err := db.RecordConnectionSecret(ConnectionSecret{
		Connection: "calendar", Member: "mika", SecretRef: "calendar-mika-imap",
		CreatedBy: "mika",
	}); err == nil {
		t.Fatal("a credential with wider upstream power was recorded with no disclosure")
	}
	if _, err := db.RecordConnectionSecret(ConnectionSecret{
		Connection: "calendar", Member: "mika", SecretRef: "calendar-mika-imap",
		CreatedBy:  "mika",
		Disclosure: "this provider issues no read-only app password, so this credential can also send mail",
	}); err != nil {
		t.Fatalf("a disclosed credential was refused: %v", err)
	}

	secrets, err := db.ListConnectionSecrets("calendar")
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 2 {
		t.Fatalf("secrets = %+v", secrets)
	}
	for _, s := range secrets {
		if strings.Contains(s.SecretRef, "hunter2") {
			t.Fatalf("a value reached the store: %+v", s)
		}
	}

	// A secret a live placement still uses cannot be erased: doing so
	// produces an adapter failing in a way nobody can diagnose.
	if err := db.AddPlacement("calendar", "n1", PlacementPrimary, 0, "root-admin"); err != nil {
		t.Fatal(err)
	}
	prepare(t, db, "n1")
	if err := db.RevokeConnectionSecret("calendar", seq); !errors.Is(err, ErrSecretReferenced) {
		t.Fatalf("a referenced credential was erased: %v", err)
	}
	// Fencing the placement first is the correct order, and then it can.
	if err := db.BeginPlacementRemoval("calendar", "n1"); err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeConnectionSecret("calendar", seq); err != nil {
		t.Fatalf("revoking after fencing: %v", err)
	}
}
