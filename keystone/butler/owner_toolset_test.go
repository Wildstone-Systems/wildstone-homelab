package butler

import (
	"context"
	"strings"
	"testing"
)

// ownerSession is an Owner Agent's session: the operator's tools and the
// household's, bound to one person and one authorizer.
func ownerSession(t *testing.T, auth ToolAuthorizer) (*OwnerToolset, *[]HouseholdActionCall) {
	t.Helper()
	seams, dispatched := householdSeams(t)
	o := NewOwnerToolset(newTestToolGateway(nil, nil), NewHouseholdGateway("mika", seams))
	o.SetAuthorizer(auth)
	o.SetCaller(CallerPersonFacing, "mika", "console")
	return o, dispatched
}

func offered(o Toolset, readOnly bool) map[string]bool {
	out := map[string]bool{}
	for _, s := range o.OfferedSpecs(readOnly) {
		out[s.Name] = true
	}
	return out
}

// An Owner Agent is offered the operator's tools AND the household's
// (ADR-W033 B3): asked to add milk to the shopping list, it has
// do_household_action to do it with.
func TestAnOwnerAgentHoldsBothToolSets(t *testing.T) {
	o, dispatched := ownerSession(t, permitAll{})
	got := offered(o, false)
	for _, want := range []string{"get_system_overview", "propose_change", "do_household_action", "list_household_actions", "get_household_events", "describe_capabilities"} {
		if !got[want] {
			t.Errorf("an Owner Agent is not offered %s", want)
		}
	}
	names := 0
	for _, s := range o.OfferedSpecs(false) {
		if s.Name == "describe_capabilities" {
			names++
		}
	}
	if names != 1 {
		t.Fatalf("describe_capabilities offered %d times; both gateways register it and it must appear once", names)
	}

	// A household action goes to the household gateway, as the person
	// being answered.
	out, err := o.Execute(context.Background(), "do_household_action", map[string]any{
		"tool": "notifications.notify", "params": map[string]any{"message": "hello"},
	}, false)
	if err != nil || out.Rejected {
		t.Fatalf("household action from an Owner Agent = %+v %v", out, err)
	}
	if len(*dispatched) != 1 || (*dispatched)[0].Member != "mika" || (*dispatched)[0].Tool != "notifications.notify" {
		t.Fatalf("dispatched = %+v", *dispatched)
	}

	// The household's ceiling still holds inside an Owner Agent: an
	// infrastructure-tier action is not a household action.
	out, _ = o.Execute(context.Background(), "do_household_action", map[string]any{
		"tool": "infra.restart_container", "params": map[string]any{},
	}, false)
	if !out.Rejected || len(*dispatched) != 1 {
		t.Fatalf("an above-domestic action went through an Owner Agent's household tools: %+v", out)
	}
}

// describe_capabilities answers for both tool sets, not the one that
// happened to register the name first.
func TestAnOwnerAgentDescribesBothToolSets(t *testing.T) {
	o, _ := ownerSession(t, permitAll{})
	out, err := o.Execute(context.Background(), "describe_capabilities", nil, false)
	if err != nil || out.Rejected {
		t.Fatalf("describe = %+v %v", out, err)
	}
	for _, want := range []string{"get_system_overview", "do_household_action"} {
		if !strings.Contains(out.Content, want) {
			t.Errorf("describe_capabilities does not mention %s:\n%s", want, out.Content)
		}
	}
	m := o.Capabilities(false)
	if m.AgentClass != AgentClassOwner {
		t.Fatalf("merged manifest class = %q", m.AgentClass)
	}
	seen := map[string]int{}
	for _, tool := range m.Tools {
		seen[tool.Name]++
	}
	if seen["describe_capabilities"] != 1 || seen["do_household_action"] != 1 || seen["get_system_overview"] != 1 {
		t.Fatalf("merged tools = %v", seen)
	}
}

// One authorizer governs both halves: turning a household tool off for
// this Agent refuses it, the same as turning off an operator tool.
func TestAnOwnerAgentsGrantsReachItsHouseholdTools(t *testing.T) {
	o, dispatched := ownerSession(t, refuseOne{tool: "do_household_action"})
	if ok, _ := o.Permits("do_household_action", false); ok {
		t.Fatal("a household tool this Agent was narrowed out of is permitted")
	}
	out, _ := o.Execute(context.Background(), "do_household_action", map[string]any{
		"tool": "notifications.notify", "params": map[string]any{"message": "hello"},
	}, false)
	if !out.Rejected || len(*dispatched) != 0 {
		t.Fatalf("a narrowed household tool ran: %+v", out)
	}
	if ok, _ := o.Permits("get_system_overview", false); !ok {
		t.Fatal("narrowing a household tool took an operator tool with it")
	}
}

// The prompt tells an Owner Agent it holds the household's tools and that
// a household request is done with them, not with a Plan.
func TestAnOwnerAgentIsToldAboutItsHouseholdTools(t *testing.T) {
	o, _ := ownerSession(t, permitAll{})
	p := o.SystemPrompt()
	if !strings.HasPrefix(p, SystemPrompt) || !strings.Contains(p, OwnerHouseholdPrompt) {
		t.Fatal("the Owner prompt is not the operator prompt plus the household addendum")
	}
	for _, want := range []string{"do_household_action", "list_household_actions", "no containers of its own"} {
		if !strings.Contains(p, want) {
			t.Errorf("the Owner prompt does not mention %q", want)
		}
	}
	if onlyOwner := NewOwnerToolset(newTestToolGateway(nil, nil), nil); onlyOwner.SystemPrompt() != SystemPrompt {
		t.Fatal("an Owner Agent on a deployment with no household surface is told about tools it does not have")
	}
}

// A household Agent is untouched: its gateway has no operator tool, and
// nothing about OwnerToolset gives it one.
func TestAMemberAgentStillHoldsOnlyHouseholdTools(t *testing.T) {
	seams, _ := householdSeams(t)
	member := newTestHouseholdGateway("sam", seams)
	for _, s := range member.OfferedSpecs(false) {
		if s.Name == "get_system_overview" || s.Name == "propose_change" {
			t.Fatalf("a Member Agent is offered the operator tool %s", s.Name)
		}
	}
}
