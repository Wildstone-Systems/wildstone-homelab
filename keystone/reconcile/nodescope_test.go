package reconcile

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// TestReconcileNamesANodelessResource covers the case validation is meant
// to make unreachable. resource.Validate now refuses a node-scoped
// resource with no metadata.node, but resources are also constructed in
// code (Butler fixtures, tests, future callers), and the reconciler is
// the last place that can say something useful about one: it is handed a
// nodeID of "" and has no session to dispatch through.
//
// The pre-fix behavior was the whole problem — the error arrived from the
// transport several frames down, naming neither the resource nor the
// missing field, so a deployment where every OidcClient silently never
// reconciled looked like a connectivity fault.
func TestReconcileNamesANodelessResource(t *testing.T) {
	rec := newReconciler(nil)
	_, err := rec.Reconcile(t.Context(), "", []resource.Resource{{
		Kind:     resource.KindOidcClient,
		Metadata: resource.Metadata{Name: "immich", Owner: "capability/photos"},
		OidcClient: &resource.OidcClientSpec{
			ClientId: "immich", ClientName: "Immich",
			RedirectUris: []string{"https://photos.example.com/cb"},
			SecretRef:    "secret/photos-oidc", AuthPolicy: "two_factor",
		},
	}})
	if err == nil {
		t.Fatal("Reconcile accepted a node-scoped resource with no node")
	}
	if !strings.Contains(err.Error(), "OidcClient/immich") || !strings.Contains(err.Error(), "metadata.node") {
		t.Fatalf("error must name the resource and the missing field, got: %v", err)
	}
}

// A provider-backed kind at node "" is the legitimate case and must still
// pass — the guard is about node-scoped kinds, not about "" itself.
func TestReconcileAcceptsProviderKindsAtNoNode(t *testing.T) {
	rec := newReconciler(nil)
	if _, err := rec.Reconcile(t.Context(), "", nil); err != nil {
		t.Fatalf("empty desired at no node: %v", err)
	}
}
