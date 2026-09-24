package butlerlive

import (
	"context"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// SystemPromptFor assembles a Butler's system prompt exactly as Serve
// does — the surface's hierarchy, the household clock, the directive,
// the memory preamble, the caller's audience note — for a caller that
// builds the session elsewhere.
//
// It exists for the Agent Gateway (ADR-W039 Phase 2a), which returns
// the prompt in the manifest rather than wrapping the Toolset. One
// function, two callers: the prompt a Butler runs under must not
// depend on which of them opened the session, and a copy would be a
// second place for the hierarchy to be reordered. Serve is unchanged
// and still calls systemFor directly.
func SystemPromptFor(ctx context.Context, gw butler.Toolset, b runtimedb.Butler, t Turn, clock string) string {
	return systemFor(ctx, gw, b, t, clock)
}
