package butlerlive

import (
	"context"
	"errors"
	"fmt"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

var ErrEmbeddingAssignmentChanged = errors.New("butlerlive: embedding assignment changed")

// ResolveEmbedder binds embedding to the exact current Agent assignment.
// The resulting adapter rechecks that assignment and the turn privacy policy
// before every provider HTTP attempt and after the response returns.
func ResolveEmbedder(db *runtimedb.DB, agent string, privacy *runtimedb.PrivacyTurn) (provider.Embedder, error) {
	b, err := db.GetButler(agent)
	if err != nil {
		return nil, err
	}
	if !b.Reachable() || b.Provider != "ollama" || b.Endpoint == "" || b.ProviderModel == "" {
		return nil, provider.ErrEmbedderNotConfigured
	}
	inner := provider.NewOllamaEmbedder(b.Endpoint, b.ProviderModel)
	check := func() error {
		if privacy == nil {
			return runtimedb.ErrPrivacy
		}
		if err := privacy.CheckModel(b.ModelKind); err != nil {
			return err
		}
		current, err := db.GetButlerBySeq(b.Seq)
		if err != nil {
			return ErrEmbeddingAssignmentChanged
		}
		if !current.Reachable() || current.ModelSeq != b.ModelSeq || current.ModelKind != b.ModelKind ||
			current.Provider != b.Provider || current.ProviderModel != b.ProviderModel || current.Endpoint != b.Endpoint {
			return ErrEmbeddingAssignmentChanged
		}
		return nil
	}
	return checkedEmbedder{inner: inner, check: check}, nil
}

type checkedEmbedder struct {
	inner provider.Embedder
	check func() error
}

func (e checkedEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := e.check(); err != nil {
		return nil, err
	}
	vector, err := e.inner.Embed(provider.WithAttemptCheck(ctx, e.check), text)
	if err != nil {
		return nil, err
	}
	if err := e.check(); err != nil {
		return nil, err
	}
	if len(vector) == 0 {
		return nil, fmt.Errorf("%w: empty vector", provider.ErrEmbeddingMalformed)
	}
	return vector, nil
}
