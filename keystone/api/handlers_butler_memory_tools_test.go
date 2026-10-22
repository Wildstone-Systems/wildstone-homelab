package api

// #364: memory is bound to a member when a session is built, so the base
// household seams carry none. The views that seed, list and check an
// Agent's tools were built from those seams and reported recall and
// remember as absent: no governed Agent could ever use memory.

import (
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
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

// An Agent made before the fix, governed with no memory tools, is given
// them once; one whose Owner took memory away is left alone.
func TestBackfillGivesMemoryToolsOnlyWhereNeverAssigned(t *testing.T) {
	h := butlerConsole(t)
	// health: the pre-fix state — governed, seeded, but never with memory.
	seq, err := h.s.DB.CreateButler(runtimedb.Butler{
		Name: "health", Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "kitchen-box",
	}, "mika")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AssignTool(seq, "get_household_events", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.GovernAgent(seq, "mika"); err != nil {
		t.Fatal(err)
	}
	health, err := h.s.DB.GetButlerBySeq(seq)
	if err != nil {
		t.Fatal(err)
	}
	createAgent(t, h, "garden")
	garden, err := h.s.DB.GetButler("garden")
	if err != nil {
		t.Fatal(err)
	}
	// An Owner's removal on garden.
	if err := h.s.DB.UnassignTool(garden.Seq, "remember", "mika"); err != nil {
		t.Fatal(err)
	}

	for range 2 { // idempotent
		if err := h.s.BackfillMemoryTools(); err != nil {
			t.Fatal(err)
		}
	}
	active := func(seq int64) map[string]int {
		out := map[string]int{}
		assigned, err := h.s.DB.ListToolAssignments(seq)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range assigned {
			out[a.Tool]++
		}
		return out
	}
	if got := active(health.Seq); got["recall"] != 1 || got["remember"] != 1 {
		t.Errorf("health's memory assignments = %v, want recall and remember once each", got)
	}
	if got := active(garden.Seq); got["remember"] != 0 || got["recall"] != 1 {
		t.Errorf("garden's memory assignments = %v, want remember left removed and recall kept", got)
	}
}
