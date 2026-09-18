package butler

import (
	"context"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

type fakeStandings struct {
	lines []CommitmentLine
	err   error
}

func (f fakeStandings) Describe() ([]CommitmentLine, error) { return f.lines, f.err }

// The ceiling, stated as a test rather than only as a comment: an agent
// drafts and reads, and there is no tool here that closes a cycle,
// records a number or moves a threshold.
func TestAnAgentHasNoToolThatAnswersACommitment(t *testing.T) {
	gw := NewToolGateway(newTestStore(t), nil)
	for _, spec := range gw.ToolSpecs() {
		name := spec.Name
		for _, forbidden := range []string{
			"complete_commitment", "satisfy_commitment", "waive_commitment",
			"record_reading", "book_commitment", "record_meter",
		} {
			if name == forbidden {
				t.Errorf("the gateway offers %q; those are writes by a person or a plan (ADR-W034 A13)", name)
			}
		}
	}
}

func TestTheSchemaToolNamesThisDeploymentsMeters(t *testing.T) {
	store := newTestStore(t)
	if err := store.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: "f150-odometer", Owner: "household"},
		Meter:    &resource.MeterSpec{Description: "Odometer on the F-150", Unit: "miles"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	gw := NewToolGateway(store, nil)
	out, err := handleGetCommitmentSchema(context.Background(), gw, nil)
	if err != nil {
		t.Fatalf("handleGetCommitmentSchema: %v", err)
	}
	if !strings.Contains(out.Content, "f150-odometer (miles)") {
		t.Errorf("the schema does not name the deployment's own meter:\n%s", out.Content)
	}
	// The closed vocabularies come from the validator, so a model cannot
	// be told about a word validation will refuse.
	for _, mode := range resource.CommitmentRollovers() {
		if !strings.Contains(out.Content, mode) {
			t.Errorf("rollover %q is missing from the schema", mode)
		}
	}
	if !strings.Contains(out.Content, "clock — the calendar") {
		t.Errorf("the built-in meter is not described")
	}
	// The three-fields distinction is the one a model most needs told.
	if !strings.Contains(out.Content, "answered by a parent") {
		t.Errorf("the schema does not explain owner vs mayComplete vs notify")
	}
}

func TestTheSchemaToolSaysWhenThereAreNoCounterMeters(t *testing.T) {
	gw := NewToolGateway(newTestStore(t), nil)
	out, err := handleGetCommitmentSchema(context.Background(), gw, nil)
	if err != nil {
		t.Fatalf("handleGetCommitmentSchema: %v", err)
	}
	if !strings.Contains(out.Content, "needs its Meter proposed first") {
		t.Errorf("a deployment with no meters does not say so:\n%s", out.Content)
	}
}

func TestGetCommitmentsReportsStandingsAndExplainsCantTell(t *testing.T) {
	gw := NewToolGateway(newTestStore(t), nil)
	gw.Commitments = fakeStandings{lines: []CommitmentLine{
		{Name: "kai-checkup", State: "overdue", Reason: "past 10 Sep 2026"},
		{Name: "f150-oil", State: "can't tell", Reason: "the odometer needs reading"},
	}}
	out, err := handleGetCommitments(context.Background(), gw, nil)
	if err != nil {
		t.Fatalf("handleGetCommitments: %v", err)
	}
	if !strings.Contains(out.Content, "kai-checkup") || !strings.Contains(out.Content, "f150-oil") {
		t.Errorf("standings missing:\n%s", out.Content)
	}
	// The one thing a model must not do with this is reassure somebody.
	if !strings.Contains(out.Content, "not the same as") {
		t.Errorf("the tool does not warn that can't-tell is not not-due:\n%s", out.Content)
	}
}

func TestGetCommitmentsWithNothingWired(t *testing.T) {
	gw := NewToolGateway(newTestStore(t), nil)
	out, err := handleGetCommitments(context.Background(), gw, nil)
	if err != nil {
		t.Fatalf("handleGetCommitments: %v", err)
	}
	if !strings.Contains(out.Content, "not tracking") {
		t.Errorf("content = %q", out.Content)
	}
}
