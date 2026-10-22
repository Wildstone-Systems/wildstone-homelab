package runtimedb

// H7's authority evidence (#353, ADR-W039 §5, ADR-W033 §6.2/§6.4):
// the host.* family is opt-in, so a governed Owner Agent does not reach
// it until it is named to that Agent by an explicit assignment — the
// same machinery TestAnOptInToolCanStillBeAssignedByName already proves
// for an arbitrary opt-in tool, exercised here against host.*'s own
// name so the family's own wiring (keystone/domestic.HostOpsAgentTools,
// keystone/butler's toolEntry.OptIn) is not the only place this claim is
// made.

import "testing"

// TestHostContainerLogsIsOptInAndNeedsExplicitAssignment is deliverable
// (c)'s "Owner Agent without the opt-in grant is denied": alfred is a
// governed Owner Agent, host.container_logs is structurally present
// (Owner-class ToolGateway registers it), and yet it is refused until
// somebody names it to him by assignment — never absorbed by a bundle,
// whatever service/class a caller supplies.
func TestHostContainerLogsIsOptInAndNeedsExplicitAssignment(t *testing.T) {
	db, alfred := governedFixture(t)

	got := ask(t, db, ServiceSystem, AgentClassOwner, "host.container_logs", true)
	if got.Allowed {
		t.Fatal("host.container_logs was allowed with no assignment at all")
	}
	if got.Source != "assignment" {
		t.Fatalf("Source = %q, want %q (denied by the assignment rule, not a bundle)", got.Source, "assignment")
	}
	if !got.Requestable {
		t.Fatal("an opt-in tool with no assignment must be Requestable — asking is not having")
	}

	// A bundle for host.* Owner tools still could not reach it: opt-in
	// means named, never absorbed (§6.2's acceptance negative).
	if err := db.GrantToolBundle(alfred, ServiceSystem, AgentClassOwner, "mika"); err != nil {
		t.Fatalf("GrantToolBundle: %v", err)
	}
	stillDenied := ask(t, db, ServiceSystem, AgentClassOwner, "host.container_logs", true)
	if stillDenied.Allowed {
		t.Fatal("a service bundle absorbed an opt-in host.* tool — §6.2 is broken")
	}

	// Named explicitly: now, and only now, it is reachable.
	if err := db.AssignTool(alfred, "host.container_logs", "mika"); err != nil {
		t.Fatalf("AssignTool: %v", err)
	}
	allowed := ask(t, db, ServiceSystem, AgentClassOwner, "host.container_logs", true)
	if !allowed.Allowed {
		t.Fatalf("host.container_logs still refused after an explicit assignment: %+v", allowed)
	}
}

// TestHostOpsMemberAgentNeverReachesIt is (b)'s runtimedb-layer half: a
// Member-class Agent's own class does not admit an Owner-class grant at
// all (§6.1) — the reason a Member Agent could never be handed host.*
// even by an Owner who tried. The manifest-level half (a Member surface
// never even REGISTERING host.*) is
// TestHostOpsNeverAppearInAMemberManifest in keystone/butler.
func TestHostOpsMemberAgentNeverReachesIt(t *testing.T) {
	db, alfred := governedFixture(t)
	if err := db.AssignTool(alfred, "host.container_logs", "mika"); err != nil {
		t.Fatalf("AssignTool: %v", err)
	}
	hobson, err := db.GetButler("hobson")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.GovernAgent(hobson.Seq, "mika"); err != nil {
		t.Fatalf("GovernAgent: %v", err)
	}
	got, err := db.ResolveButlerTool("hobson", ToolQuery{
		Tool: "host.container_logs", Structural: false, // a Member gateway never registers it at all
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Fatal("a Member Agent reached host.container_logs — it must be structurally absent")
	}
	if got.Source != "structural" {
		t.Fatalf("Source = %q, want %q", got.Source, "structural")
	}
}
