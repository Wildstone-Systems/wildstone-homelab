package api

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

const platformFactsTimeout = 3 * time.Second

// checkPackagePlatforms refuses a package-declared workload before an
// approvable Plan exists when any selected target is not a known supported
// platform. Omitted platforms preserve existing behavior. Config-only
// packages also need no host eligibility check because they create no
// Container or Job workload.
func (s *Server) checkPackagePlatforms(ctx context.Context, pkg *pkgspec.Package, rendered []resource.Resource) error {
	if len(pkg.Platforms) == 0 {
		return nil
	}
	nodes := map[string]bool{}
	for _, r := range rendered {
		if r.Kind == resource.KindContainer || r.Kind == resource.KindJob {
			nodes[r.Metadata.Node] = true
		}
	}
	if len(nodes) == 0 {
		return nil
	}
	if s.Agents == nil {
		return fmt.Errorf("package %s declares platforms, but node platform facts are unavailable", pkg.Metadata.Name)
	}

	ordered := make([]string, 0, len(nodes))
	for node := range nodes {
		ordered = append(ordered, node)
	}
	sort.Strings(ordered)
	allowed := make([]string, len(pkg.Platforms))
	for i, p := range pkg.Platforms {
		allowed[i] = string(p)
	}

	for _, node := range ordered {
		probeCtx, cancel := context.WithTimeout(ctx, platformFactsTimeout)
		res, err := s.Agents.Apply(probeCtx, node, &wsv1.Operation{
			Op: &wsv1.Operation_HostFacts{HostFacts: &wsv1.HostFacts{}},
		})
		cancel()
		if err != nil {
			return fmt.Errorf("package %s platform check for node %s: %w", pkg.Metadata.Name, node, err)
		}
		if res == nil {
			return fmt.Errorf("package %s platform check for node %s returned no result", pkg.Metadata.Name, node)
		}
		if opErr := res.GetError(); opErr != nil {
			return fmt.Errorf("package %s platform check for node %s: %s", pkg.Metadata.Name, node, opErr.GetMessage())
		}
		facts := res.GetHostFacts()
		if facts == nil {
			return fmt.Errorf("package %s platform check for node %s returned no HostFacts", pkg.Metadata.Name, node)
		}
		os, arch := facts.GetOs(), facts.GetArchitecture()
		if os == "" || arch == "" {
			return fmt.Errorf("package %s supports %s, but node %s did not report a canonical OS/architecture (wardstoned protocol minor 35 or later required)",
				pkg.Metadata.Name, strings.Join(allowed, ", "), node)
		}
		if !pkg.SupportsPlatform(os, arch) {
			return fmt.Errorf("package %s supports %s, but node %s reports %s/%s", pkg.Metadata.Name,
				strings.Join(allowed, ", "), node, os, arch)
		}
	}
	return nil
}
