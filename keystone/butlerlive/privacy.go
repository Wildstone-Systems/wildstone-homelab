package butlerlive

import (
	"context"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Enforce at every call, including tool-result rounds and retries, and again
// before accepting output. Local inference never bypasses destination policy.
type privacyProvider struct {
	provider.Provider
	privacy *runtimedb.PrivacyTurn
}

func (p privacyProvider) Chat(ctx context.Context, r provider.ChatRequest) (*provider.ChatResponse, error) {
	if err := p.privacy.Check(); err != nil {
		return nil, err
	}
	response, err := p.Provider.Chat(provider.WithAttemptCheck(ctx, p.privacy.Check), r)
	if err != nil {
		return nil, err
	}
	if err = p.privacy.Check(); err != nil {
		return nil, err
	}
	return response, nil
}

// GuardPrivacyProvider shares the live inference boundary with legacy sessions.
func GuardPrivacyProvider(p provider.Provider, privacy *runtimedb.PrivacyTurn) provider.Provider {
	return privacyProvider{Provider: p, privacy: privacy}
}
