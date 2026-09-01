package reconcile

// Managed config in the container spec hash.
//
// A container reads its configuration from files Wildstone writes into
// the managed prefix and bind-mounts in. Those files are NOT part of the
// container spec, so changing one changes nothing the agent compares:
// the spec hash matches, EnsureContainer is a no-op, and the container
// goes on serving the configuration it read at start. Converged on
// paper, running stale config in fact.
//
// This is the same failure the network dimension of specHash already
// exists to prevent (see specHash's own comment: a pre-minor-8 container
// stayed "converged on paper, unreachable by name in fact"), and it gets
// the same answer rather than a new one. Folding a digest of the managed
// files a container mounts into its spec hash makes a config change a
// SPEC change, so the existing machinery replaces the container and the
// new config is read — level-triggered, with no reload op, no seam, and
// no need for Keystone to know which container is which application.
//
// The alternative was an Authelia-specific reload seam. It would have
// meant naming a container Keystone does not own, which is the layering
// ADR-W005 exists to prevent, and it would have covered exactly one app.
//
// WHAT IS DELIBERATELY EXCLUDED: the ingress capability's own files —
// the base Caddyfile, the placeholder fragment, and the per-Route
// fragments under <prefix>/routes/. Both shipped ingress profiles run
// Caddy with --watch, so writing a fragment IS the reload (verified on a
// live Caddy, add and remove alike, with unaffected routes left serving
// throughout — see ReloadFn). Including them would trade a free,
// zero-downtime reload for a container replacement on every Route
// change, which is strictly worse. An app that can reload its own config
// should be excluded here for the same reason.

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// managedConfigContents returns the DESIRED content of every managed
// file that participates in a container's spec hash, keyed by its path
// on the node.
//
// Desired content, not observed: Keystone is deciding what the container
// should be running against, and the applier's own health gate is what
// establishes that the file on disk matches. Using observed content
// would also be impossible for a materialized file, whose bytes Keystone
// never sees (it holds no secrets — ADR-W005 A2).
//
// It is a pure function of the desired set so that observe and apply,
// which reach it by different paths, cannot disagree about what a
// container's configuration is within one tick.
func managedConfigContents(managedPrefix string, desiredByKey map[resKey]resource.Resource) map[string]string {
	if managedPrefix == "" {
		return nil
	}
	out := make(map[string]string)

	desired := make([]resource.Resource, 0, len(desiredByKey))
	for _, r := range desiredByKey {
		desired = append(desired, r)
		if r.Kind != resource.KindConfigFile || r.ConfigFile == nil {
			continue
		}
		p, err := configFilePath(managedPrefix, r)
		if err != nil {
			// A ConfigFile whose path cannot be derived is one the
			// applier will fail on with the same error. Skipping it here
			// leaves the container's hash unaffected by a file that is
			// never going to be written, which is the honest answer.
			continue
		}
		out[p] = r.ConfigFile.Content
	}

	// The merged Authelia clients file is rendered by the reconciler
	// rather than declared by a package, but it is read by a container
	// exactly like a ConfigFile is, and a client added to it must reach
	// the running provider. renderAutheliaClients sorts internally, so
	// the map iteration order above does not leak into the digest.
	out[autheliaClientsPath(managedPrefix)] = string(renderAutheliaClients(desired))

	// The LLDAP group list, for the same reason and with more force: the
	// job that reads it re-runs BECAUSE this digest changed. Declaring a
	// new group is what has to reach the directory, and this is the
	// whole mechanism by which it does.
	out[lldapGroupsPath(managedPrefix)] = string(renderLdapGroups(desired))

	return out
}

// configDigest is the digest of the managed configuration visible to
// spec through its bind mounts, or "" when the container mounts none.
//
// Empty is load-bearing: specHash appends nothing for it, so every
// container that reads no managed config keeps the hash it already has
// and is NOT replaced when this lands. Only the containers that actually
// mount managed config churn once, which is the smallest correct blast
// radius for the change.
func configDigest(spec *resource.ContainerSpec, contents map[string]string) string {
	if spec == nil || len(contents) == 0 {
		return ""
	}

	// A file can be visible through more than one mount; the set of
	// (path, content) pairs is what matters, not how many ways in there
	// are. Sorted, so the digest does not depend on mount or map order.
	seen := make(map[string]bool)
	var paths []string
	for _, m := range spec.Mounts {
		if !strings.HasPrefix(m.Source, "/") {
			continue // named volume: nothing of ours is under it
		}
		for p := range contents {
			if !underMount(m.Source, p) || seen[p] {
				continue
			}
			seen[p] = true
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return ""
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write([]byte(contents[p]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// underMount reports whether file is at or beneath dir, comparing path
// SEGMENTS so that /etc/wildstone/identity-old does not count as being
// under /etc/wildstone/identity.
func underMount(dir, file string) bool {
	dir, file = path.Clean(dir), path.Clean(file)
	return dir == file || strings.HasPrefix(file, dir+"/")
}
