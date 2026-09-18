package api

// Receipts: what the assistant did, as a member can read it.

import (
	"encoding/json"
	"testing"
)

// A refusal, a failure and a success are three different rows, and the
// refusal is the one the design says to read first — it is the ceiling
// made visible.
func TestReceiptsTellRefusalFailureAndSuccessApart(t *testing.T) {
	raw, err := json.Marshal(storedTranscript{
		Provider: "ollama",
		Calls: []transcriptRecord{
			{Name: "pantry.read_list", Result: "milk, bread"},
			{Name: "infra.restart_container", Rejected: true,
				Error: "That needs the owner — it isn't something I can do from the dashboard."},
			{Name: "home.lights", Error: "the bridge did not answer"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := receiptsFrom(string(raw))
	if len(got) != 3 {
		t.Fatalf("got %d receipts, want 3", len(got))
	}

	if got[0].Refused || got[0].Failed || got[0].Result == "" {
		t.Errorf("a successful call rendered as %+v", got[0])
	}
	// Refused before it ran: not a failure, and the reason names the
	// rule rather than an error code.
	if !got[1].Refused || got[1].Failed {
		t.Errorf("a refusal rendered as %+v", got[1])
	}
	if got[1].Why == "" {
		t.Error("a refusal with no reason")
	}
	// Ran and errored: neither a refusal nor a success, and must not be
	// rendered as either.
	if !got[2].Failed || got[2].Refused {
		t.Errorf("a failure rendered as %+v", got[2])
	}
}

// A refusal that carried no reason still says it was refused, rather
// than rendering as a blank row a member cannot interpret.
func TestARefusalWithoutAReasonStillReadsAsRefused(t *testing.T) {
	raw, _ := json.Marshal(storedTranscript{Calls: []transcriptRecord{{Name: "packages.install", Rejected: true}}})
	got := receiptsFrom(string(raw))
	if len(got) != 1 || !got[0].Refused {
		t.Fatalf("got %+v", got)
	}
	if got[0].Why == "" {
		t.Error("a refusal with no reason renders as a blank row")
	}
}

// An unparseable transcript yields NO receipts rather than a partial
// list. The panel promises a member can see what was done in their
// name, and a list that quietly dropped one action would break that
// promise while looking complete.
func TestAnUnreadableTranscriptYieldsNoReceiptsRatherThanSome(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json", `{"calls":`, `{"calls":[{"name":`} {
		if got := receiptsFrom(raw); got != nil {
			t.Errorf("receiptsFrom(%q) = %+v, want nil", raw, got)
		}
	}
}

// A conversation with no tool calls has no receipts — and that is a
// real answer, not a missing one.
func TestAConversationThatOnlyAnsweredHasNoReceipts(t *testing.T) {
	raw, _ := json.Marshal(storedTranscript{Provider: "ollama"})
	if got := receiptsFrom(string(raw)); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

// A tool that returns a page of text is bounded to a line: a receipt is
// a receipt, not a transcript.
func TestALongResultIsBoundedToALine(t *testing.T) {
	long := make([]byte, 4000)
	for i := range long {
		long[i] = 'x'
	}
	raw, _ := json.Marshal(storedTranscript{
		Calls: []transcriptRecord{{Name: "pantry.read_list", Result: string(long)}},
	})
	got := receiptsFrom(string(raw))
	if len(got) != 1 {
		t.Fatal("no receipt")
	}
	if len([]rune(got[0].Result)) > maxReceiptResult+1 {
		t.Errorf("result is %d runes, want it bounded to %d", len([]rune(got[0].Result)), maxReceiptResult)
	}
}
