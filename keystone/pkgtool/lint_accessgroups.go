package pkgtool

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
)

// accessGroupIssues checks the groups a package says it is for against
// the closed household vocabulary (ADR-W033 design §4, slice 8).
//
// Vocabulary rather than structure, so it lives here and not in
// pkgspec.Validate: a manifest naming a group this binary has not heard
// of must still LOAD (ADR-W025), or a newer catalog becomes unreadable
// by an older Keystone. What it must not do is INSTALL quietly, and this
// is where that is stopped.
//
// The failure it exists for is specific. An entitlement defaults to
// deny, so a package that meant `adults` and wrote `Adults` declares an
// audience nobody is in — and the household sees an app that refuses
// everyone, which reads as a broken install rather than as a typo. That
// is the same trap `classifications: [admin]` was (F31), in a field that
// is load-bearing from its first day.
func accessGroupIssues(pkg *pkgspec.Package) []Issue {
	var issues []Issue
	for _, g := range pkg.AccessGroups {
		if pkgspec.AccessGroups[g] {
			continue
		}
		msg := fmt.Sprintf("accessGroups: %q is not a household group (one of: %s)",
			g, strings.Join(pkgspec.AccessGroupNames(), ", "))
		// The likeliest way to get here is capitalisation: the household
		// calls the group `Adults` and the seed key is `adults`, and the
		// two are different strings in a field where the difference
		// means "nobody".
		if lower := strings.ToLower(strings.TrimSpace(g)); pkgspec.AccessGroups[lower] {
			msg += fmt.Sprintf(" — did you mean %q? The display name is the household's and may change; "+
				"the seed key names the role and does not follow it", lower)
		}
		issues = append(issues, Issue{Rule: "access-group", Msg: msg})
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Msg < issues[j].Msg })
	return issues
}
