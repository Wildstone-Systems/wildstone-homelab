package api

// Replaying a draft held as a parsed spec (#172).
//
// The Console edits the spec the detail read served it and dry-runs that,
// so it needs a way to replay a draft without writing YAML. The routine
// supplies the envelope; the spec replaces what the rule does.

import (
	"net/http"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestReplayAcceptsASpecAgainstARoutinesEnvelope(t *testing.T) {
	h := newHarness(t)
	seedRoutine(t, h, "evening-mail", "mail-check", runtimedb.RoutineTrial)

	var got replayResponse
	if resp := h.do(runtimedb.RoleOperator, http.MethodPost,
		"/api/v1/household/automations/replay",
		map[string]any{"routine": "evening-mail", "spec": editedSpec(t, "13:00")},
		&got); resp.StatusCode != http.StatusOK {
		t.Fatalf("replay: %s", resp.Status)
	}
	if !got.Draft {
		t.Fatal("a spec replay is a draft replay")
	}
	// The envelope comes from the routine, so the rule keeps its name.
	if got.Rule != "mail-check" {
		t.Fatalf("the draft did not take the routine's rule name: %q", got.Rule)
	}
	// Nothing was stored: this is a read.
	after, err := h.s.DB.GetHouseholdRoutine("evening-mail")
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != 1 {
		t.Fatalf("a replay recorded a revision: %+v", after)
	}
}

func TestReplaySpecRefusals(t *testing.T) {
	h := newHarness(t)
	seedRoutine(t, h, "evening-mail", "mail-check", runtimedb.RoutineTrial)
	spec := editedSpec(t, "13:00")

	for _, c := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"a spec with no routine has no envelope",
			map[string]any{"spec": spec}, http.StatusBadRequest},
		{"a routine with no spec replaces nothing",
			map[string]any{"routine": "evening-mail"}, http.StatusBadRequest},
		{"naming two things at once",
			map[string]any{"name": "mail-check", "routine": "evening-mail", "spec": spec}, http.StatusBadRequest},
		{"a routine nobody has",
			map[string]any{"routine": "no-such-thing", "spec": spec}, http.StatusNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			if resp := h.do(runtimedb.RoleOperator, http.MethodPost,
				"/api/v1/household/automations/replay", c.body, nil); resp.StatusCode != c.want {
				t.Fatalf("want %d, got %s", c.want, resp.Status)
			}
		})
	}
}

// A spec that will not survive being written and read back is not a
// rule, and finding that out here beats replaying something the engine
// would refuse.
func TestReplayRefusesASpecThatIsNotARule(t *testing.T) {
	h := newHarness(t)
	seedRoutine(t, h, "evening-mail", "mail-check", runtimedb.RoutineTrial)
	bad := editedSpec(t, "13:00")
	bad.Action.Tool = "notify" // not <capability>.<tool>

	if resp := h.do(runtimedb.RoleOperator, http.MethodPost,
		"/api/v1/household/automations/replay",
		map[string]any{"routine": "evening-mail", "spec": bad}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an invalid spec was replayed: %s", resp.Status)
	}
}

// The document forms still work exactly as they did.
func TestReplayStillTakesARuleDocument(t *testing.T) {
	h := newHarness(t)
	var got replayResponse
	if resp := h.do(runtimedb.RoleOperator, http.MethodPost,
		"/api/v1/household/automations/replay",
		map[string]any{"rule": routineDoc("mail-check")}, &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("replay: %s", resp.Status)
	}
	if !got.Draft || got.Rule != "mail-check" {
		t.Fatalf("draft replay by document: %+v", got)
	}
}
