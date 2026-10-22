package portal

import "testing"

// The filters partition the set: every verb this surface speaks belongs
// to exactly one subject filter, and none belongs to two.
//
// This is the test that keeps the counts beside the tabs honest. A verb
// in two families would be counted twice; a verb in none would be a row
// a member can only reach by turning every filter off.
func TestTheSubjectFiltersPartitionEveryVerb(t *testing.T) {
	subjects := []Filter{FilterHome, FilterFiles, FilterAssistant}
	for verb := range lines {
		var in []Filter
		for _, f := range subjects {
			if Matches(f, verb, ToneOK) {
				in = append(in, f)
			}
		}
		switch len(in) {
		case 1:
			// Exactly right.
		case 0:
			t.Errorf("%q is in no subject filter; it would be unreachable from the tabs", verb)
		default:
			t.Errorf("%q is in %v; the counts beside the tabs would double-count it", verb, in)
		}
	}
}

// Everything means everything, and the decision filter cuts by tone
// rather than by subject.
func TestEverythingAndWaitingCutDifferently(t *testing.T) {
	for verb := range lines {
		if !Matches(FilterEverything, verb, ToneOK) {
			t.Errorf("%q is missing from Everything", verb)
		}
	}
	// A waiting row is a waiting row whatever it is about.
	for _, verb := range []string{"automation.escalated", "filedrop.uploaded", "person.arrived_home"} {
		if !Matches(FilterWaiting, verb, ToneWaiting) {
			t.Errorf("%q with a waiting tone is not under the decision filter", verb)
		}
		if Matches(FilterWaiting, verb, ToneOK) {
			t.Errorf("%q with an ok tone is under the decision filter", verb)
		}
	}
}

// A verb this surface has no sentence for has no subject either: both
// drop it, and for the same reason.
func TestAnUnmappedVerbHasNoSubject(t *testing.T) {
	if _, ok := Subject("reconcile.applied"); ok {
		t.Error("an operator verb was filed under a household filter")
	}
	for _, f := range []Filter{FilterHome, FilterFiles, FilterAssistant} {
		if Matches(f, "reconcile.applied", ToneOK) {
			t.Errorf("reconcile.applied matched %s", f)
		}
	}
}
