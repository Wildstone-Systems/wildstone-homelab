package butler

// Test support for the authority seam (design §10.1).
//
// Every test below this line builds a gateway the way a real caller
// does: a session is told WHO is asking, over WHICH Connection, and
// WHERE authority is resolved, and an inventory says it is an
// inventory. That is not test scaffolding around an inconvenience — it
// is the slice's whole claim. A gateway that was never told any of it
// refuses every tool, and these helpers exist so each test states what
// kind of caller it is rather than every test restating the same three
// lines.

// testConnection is the Connection a test session arrives over. §10.1
// fails closed without one, so a test that opened a session with no
// Connection would be testing the refusal rather than its subject.
import "github.com/chickenrat17/wildstone-homelab/keystone/statefile"

const testConnection = "test_connection"

// testPerson is who is asking in a test that does not otherwise care.
const testPerson = "tester"

// permitAll is the authorizer a test wires when its subject is not
// authority itself: it answers exactly what structural presence already
// said, which is what the seam did for everybody before this slice.
//
// It is NOT a stand-in for "no authorizer". The difference is the
// slice: no authorizer on a person-facing path is a refusal, and a
// resolver that permits what the gateway registers is a deployment with
// no controls configured. Tests that mean the first pass nil.
type permitAll struct{}

func (permitAll) AuthorizeTool(q ToolAuthorityQuery) (ToolDecision, error) {
	return ToolDecision{Allowed: q.Structural}, nil
}

// newTestToolGateway builds an operator gateway as a session opens one.
func newTestToolGateway(store *statefile.Store, reader *AgentReader) *ToolGateway {
	gw := NewToolGateway(store, reader)
	gw.SetAuthorizer(permitAll{})
	gw.SetCaller(CallerPersonFacing, testPerson, testConnection)
	return gw
}

// newTestHouseholdGateway builds a family gateway as a session opens
// one — or, for a gateway bound to no member, as the inventory
// endpoints build theirs. Nobody is asking in the second case, so there
// is no Person to check and the administrative path is the honest one.
func newTestHouseholdGateway(member string, seams HouseholdSeams) *HouseholdGateway {
	return asTestSession(NewHouseholdGateway(member, seams), member)
}

// newTestHouseholdGroupGateway is the same for a room's gateway.
func newTestHouseholdGroupGateway(member string, seams HouseholdSeams) *HouseholdGateway {
	return asTestSession(NewHouseholdGroupGateway(member, seams), member)
}

func asTestSession(gw *HouseholdGateway, member string) *HouseholdGateway {
	if member == "" {
		return gw.ForAdministration()
	}
	gw.SetAuthorizer(permitAll{})
	gw.SetCaller(CallerPersonFacing, member, testConnection)
	return gw
}
