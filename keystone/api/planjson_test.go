package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// TestPlanViewDropsLegacyPlanJSON pins F25. The blob was meant to be
// dropped — planView declared PlanJSON with `json:"-"` and the comment
// said so — but `json:"-"` does not shadow an embedded field, it removes
// the outer field from consideration, leaving PlanRecord.PlanJSON the
// only candidate for the name. So one response carried the same content
// three times, and the copies disagreed: the escaped copy reported
// basedOnHead as [REDACTED] against the real hash beside it.
func TestPlanViewDropsLegacyPlanJSON(t *testing.T) {
	const head = "015ef04480e32df1a8b5a8e7856f4efedb75146e"
	rec := runtimedb.PlanRecord{
		ID:      "plan-1",
		State:   runtimedb.PlanProposed,
		Summary: "install ingress",
		PlanJSON: `{"basedOnHead":"` + head +
			`","summary":"install ingress","put":["kind: Volume\n"]}`,
	}
	data, err := json.Marshal(viewPlan(rec))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)

	if strings.Contains(body, "planJson") {
		t.Errorf("legacy planJson is still served:\n%s", body)
	}
	// The structured fields it was duplicating must still be there —
	// dropping the blob must not drop the content.
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["basedOnHead"] != head {
		t.Errorf("basedOnHead = %v, want %s", got["basedOnHead"], head)
	}
	if _, ok := got["put"]; !ok {
		t.Errorf("put missing from the view:\n%s", body)
	}
}
