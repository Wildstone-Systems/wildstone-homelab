package resource

import "sort"

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
