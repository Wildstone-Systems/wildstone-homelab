package pkgtool

import (
	"encoding/json"
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
		// unblocked by protocol minor 12 (F98)
		"vpn-gluetun",
	} {
		if !have[want] {
			t.Errorf("packages/%s is missing from the catalog", want)
		}
	}
	if len(dirs) < 50 {
		t.Errorf("the catalog ships %d packages; the expansion committed to at least 50", len(dirs))
	}
}

// TestShippedSeedsNameDeclaredVolumes: a SeedFile writes into a volume by
// name, before the app starts. A name no Volume in the package declares
// is a seed the node refuses (the volume does not exist) or, worse, one
// that lands in a volume belonging to someone else.
func TestShippedSeedsNameDeclaredVolumes(t *testing.T) {
	checked := 0
	for name, rendered := range renderShippedPackages(t) {
		t.Run(name, func(t *testing.T) {
			volumes := map[string]bool{}
			for _, r := range rendered {
				if r.Kind == resource.KindVolume && r.Volume != nil {
					volumes[r.Volume.Name] = true
				}
			}
			for _, r := range rendered {
				if r.Kind != resource.KindSeedFile || r.SeedFile == nil {
					continue
				}
				checked++
				if !volumes[r.SeedFile.Volume] {
					t.Errorf("SeedFile %q writes into volume %q, which this package does not declare",
						r.Metadata.Name, r.SeedFile.Volume)
				}
			}
		})
	}
	_ = checked // a catalog with no seeds at all is legitimate
}

