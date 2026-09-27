package api

// #364: memory is bound to a member when a session is built, so the base
// household seams carry none. The views that seed, list and check an
// Agent's tools were built from those seams and reported recall and
// remember as absent: no governed Agent could ever use memory.

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestANewAgentIsSeededWithMemory(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")
	b, err := h.s.DB.GetButler("health")
	if err != nil {
		t.Fatal(err)
	}
	assigned, err := h.s.DB.ListToolAssignments(b.Seq)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, a := range assigned {
		have[a.Tool] = true
	}
	for _, tool := range []string{"recall", "remember"} {
		if !have[tool] {
			t.Errorf("a new Agent was not seeded with %s; assigned: %v", tool, assigned)
		}
	}
}

func TestMemoryToolsExistOnBothAgentClasses(t *testing.T) {
	h := butlerConsole(t)
	for _, class := range []string{runtimedb.AgentClassMember, runtimedb.AgentClassOwner} {
		for _, tool := range []string{"recall", "remember"} {
			if !h.s.toolExistsOnClass(class, tool) {
				t.Errorf("%s does not exist on a %s Agent, so it can never be assigned", tool, class)
			}
		}
	}
}
