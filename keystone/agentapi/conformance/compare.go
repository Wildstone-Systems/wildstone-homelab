package conformance

import (
	"encoding/json"
	"testing"
)

// Script is one deterministic sequence of calls against a single
// Target, whose return value Compare marshals and diffs byte for byte
// against the other Target's. It takes the whole Target, not just a
// Client, so a script can reach target.Ctx(), target.Handle(t) and
// target.Deliveries exactly as a hand-written test would — Compare adds
// nothing to what the script does; it only runs the identical script
// twice and compares what came back.
//
// A Script fails the surrounding test itself (t.Fatal/t.Error) for
// anything that is not part of the byte-identical comparison — a
// transport error, a state the two arms disagree about before either
// result is even marshaled — exactly as the checks in this package do.
type Script func(t *testing.T, target Target) any

// Compare runs script once against a and once against b, then asserts
// their two results marshal to byte-identical JSON. This generalises
// what keystone/api/handlers_agent_test.go's own byte-identical tests
// proved by hand through Phase 4 (#337's own evidence bar:
// the in-process and HTTP clients answer byte-identically): the same guarantee, for any two agentapi.Client
// implementations a caller wants to compare, not only Wildstone's own
// two.
//
// The comparison is of the WHOLE marshaled value: if script returns a
// struct with several fields, a mismatch in any one of them fails the
// test with both full JSON blobs printed, which is enough to see which
// field differs without Compare having to know the shape of what it is
// comparing.
func Compare(t *testing.T, a, b Target, script Script) {
	t.Helper()
	ra := script(t, a)
	rb := script(t, b)
	ja, err := json.Marshal(ra)
	if err != nil {
		t.Fatalf("conformance.Compare: marshaling target a's result: %v", err)
	}
	jb, err := json.Marshal(rb)
	if err != nil {
		t.Fatalf("conformance.Compare: marshaling target b's result: %v", err)
	}
	if string(ja) != string(jb) {
		t.Errorf("the two targets are not byte-identical:\n a: %s\n b: %s", ja, jb)
	}
}
