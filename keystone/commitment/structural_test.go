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
