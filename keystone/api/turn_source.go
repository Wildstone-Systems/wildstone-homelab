package api

import (
	"context"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// How a person's words reached the Agent travels with the request, so
// the three session runners label the stored turn without every caller
// in between growing a parameter for it (ADR-W042 §2.4). It is a label
// for the conversations page and authorises nothing.
type turnSourceKey struct{}

// withVoiceTurn marks the turn this context runs as spoken.
func withVoiceTurn(ctx context.Context) context.Context {
	return context.WithValue(ctx, turnSourceKey{}, runtimedb.TurnSourceVoice)
}

// turnSource is the stored source for the turn this context runs: ""
// for typed text.
func turnSource(ctx context.Context) string {
	src, _ := ctx.Value(turnSourceKey{}).(string)
	return src
}
