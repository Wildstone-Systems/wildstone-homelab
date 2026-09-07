package pkgtool

import (
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// The catalog grew from sixteen packages to more than fifty in one
// change, and every one of the new packages was written by hand from
// the same handful of shapes. Lint checks each rendered resource on its
// own; what it cannot see is whether the resources in one package agree
// with EACH OTHER — a Container mounting a Volume nobody declared, a
// Route pointing at a container name with a typo, a BackupPlan naming a
// repository volume that does not exist, an OidcClient whose redirect
// host is not the host the Route serves. Each of those expands cleanly,
// validates cleanly, and fails on the node, in a plan somebody already
// approved.
//
// These tests are the cross-resource half of the contract, run over the
// whole shipped catalog so a package cannot ship without meeting them.

// secretRefRegexp finds secret/<name> references wherever a package can
// carry one: env values, ConfigFile content, BackupPlan passwords,
// OidcClient secretRefs.
var secretRefRegexp = regexp.MustCompile(`secret/([A-Za-z0-9._-]+)`)

// TestShippedSecretReferencesAreDeclared: every secret/<name> a package
// references must be a Secret it declares (or one the engine
// synthesises for it, which is in the rendered set too). The failure
// this prevents is the one F37 recorded: an operator meets each missing
// secret as ERROR_CODE_NOT_FOUND, one per tick, after approving a plan
// that could never have converged.
func TestShippedSecretReferencesAreDeclared(t *testing.T) {
	checked := 0
	for name, rendered := range renderShippedPackages(t) {
		t.Run(name, func(t *testing.T) {
			declared := map[string]bool{}
			for _, r := range rendered {
				if r.Kind == resource.KindSecret && r.Secret != nil {
					declared[r.Secret.Name] = true
				}
			}
			var refs []string
			note := func(where, text string) {
				for _, m := range secretRefRegexp.FindAllStringSubmatch(text, -1) {
					refs = append(refs, where+"="+m[1])
				}
			}
			for _, r := range rendered {
				switch {
				case r.Kind == resource.KindContainer && r.Container != nil:
					for k, v := range r.Container.Env {
						note("Container/"+r.Metadata.Name+" env "+k, v)
					}
				case r.Kind == resource.KindJob && r.Job != nil:
					for k, v := range r.Job.Run.Env {
						note("Job/"+r.Metadata.Name+" env "+k, v)
					}
				case r.Kind == resource.KindConfigFile && r.ConfigFile != nil:
					note("ConfigFile/"+r.Metadata.Name, r.ConfigFile.Content)
				case r.Kind == resource.KindBackupPlan && r.BackupPlan != nil:
					note("BackupPlan/"+r.Metadata.Name+" passwordSecret", r.BackupPlan.PasswordSecret)
				case r.Kind == resource.KindOidcClient && r.OidcClient != nil:
					note("OidcClient/"+r.Metadata.Name+" secretRef", r.OidcClient.SecretRef)
				}
			}
			sort.Strings(refs)
			for _, ref := range refs {
				checked++
				where, secret, _ := strings.Cut(ref, "=")
				if !declared[secret] {
					t.Errorf("%s references secret/%s, which this package never declares: the node will refuse the "+
						"step with NOT_FOUND after the plan is approved. Add a Secret resource (provided: true if only "+
						"a person can supply it).", where, secret)
				}
			}
		})
	}
	if checked == 0 {
		t.Fatal("no shipped package referenced a secret — the walk is broken, not the packages")
	}
}

// TestShippedMountsNameDeclaredVolumes: a mount whose source is not a
// host path is a named Volume, and Docker will happily create an
// anonymous one for a name nothing declared — so a typo in a mount
// source does not fail, it silently gives the container an empty disk
// that is in no BackupPlan.
func TestShippedMountsNameDeclaredVolumes(t *testing.T) {
	checked := 0
	for name, rendered := range renderShippedPackages(t) {
		t.Run(name, func(t *testing.T) {
			volumes := map[string]bool{}
			for _, r := range rendered {
				if r.Kind == resource.KindVolume && r.Volume != nil {
					volumes[r.Volume.Name] = true
				}
			}
			for _, r := range runnables(rendered) {
				for i, m := range r.spec.Mounts {
					if strings.HasPrefix(m.Source, "/") {
						continue
					}
					checked++
					if !volumes[m.Source] {
						t.Errorf("%s %q mounts[%d] names volume %q, which this package does not declare: Docker "+
							"would create an anonymous one, outside every BackupPlan", r.kind, r.name, i, m.Source)
					}
				}
			}
		})
	}
	if checked == 0 {
		t.Fatal("no shipped container mounted a named volume — the walk is broken, not the packages")
	}
}

// TestShippedRouteUpstreamsNameAContainerInThePackage: the reconciler
// resolves "container/<name>" on the node's Docker network, so a name
// that no Container in the package produces renders a Route to nowhere
// — healthy on paper, 502 in the browser.
func TestShippedRouteUpstreamsNameAContainerInThePackage(t *testing.T) {
	checked := 0
	for name, rendered := range renderShippedPackages(t) {
		t.Run(name, func(t *testing.T) {
			containers := map[string]bool{}
			for _, r := range rendered {
				if r.Kind == resource.KindContainer && r.Container != nil {
					containers[r.Metadata.Name] = true
				}
			}
			for _, r := range rendered {
				if r.Kind != resource.KindRoute || r.Route == nil {
					continue
				}
				rest, ok := strings.CutPrefix(r.Route.Upstream, "container/")
				if !ok {
					continue
				}
				checked++
				ctr, _, _ := strings.Cut(rest, ":")
				if !containers[ctr] {
					t.Errorf("Route %q upstream %q names container %q, which this package does not run",
						r.Metadata.Name, r.Route.Upstream, ctr)
				}
			}
		})
	}
	if checked == 0 {
		t.Fatal("no shipped Route had a container upstream — the walk is broken, not the packages")
	}
}

// TestShippedOidcWiringIsCoherent pins the three facts an app_oidc
// integration depends on, none of which any single resource can check:
//
//   - a Route whose auth is app_oidc has an OidcClient in the package
//     (the proxy passes through BECAUSE the app authenticates itself,
//     and without a registration it cannot);
//   - every https redirect URI points at a host some Route in the
//     package serves — a redirect to a host nobody serves is a login
//     that ends on a blank page;
//   - the client id the OidcClient registers is the one the app is
//     told, in its env or its rendered config, so the two halves of
//     the handshake agree.
//
// The immich fixture this project's go/no-go gate was built around is
// exactly this wiring; these are the mistakes that copying it thirty
// times invites.
func TestShippedOidcWiringIsCoherent(t *testing.T) {
	checked := 0
	for name, sp := range walkShipped(t) {
		rendered := sp.rendered
		t.Run(name, func(t *testing.T) {
			hosts := map[string]bool{}
			var appOidcRoutes []string
			for _, r := range rendered {
				if r.Kind == resource.KindRoute && r.Route != nil {
					hosts[r.Route.Host] = true
					if r.Route.Auth == "app_oidc" {
						appOidcRoutes = append(appOidcRoutes, r.Metadata.Name)
					}
				}
			}
			var told strings.Builder // everything the app is configured with
			for _, r := range rendered {
				switch {
				case r.Kind == resource.KindContainer && r.Container != nil:
					for _, v := range r.Container.Env {
						told.WriteString(v + "\n")
					}
				case r.Kind == resource.KindConfigFile && r.ConfigFile != nil:
					told.WriteString(r.ConfigFile.Content + "\n")
				}
			}
			clients := 0
			for _, r := range rendered {
				if r.Kind != resource.KindOidcClient || r.OidcClient == nil {
					continue
				}
				clients++
				checked++
				for _, raw := range r.OidcClient.RedirectUris {
					u, err := url.Parse(raw)
					if err != nil || u.Scheme != "https" {
						continue // app-scheme callbacks (app.immich:///...) are the app's own
					}
					if !hosts[u.Host] {
						t.Errorf("OidcClient %q redirects to %s, but no Route in this package serves host %q",
							r.Metadata.Name, raw, u.Host)
					}
				}
				if !strings.Contains(told.String(), r.OidcClient.ClientId) {
					t.Errorf("OidcClient %q registers client id %q, but nothing in this package's env or rendered "+
						"config tells the app that id — the two halves of the login will not agree",
						r.Metadata.Name, r.OidcClient.ClientId)
				}
				requires := false
				for _, req := range sp.pkg.Requires {
					if req.Trait == "identity" && req.Mode == "oidc" {
						requires = true
					}
				}
				if !requires {
					t.Errorf("package renders OidcClient %q but does not require identity:oidc, so the engine "+
						"would expand it against a deployment with no provider", r.Metadata.Name)
				}
			}
			if len(appOidcRoutes) > 0 && clients == 0 {
				t.Errorf("Route(s) %v are auth: app_oidc — the proxy passes through because the app signs in through "+
					"the identity provider — but this package registers no OidcClient", appOidcRoutes)
			}
		})
	}
	if checked == 0 {
		t.Fatal("no shipped package rendered an OidcClient — the walk is broken, not the packages")
	}
}

// TestShippedBackupPlansAreCoherent: a BackupPlan names volumes to back
// up, a volume to back them up INTO, and a password. All three are
// names, and a name is exactly what this walk can check: each must be
// declared in the same package, and the repository must not be among
// the sources (restic backing up its own repository into itself fills
// the disk, slowly, and every snapshot after the first is wrong).
func TestShippedBackupPlansAreCoherent(t *testing.T) {
	checked := 0
	for name, rendered := range renderShippedPackages(t) {
		t.Run(name, func(t *testing.T) {
			volumes := map[string]bool{}
			secrets := map[string]bool{}
			for _, r := range rendered {
				switch {
				case r.Kind == resource.KindVolume && r.Volume != nil:
					volumes[r.Volume.Name] = true
				case r.Kind == resource.KindSecret && r.Secret != nil:
					secrets[r.Secret.Name] = true
				}
			}
			for _, r := range rendered {
				if r.Kind != resource.KindBackupPlan || r.BackupPlan == nil {
					continue
				}
				checked++
				bp := r.BackupPlan
				for _, v := range bp.Volumes {
					if !volumes[v] {
						t.Errorf("BackupPlan %q backs up volume %q, which this package does not declare", r.Metadata.Name, v)
					}
					if v == bp.Repository {
						t.Errorf("BackupPlan %q backs up its own repository volume %q into itself", r.Metadata.Name, v)
					}
				}
				if !volumes[bp.Repository] {
					t.Errorf("BackupPlan %q writes to repository volume %q, which this package does not declare", r.Metadata.Name, bp.Repository)
				}
				if s, ok := strings.CutPrefix(bp.PasswordSecret, "secret/"); !ok || !secrets[s] {
					t.Errorf("BackupPlan %q password %q is not a secret this package declares", r.Metadata.Name, bp.PasswordSecret)
				}
			}
		})
	}
	if checked == 0 {
		t.Fatal("no shipped package declared a BackupPlan — the walk is broken, not the packages")
	}
}

// TestEveryShippedRouteLabelIsPinned is F82 applied to the whole
// catalog, walked rather than listed. Under the tailnet posture an
// unpinned Route takes the next free port, which moves when anything
// sorting before it is installed — and the Console shows the package
// as blocked there ("the links it builds for itself will not match
// where it is served"). So a shipped package with a Route must have
// its default label in keystone/ingressorigin's wellKnown table, or it
// is not installable under the posture the installer offers first.
func TestEveryShippedRouteLabelIsPinned(t *testing.T) {
	checked := 0
	for name, sp := range walkShipped(t) {
		for _, label := range pkgspec.RouteLabels(sp.pkg) {
			checked++
			if _, ok := ingressorigin.WellKnownPortFor(label); !ok {
				t.Errorf("packages/%s declares a Route labelled %q with no pinned port in keystone/ingressorigin: "+
					"under the tailnet posture it takes whatever port is free, moves when a neighbour is installed, "+
					"and the Console reports the package as blocked. Add a wellKnown entry.", name, label)
			}
		}
	}
	if checked < 7 {
		t.Fatalf("checked only %d Route labels — the walk is broken, not the packages", checked)
	}
}

// TestTailnetIngressPublishesTheWholePortBlock holds the two halves of
// the tailnet addressing together: ingressorigin.PortBlock is what
// Keystone assigns from, and ingress-tailnet-ts's published ports are
// what the node actually binds. A port in the block but not published
// is a Route that converges, reports healthy, and is unreachable; a
// port published but not in the block is a listener nothing will ever
// be assigned to.
func TestTailnetIngressPublishesTheWholePortBlock(t *testing.T) {
	rendered, ok := renderShippedPackages(t)["ingress-tailnet-ts"]
	if !ok {
		t.Fatal("no ingress-tailnet-ts package under packages/")
	}
	published := map[int]bool{}
	for _, r := range runnables(rendered) {
		for _, p := range r.spec.Ports {
			if _, hostPort, _, ok := resource.SplitPortMapping(p); ok {
				published[int(hostPort)] = true
			}
		}
	}
	want := map[int]bool{}
	for _, p := range ingressorigin.PortBlock {
		want[p] = true
		if !published[p] {
			t.Errorf("PortBlock contains %d but ingress-tailnet-ts does not publish it: a Route assigned that port "+
				"converges, reports healthy, and is unreachable", p)
		}
	}
	for p := range published {
		if !want[p] {
			t.Errorf("ingress-tailnet-ts publishes %d, which is not in PortBlock: nothing will ever be served there", p)
		}
	}
	// Every pin must be somewhere in the block, or the pin is a port
	// the proxy does not have.
	for name, sp := range walkShipped(t) {
		for _, label := range pkgspec.RouteLabels(sp.pkg) {
			if p, ok := ingressorigin.WellKnownPortFor(label); ok && !want[p] {
				t.Errorf("packages/%s label %q is pinned to %d, which is outside PortBlock", name, label, p)
			}
		}
	}
}

// TestCatalogShipsTheLaunchServiceSet names the packages the catalog is
// committed to carrying — the household services the 2026-09 catalog
// expansion was asked for — so a rename or a deletion is a deliberate
// edit to this list rather than a package quietly going missing from
// a release bundle. The count is the goal that expansion set (fifty
// installable packages) and is here so that the number is a test
// rather than a claim.
func TestCatalogShipsTheLaunchServiceSet(t *testing.T) {
	dirs := packageDirs(t)
	have := map[string]bool{}
	for _, d := range dirs {
		have[filepath.Base(d)] = true
	}
	for _, want := range []string{
		// launch set
		"identity", "ingress", "dashboard", "monitoring", "notifications",
		"media-jellyfin", "photos-immich", "documents-paperless", "home-automation",
		// the household list the expansion was asked for
		"home-assistant", "cameras-frigate", "media-plex", "dns-pihole", "dns-adguard",
		"ai-ollama", "passwords-vaultwarden", "zigbee-zigbee2mqtt", "mqtt-mosquitto",
		"ai-open-webui", "metrics-grafana", "downloads-qbittorrent", "movies-radarr",
		"music-lidarr", "tv-sonarr", "indexers-prowlarr", "requests-seerr",
		"dashboard-homarr", "audiobooks-audiobookshelf", "music-navidrome",
		"monitoring-beszel", "books-calibre-web", "books-kavita",
	} {
		if !have[want] {
			t.Errorf("packages/%s is missing from the catalog", want)
		}
	}
	if len(dirs) < 50 {
		t.Errorf("the catalog ships %d packages; the expansion committed to at least 50", len(dirs))
	}
}
