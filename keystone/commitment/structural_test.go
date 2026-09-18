package commitment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// TestPackageImportsOnlyTheStandardLibrary pins what the package doc
// claims: this is a leaf, and the due-ness function depends on nothing
// that could bring storage, a clock, a network or a model with it.
//
// The claim is worth a guard rather than a comment because the pressure
// to break it is real and arrives sideways — one `keystone/resource`
// import for a shared constant, and a pure function starts carrying the
// Statefile's dependency graph. This file may import it; the package may
// not, which is the whole distinction.
func TestPackageImportsOnlyTheStandardLibrary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "wildstone-homelab/") {
			t.Errorf("%s imports another Wildstone package; this one is a leaf on the standard library (ADR-W034 K3)", name)
		}
	}
}

// TestTheClockMeterIsNamedTheSameInBothPackages pins the duplicated
// constant, the way keystone/household pins its event-type pattern
// against keystone/pkgspec.
//
// resource.KindMeter refuses a declaration called "clock" so that the
// built-in calendar meter cannot be shadowed; this package treats that
// same name as the built-in. If the two drifted, a household could
// declare a meter the validator allowed and the engine then quietly
// treated as the calendar — which is the most confusing possible
// failure, because every date it produced would look plausible.
func TestTheClockMeterIsNamedTheSameInBothPackages(t *testing.T) {
	if MeterClock != resource.MeterClock {
		t.Errorf("commitment.MeterClock = %q, resource.MeterClock = %q: a declared meter could shadow the calendar",
			MeterClock, resource.MeterClock)
	}
}

// TestTheIntervalGrammarsAgree pins keystone/resource's interval
// validator against this package's parser.
//
// They are separate implementations of one grammar because resource is
// imported everywhere and this package is a leaf, so neither can hold the
// other's copy. That is a duplication with a specific failure mode worth
// testing rather than trusting: a validator that ACCEPTS what the engine
// rejects lets a household approve a plan that can never run, and one
// that REFUSES what the engine takes makes a legal commitment
// unwritable. Both directions are checked, over the same table.
func TestTheIntervalGrammarsAgree(t *testing.T) {
	for _, in := range []string{
		// counts
		"5000", "300", "0.5", "1",
		// spans
		"90d", "2w", "36h", "30m", "45s",
		// calendar steps
		"6mo", "1yr", "2y", "18mo",
		// and the ways people get it wrong
		"", "   ", "abc", "0", "-5", "1.5mo", "1day", "2wk", "mo", "5000x",
		"12,000", "6 mo", "1yr2mo", "yr", "0d", "0mo",
	} {
		_, parseErr := ParseInterval(in)
		engineTakesIt := parseErr == nil
		validatorTakesIt := resource.CommitmentIntervalValid(in)
		if engineTakesIt != validatorTakesIt {
			t.Errorf("%q: the engine %s it and the validator %s it",
				in, took(engineTakesIt), took(validatorTakesIt))
		}
	}
}

func took(ok bool) string {
	if ok {
		return "takes"
	}
	return "refuses"
}

// TestTheRolloverModesAgree pins the other duplicated vocabulary. A
// commitment whose rollover validates and then does not evaluate is a
// plan a household approved and the engine cannot run.
func TestTheRolloverModesAgree(t *testing.T) {
	engine := map[string]bool{
		string(Once): true, string(FromCompletion): true, string(FromSchedule): true,
	}
	declared := map[string]bool{}
	for _, r := range resource.CommitmentRollovers() {
		declared[r] = true
	}
	if len(engine) != len(declared) {
		t.Fatalf("engine has %d modes, resource offers %d", len(engine), len(declared))
	}
	for mode := range engine {
		if !declared[mode] {
			t.Errorf("the engine runs %q and resource does not offer it", mode)
		}
	}
}
