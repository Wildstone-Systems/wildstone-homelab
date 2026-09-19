package butler

// The default audience this package writes must be one the store knows.

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// butler does not import runtimedb, on purpose, so the strictest
// audience is declared in both places. This is the seam that keeps them
// equal — a test may depend on what the package it tests may not.
//
// Without it the drift is silent at compile time and loud at runtime,
// in a store error naming a value nobody can find the source of.
func TestTheDefaultAudienceMatchesTheStore(t *testing.T) {
	if strictestAudience != string(runtimedb.ButlerAudiencePrivate) {
		t.Errorf("butler writes %q as its default audience, the store calls it %q; "+
			"a run that names no audience would write a value the schema does not know",
			strictestAudience, runtimedb.ButlerAudiencePrivate)
	}
}
