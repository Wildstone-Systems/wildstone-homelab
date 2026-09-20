package butlerstore

// The effective-authority resolver, bound to the runtime database
// (ADR-W030 A7/A8, P7; design §10).
//
// butler defines the seam and holds no database; this implements it.
// The ordering that makes the guarantees true lives in the store's
// resolver, not here — this is the adapter that lets the Butler ask.
//
// What this file DOES decide is layer 1 for a person's request. Design
// §10 stacks three layers in order and a later one never widens an
// earlier one, so the Person check sits ABOVE the Agent's assignments
// and grants rather than beside them: it runs first, on its own, and an
// Agent's reach can only narrow what a Person is already allowed. That
// is §18.6's rule in code — an Agent holding a grant does not authorize
// the person asking.

import (
	"errors"
	"fmt"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Authority implements butler.ToolAuthorizer.
//
// Butler names the specialist this session is running as (ADR-W031
// A4/A9). When it is set, the Butler's own narrowings are consulted
// first and the CLASS comes from the Butler's record rather than from
// the query — so a session physically cannot ask about a wider ceiling
// than the Butler it belongs to. The Butler's own row id is set there
// too, which is what scopes a grant to this Agent rather than to every
// Agent of its class.
//
// It is a field on the resolver rather than a member of the query for
// the same reason. One Authority is built per session, bound to one
// Butler; there is no call shape in which a running session could name
// a different one, which is a structural answer rather than a check
// somebody has to remember to write.
//
// Empty means no Butler scope — the pre-W031 class-level resolution,
// which is what a deployment with no Butlers still runs. Such a session
// reaches only the class-level grants of the pre-0058 shape, never
// another Agent's.
type Authority struct {
	DB     *runtimedb.DB
	Butler string

	// PersonAuthority is layer 1: may this person have work done for
	// them at all, right now. A nil error is yes.
	//
	// It is a seam for the reason Ledger.OwnerActive is one — a
	// deployment mid-migration, or a test, supplies its own without
	// this package deciding what admission means — but it fails the
	// OPPOSITE way, and the difference is deliberate. An unwired
	// OwnerActive lets durable work continue, because a task that was
	// already admitted is not made unsafe by an admission flow that has
	// not run. An unwired PersonAuthority refuses a person's tool call,
	// because "nobody can say whether they may" is the situation §10.1
	// answers with no.
	//
	// PersonIsAdmitted below is the wiring, and calling it is the act.
	PersonAuthority func(person string) error

	// Resources is layer 2's per-kind rules. An empty set admits no
	// resource, which is the correct answer for a resolver that has
	// been given no resource rules: the general rule requires them
	// without knowing them.
	Resources butler.ResourceRules
}

// AuthorizeTool resolves one dispatch.
func (a *Authority) AuthorizeTool(q butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	// Layer 1 first, and on its own. Everything below this point is
	// about the AGENT — its narrowings, its assignments, the grants
	// issued to it — and none of it says anything about the person who
	// is asking (§18.6). Folding the two together is exactly how an
	// Agent's grant becomes a person's permission.
	if q.Caller == butler.CallerPersonFacing {
		if a.PersonAuthority == nil {
			return butler.ToolDecision{
				Allowed: false,
				Reason: fmt.Sprintf(
					"nothing on this deployment can say whether %s may use %s, so it was not run",
					q.Person, q.Tool),
			}, nil
		}
		if err := a.PersonAuthority(q.Person); err != nil {
			return butler.ToolDecision{
				Allowed: false,
				Reason:  fmt.Sprintf("%s, so %s was not run", err, q.Tool),
			}, nil
		}
	}

	query := runtimedb.ToolQuery{
		AgentClass:    q.AgentClass,
		Tool:          q.Tool,
		Structural:    q.Structural,
		GrantRequired: q.GrantRequired,
		TaskID:        q.TaskID,
		RoutineName:   q.RoutineName,
	}
	var (
		res runtimedb.ToolAuthority
		err error
	)
	if a.Butler != "" {
		res, err = a.DB.ResolveButlerTool(a.Butler, query)
	} else {
		res, err = a.DB.ResolveToolAuthority(query)
	}
	if err != nil {
		return butler.ToolDecision{}, err
	}
	return butler.ToolDecision{
		Allowed:     res.Allowed,
		Reason:      res.Reason,
		Requestable: res.Requestable,
		GrantSeq:    res.GrantSeq,
	}, nil
}

// AdmitResource implements butler.ResourceAuthorizer: layer 2, one rule
// per resource kind and no general answer for a kind that has none.
func (a *Authority) AdmitResource(q butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	return a.Resources.AdmitResource(q)
}

// PersonIsAdmitted wires PersonAuthority to the Butler admission
// records, the same records Ledger.AdmissionCheck reads.
//
// Group membership is deliberately NOT consulted here. §10's layer 1
// admits a person, including through the groups they are in; it does
// not decide which tool they get, and a group that granted tools would
// be layer 2 wearing layer 1's clothes. Belonging admits; the Agent's
// assignments below still decide.
func (a *Authority) PersonIsAdmitted() func(string) error {
	return func(person string) error {
		if person == "" {
			// Cannot happen through the seam, which refuses an empty
			// Person before reaching a resolver. Repeated here because
			// a resolver is a public type somebody else may call, and
			// "" matching no row would otherwise read as a lookup
			// failure rather than as missing identity.
			return errors.New("this session does not say who is asking")
		}
		m, err := a.DB.GetButlerMember(person)
		if errors.Is(err, runtimedb.ErrNotFound) {
			return fmt.Errorf("%s has no Butler admission record", person)
		}
		if err != nil {
			return err
		}
		if !m.Active() {
			return fmt.Errorf("%s is no longer admitted to the Butler", person)
		}
		return nil
	}
}

var (
	_ butler.ToolAuthorizer     = (*Authority)(nil)
	_ butler.ResourceAuthorizer = (*Authority)(nil)
)
