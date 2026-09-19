package pkgspec

// The household groups a package may name (ADR-W033 design §4, slice 8).
//
// Deliberately only the three ROLES slice 6 seeds. A household's own
// groups are referenced by id, because a group a household invented has
// no name a package author could have known — and slice 6 is explicit
// that "nothing in Wildstone should have an opinion about a group it did
// not define". Those assignments are made in the Console, as entitlement
// rows, not in a manifest.
//
// Held here rather than in pkgtool for the same reason ParamShapes is:
// the vocabulary is one list, and lint is what REJECTS a name outside it
// so that a manifest written against a newer list still loads (ADR-W025).
// The runtimedb seed keys are the same three strings, and a test there
// fails if the two ever drift.
const (
	AccessGroupFamily = "family"
	AccessGroupAdults = "adults"
	AccessGroupKids   = "kids"
)

// AccessGroups is the closed set, for lint and for anything that needs to
// say what the choices are.
var AccessGroups = map[string]bool{
	AccessGroupFamily: true,
	AccessGroupAdults: true,
	AccessGroupKids:   true,
}

// AccessGroupNames lists the set in a stable order, for error messages.
func AccessGroupNames() []string {
	return []string{AccessGroupFamily, AccessGroupAdults, AccessGroupKids}
}
