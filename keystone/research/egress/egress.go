// Package egress is the mandatory provenance boundary for model-authored
// research. Transport adapters remain usable for fixed synthetic Owner probes;
// native tools must use this seam before handing a query or URL to any adapter.
package egress

import (
	"context"

	"github.com/chickenrat17/wildstone-homelab/keystone/research"
	"github.com/chickenrat17/wildstone-homelab/keystone/research/publicfetch"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func Search(ctx context.Context, adapter research.SearchAdapter, request research.SearchRequest) (research.SearchResult, error) {
	if err := runtimedb.CheckResearchEgress(ctx); err != nil {
		return research.SearchResult{}, err
	}
	return adapter.Search(ctx, request)
}

type FetchAdapter interface {
	Fetch(context.Context, string) (publicfetch.Page, error)
}

func Fetch(ctx context.Context, adapter FetchAdapter, url string) (publicfetch.Page, error) {
	if err := runtimedb.CheckResearchEgress(ctx); err != nil {
		return publicfetch.Page{}, err
	}
	return adapter.Fetch(ctx, url)
}