// TestHomeAssistantSeedsItsProxyTrust pins the specific regression, and
// the specific SHAPE of the fix.
//
// Home Assistant reads http: exactly once, on its first start. Deliver
// that setting any later and the service answers 400 to every proxied
// request while reporting a perfectly converged install.
//
// Delivering it through configuration.yaml does not work either, and
// fails later, which is worse: a migrated YAML block is staged as a
// PENDING config on trial and auto-reverts after five minutes unless a
// person promotes it in the UI. So the settings store is seeded directly
// with the value already in `stable`. See ADR-W029.
func TestHomeAssistantSeedsItsProxyTrust(t *testing.T) {
	rendered, ok := renderShippedPackages(t)["home-assistant"]
	if !ok {
		t.Fatal("home-assistant is not in the catalog")
	}

	seeds := map[string]string{}
	for _, r := range rendered {
		if r.Kind == resource.KindSeedFile && r.SeedFile != nil {
			seeds[r.SeedFile.Path] = r.SeedFile.Content
		}
	}

	store, ok := seeds[".storage/http"]
	if !ok {
		t.Fatal("home-assistant does not seed .storage/http: the proxy trust would arrive as a five-minute trial and auto-revert")
	}
	var parsed struct {
		Data struct {
			Stable struct {
				UseXForwardedFor bool     `json:"use_x_forwarded_for"`
				TrustedProxies   []string `json:"trusted_proxies"`
			} `json:"stable"`
			Pending           any  `json:"pending"`
			YamlMigrationDone bool `json:"yaml_migration_done"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(store), &parsed); err != nil {
		t.Fatalf("the seeded settings store is not valid JSON (%v):\n%s", err, store)
	}
	if !parsed.Data.Stable.UseXForwardedFor {
		t.Error("use_x_forwarded_for must be in the CONFIRMED `stable` slot, or proxied requests 400")
	}
	if parsed.Data.Pending != nil {
		t.Error("`pending` must be null: a pending config is a trial that auto-reverts after five minutes")
	}
	if !parsed.Data.YamlMigrationDone {
		t.Error("yaml_migration_done must be true, or the first boot migrates YAML and stages a trial")
	}
	// The derived value, not a per-package guess.
	if len(parsed.Data.Stable.TrustedProxies) == 0 {
		t.Error("no trusted_proxies seeded")
	}
	var sawDockerRange bool
	for _, c := range parsed.Data.Stable.TrustedProxies {
		if c == "172.16.0.0/12" {
			sawDockerRange = true
		}
	}
	if !sawDockerRange {
		t.Errorf("trusted_proxies should come from .Resolved.ProxyTrustCIDRs; got %v", parsed.Data.Stable.TrustedProxies)
	}

	// With the migration marked done, a leftover http: block in YAML is
	// ignored and only raises a "your YAML is being ignored" repair
	// notice — so the seeded configuration.yaml must not carry one.
	config, ok := seeds["configuration.yaml"]
	if !ok {
		t.Fatal("home-assistant does not seed configuration.yaml")
	}
	for _, line := range strings.Split(config, "\n") {
		if strings.HasPrefix(line, "http:") {
			t.Error("configuration.yaml carries an http: block; with the migration already done it is ignored and only raises a repair notice")
		}
	}

	// Home Assistant only creates its default config directory when
	// configuration.yaml is ABSENT, and returns early the moment it finds
	// one. Seeding that file therefore means seeding every sibling its
	// own `!include` lines name, or the first boot fails on a missing
	// file. .HA_VERSION is included because its absence is read as
	// version 0.7.7 and runs years of migrations against a directory
	// created seconds ago.
	for _, want := range []string{"automations.yaml", "scripts.yaml", "scenes.yaml", ".HA_VERSION"} {
		if _, ok := seeds[want]; !ok {
			t.Errorf("configuration.yaml is seeded but %s is not: Home Assistant will not create it, and the first boot fails", want)
		}
	}
}

// TestEveryPathParamOffersAPicker: a param whose value is a folder must
// declare `source: directory`, so the form renders a picker instead of a
// blank box.
//
// A free-text path is the field that let a corrupted default through
// unnoticed — the redactor turned a package's own default into
// "/[REDACTED]", the API served it back as the value to submit, and the
// operator accepted what they were shown, because a blank box gives you
// nothing to check a value against. Tagging the nineteen that existed
// fixed those nineteen; this is what keeps it true of the twentieth.
//
// Matched on the name ending in _path, which is the convention every
// shipped package already follows. A folder param named something else
// is not caught here, and renaming the convention is the thing that
// would have to change.
func TestEveryPathParamOffersAPicker(t *testing.T) {
	checked := 0
	for _, dir := range packageDirs(t) {
		pkg, err := pkgspec.Load(filepath.Join(dir, "package.yaml"))
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		for _, p := range pkg.Params {
			if !strings.HasSuffix(p.Name, "_path") {
				continue
			}
			checked++
			if p.Source != "directory" {
				t.Errorf("%s: param %q is a folder but declares source %q — it should be `source: directory`, "+
					"so the form offers a picker instead of a box the operator cannot check a value against",
					filepath.Base(dir), p.Name, p.Source)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no path params found at all — the walk is broken, not the packages")
	}
}

// TestSiblingProfilesDoNotCollide: two packages providing the same
// capability may now BOTH be installed (ADR-W029 A7), so any resource
// name they share has to mean the same thing in both.
//
// Sharing a name is not itself wrong. books-calibre-web and books-kavita
// both declare Secret/restic-books and Volume/books-backup-repo with
// identical specs — one repository for the household's books, whichever
// app is reading them — and installing both simply converges on the same
// resource twice.
//
// Declaring the same name with a DIFFERENT spec is wrong, and was the
// one real obstacle to running two profiles side by side: both book
// packages declared BackupPlan/books, differing only in which config
// volume they backed up, so whichever installed second silently redefined
// the first one's backup. Same for media-jellyfin and media-plex.
//
// The fix is to name such a resource for the profile rather than the
// capability. This test is what keeps the next author from reaching for
// the capability name out of habit.
func TestSiblingProfilesDoNotCollide(t *testing.T) {
	byCapability := map[string]map[string]shipped{}
	for name, sp := range walkShipped(t) {
		cap := sp.pkg.Metadata.Capability
		if byCapability[cap] == nil {
			byCapability[cap] = map[string]shipped{}
		}
		byCapability[cap][name] = sp
	}

	type origin struct{ pkg, spec string }
	compared := 0
	for cap, siblings := range byCapability {
		if len(siblings) < 2 {
			continue
		}
		// An exclusive capability replaces its sibling rather than
		// running beside it, so a shared name can never be live twice.
		exclusive := false
		for _, sp := range siblings {
			if sp.pkg.Metadata.Exclusive {
				exclusive = true
			}
		}
		if exclusive {
			continue
		}
		t.Run(cap, func(t *testing.T) {
			compared++
			seen := map[string]origin{}
			for _, pkgName := range sortedNames(siblings) {
				for _, r := range siblings[pkgName].rendered {
					key := string(r.Kind) + "/" + r.Metadata.Name
					// Compare the SPEC, not the whole resource: metadata
					// carries the package name, which differs by
					// construction and is not a disagreement.
					body, err := resource.Marshal(resource.Resource{
						ApiVersion: r.ApiVersion, Kind: r.Kind,
						Metadata:  resource.Metadata{Name: r.Metadata.Name, Owner: r.Metadata.Owner, Node: r.Metadata.Node},
						Container: r.Container, Route: r.Route, Volume: r.Volume, Secret: r.Secret,
						ConfigFile: r.ConfigFile, BackupPlan: r.BackupPlan, OidcClient: r.OidcClient,
						SeedFile: r.SeedFile, Job: r.Job, LdapGroup: r.LdapGroup, Ingress: r.Ingress,
						DnsRecord: r.DnsRecord,
					})
					if err != nil {
						t.Fatalf("%s: marshalling %s: %v", pkgName, key, err)
					}
					prev, clash := seen[key]
					if clash && prev.spec != string(body) {
						t.Errorf("%s and %s both declare %s with different specs.\n"+
							"Two profiles of capability %q can now run side by side, so a shared name must mean "+
							"the same thing in both. Name it for the profile instead (e.g. %s-<profile>).",
							prev.pkg, pkgName, key, cap, cap)
						continue
					}
					if !clash {
						seen[key] = origin{pkg: pkgName, spec: string(body)}
					}
				}
			}
		})
	}
	if compared == 0 {
		t.Fatal("no plural capability had two profiles to compare — this test walked nothing")
	}
}

func sortedNames(m map[string]shipped) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestAPasswordSetOnADatabaseIsGivenToItsApp: when a package creates a
// database with a generated password, something else in that package has
// to be told the same secret.
//
// documents-paperless set POSTGRES_PASSWORD on its postgres container
// and gave the app only PAPERLESS_DBHOST. Paperless fell back to its own
// defaults — user "paperless", password "paperless" — while postgres
// held the generated value, so every start ended in:
//
//	FATAL: password authentication failed for user "paperless"
//
// It lints clean, reconciles clean, and cannot work. The reference is
// declared, mounted and materialized correctly; it is simply only on one
// side of a conversation between two containers.
//
// Checked per package rather than per resource, because a single
// container can say nothing about whether anyone is listening.
func TestAPasswordSetOnADatabaseIsGivenToItsApp(t *testing.T) {
	// The env names that CREATE a database credential, by image family.
	creators := map[string]bool{
		"POSTGRES_PASSWORD":          true,
		"MYSQL_PASSWORD":             true,
		"MARIADB_PASSWORD":           true,
		"MONGO_INITDB_ROOT_PASSWORD": true,
	}
	checked := 0
	for name, rendered := range renderShippedPackages(t) {
		t.Run(name, func(t *testing.T) {
			// Every secret reference this package's containers make.
			refs := map[string]int{}
			created := map[string]string{} // secret -> the container that creates it
			for _, r := range rendered {
				if r.Kind != resource.KindContainer || r.Container == nil {
					continue
				}
				for k, v := range r.Container.Env {
					ref, ok := strings.CutPrefix(strings.TrimSpace(v), "secret/")
					if !ok {
						continue
					}
					refs[ref]++
					if creators[k] {
						created[ref] = r.Metadata.Name
					}
				}
			}
			for ref, by := range created {
				checked++
				if refs[ref] < 2 {
					t.Errorf("Container/%s creates a database with secret/%s and nothing else in this package "+
						"references it. The app will fall back to its own default credentials and fail to "+
						"authenticate; both sides of the connection need the same reference.", by, ref)
				}
			}
		})
	}
	if checked == 0 {
		t.Fatal("no package created a database credential — this test walked nothing")
	}
}

// TestEveryRequiredParamCanBeAnswered: a required param the operator
// must invent needs a description, because the install form renders a
// name and a description and cannot see this repository.
//
// ParamDecl.Description exists for exactly this — "admin_domain *" is an
// unanswerable question, and the explanation was sitting three lines
// away in a YAML comment. Two packages had drifted back: home-automation
// asked for node, intake_url and people with no description on any of
// them, and ingress asked which address its admin surface should bind
// with the reasoning in a comment above.
//
// A param with a `source` is exempt: the deployment answers it, so the
// form offers a value rather than an empty box.
func TestEveryRequiredParamCanBeAnswered(t *testing.T) {
	checked := 0
	for name, sp := range walkShipped(t) {
		t.Run(name, func(t *testing.T) {
			for _, p := range sp.pkg.Params {
				if !p.Required || p.Source != "" {
					continue
				}
				checked++
				if strings.TrimSpace(p.Description) == "" {
					t.Errorf("required param %q has no description. The install form shows its NAME and "+
						"nothing else, so an operator sees %q and an empty box; a comment above it in "+
						"package.yaml is not something the form can read.", p.Name, p.Name+" *")
				}
			}
		})
	}
	if checked == 0 {
		t.Fatal("no required param needed a description — this test walked nothing")
	}
}
