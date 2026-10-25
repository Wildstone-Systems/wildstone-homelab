package reconcile

import (
	"context"
	"errors"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// VerifyRunningContainer performs only a fresh, owned-container inspection. It
// requires the exact current reconcile spec (including image, port bindings,
// config contents and node host aliases); old agents get no presence fallback.
// This is a dependency check, not proof that an upstream service works.
func (r *Reconciler) VerifyRunningContainer(ctx context.Context, node, name string, desired []resource.Resource) error {
	unavailable := errors.New("container dependency unavailable")
	if r == nil || r.Applier == nil || node == "" || ctx.Err() != nil {
		return unavailable
	}
	byKey := make(map[resKey]resource.Resource)
	for _, res := range desired {
		if res.Metadata.Node != node {
			continue
		}
		key := resKey{res.Kind, res.Metadata.Name}
		if _, exists := byKey[key]; exists {
			return unavailable
		}
		byKey[key] = res
	}
	want, ok := byKey[resKey{resource.KindContainer, name}]
	if !ok || want.Container == nil || want.Validate() != nil {
		return unavailable
	}
	hash := specHash(want.Container, ContainerNetwork,
		configDigest(r.ManagedPrefix, want.Container, managedConfigContents(r.ManagedPrefix, byKey)),
		r.hostAliasesForNode(node, byKey))
	res, err := r.Applier.Apply(ctx, node, docker.inspectContainer(name))
	if err != nil || ctx.Err() != nil || res.GetError() != nil {
		return unavailable
	}
	got := res.GetInspectContainer()
	if !got.GetRunning() || got.GetImage() != want.Container.Image || hash == "" || got.GetLabels()[specHashLabel] != hash {
		return unavailable
	}
	if status := got.GetStatus(); status != "" && status != "running" {
		return unavailable
	}
	if health := got.GetAppHealth(); health != "" && health != "healthy" {
		return unavailable
	}
	return nil
}
