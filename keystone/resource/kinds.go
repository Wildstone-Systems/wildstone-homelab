package resource

import "sort"

// ReservedRouteName is the one Route name Wildstone keeps for itself. The
// reconciler writes a placeholder fragment at the path this name renders
// to, so that `import routes/*.caddy` always matches a non-empty file
// (F30). Nothing reserved the name, so a Route could claim it: the Route
// applied and served, then the next container ensure overwrote its
// fragment with the placeholder, the next tick raised a drift plan for
// it, approving that rewrote the fragment, and the next container ensure
// clobbered it again — two mechanisms oscillating over one file (F35).
//
// It lives here, beside the kind vocabulary, because validation must
// refuse it at proposal time and the reconciler must render to it, and a
// second copy of the string is exactly how those two drift apart.
const ReservedRouteName = "00-wildstone-placeholder"

// Kinds returns the closed kind vocabulary, sorted. It reads the same
// registry that Unmarshal dispatches on, so a caller building a pattern
// over kind names cannot drift from the kinds that actually exist —
// adding a kind is a code change in one place (ADR-W004 A1), and this
// follows it.
func Kinds() []Kind {
	out := make([]Kind, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// NodeScoped reports whether resources of kind k are applied at a
// Wardstone node — the same registry flag Validate enforces. Callers that
// dispatch by node (the reconcile loop) use it to tell "this bucket has
// no node because the kind has none" from "this bucket has no node
// because something skipped validation".
func NodeScoped(k Kind) bool {
	return registry[k].nodeScoped
}
