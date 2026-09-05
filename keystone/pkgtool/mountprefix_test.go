package pkgtool

import (
	"strings"
	"testing"
)

// defaultMountPrefixes are the host paths Wardstone bind-mounts from with
// a stock configuration (ADR-W003 A1). An agent may be configured with
// others, which is exactly why this is a CATALOG rule and not schema
// validation: a household's own Container mounting /data/media is fine
// if their node allows it, but a package Wildstone SHIPS must work on a
// node configured the way the installer configures it.
var defaultMountPrefixes = []string{"/etc/wildstone", "/srv/wildstone"}

// TestShippedPackagesMountOnlyManagedPrefixes.
//
// This exists because of a specific failure, and the shape of it matters
// more than the rule. packages/ingress-tailnet-ts mounted
// /var/run/tailscale/tailscaled.sock — the socket Caddy fetches a
// certificate from. It passed resource validation (which checks only
// that source and target are non-empty), passed lint, expanded cleanly,
// and produced a dry-run plan that read exactly right. It was approved
// on that evidence.
//
// It then failed on the node with ERROR_CODE_FORBIDDEN, in a plan whose
// EARLIER step had already deleted the proxy it was replacing. The
// household was left with no ingress at all, and the only thing in the
// entire system that had objected was the agent, at apply time.
//
// So the rule is enforced here, against the shipped catalog, where being
// wrong is a failing test rather than a half-applied plan.
func TestShippedPackagesMountOnlyManagedPrefixes(t *testing.T) {
	checked := 0
	for name, rendered := range renderShippedPackages(t) {
		t.Run(name, func(t *testing.T) {
			for _, r := range runnables(rendered) {
				for i, m := range r.spec.Mounts {
					if !strings.HasPrefix(m.Source, "/") {
						continue // a named Volume, not a host path
					}
					checked++
					if !underAnyPrefix(m.Source) {
						t.Errorf("%s %q mounts[%d] source %q is outside %v; Wardstone refuses it at apply "+
							"time, so this reaches a node as a forbidden step inside an already-approved plan",
							r.kind, r.name, i, m.Source, defaultMountPrefixes)
					}
				}
			}
		})
	}
	if checked == 0 {
		t.Fatal("no shipped container mounted a host path — the walk is broken, not the packages")
	}
}

func underAnyPrefix(source string) bool {
	if strings.Contains(source, "..") {
		return false
	}
	for _, p := range defaultMountPrefixes {
		if source == p || strings.HasPrefix(source, p+"/") {
			return true
		}
	}
	return false
}
