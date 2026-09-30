package pkgtool_test

// Every capability in the shipped catalog must have an answer on the
// household surface: either it is the operator's, or it is filed under
// something a member would recognise.
//
// This test exists because the first version of portal's tables was
// written from the capability names I expected rather than the ones the
// packages declare. "indexers" and "monitoring" were denied; the real
// names are "movie-automation" and "host-monitoring", so Radarr and
// Beszel were offered to the household, and nineteen capabilities fell
// through a default into "Everything else" — all of it silent, because
// a missing map entry is a zero value and not an error.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
)

// shippedCapabilities reads every capability the catalog declares.
func shippedCapabilities(t *testing.T) map[string]string {
	t.Helper()
	root, err := filepath.Abs("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name(), "package.yaml"))
		if err != nil {
			continue
		}
		p, err := pkgspec.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if p.Metadata.Capability != "" {
			out[p.Metadata.Capability] = e.Name()
		}
	}
	if len(out) < 20 {
		t.Fatalf("only %d capabilities found; the catalog did not load", len(out))
	}
	return out
}

// A capability is either the operator's or it is categorised. Falling
// into "other" by default is how nineteen of them ended up in
// "Everything else" without anybody deciding that.
func TestEveryCapabilityIsEitherOperatorOnlyOrCategorised(t *testing.T) {
	for capability, pkg := range shippedCapabilities(t) {
		if !portal.Requestable(capability) {
			continue
		}
		if portal.CategoryFor(capability) == "other" && !portal.DeliberatelyOther(capability) {
			t.Errorf("capability %q (%s) is offered to households but falls into Everything else; "+
				"put it in a category or deny it", capability, pkg)
		}
	}
}

// The operator's capabilities are named with the spellings the packages
// actually use, not the ones a design document used.
func TestTheOperatorOnlyNamesExist(t *testing.T) {
	shipped := shippedCapabilities(t)
	for _, capability := range portal.OperatorOnly() {
		if _, ok := shipped[capability]; !ok {
			t.Errorf("operatorOnly names %q, which no package declares — "+
				"a denial that matches nothing protects nothing", capability)
		}
	}
}

// The things a household must never be offered, spelled out, so that
// renaming a capability cannot quietly open one of them up.
func TestTheHouseholdIsNeverOfferedTheOperatorsCapabilities(t *testing.T) {
	shipped := shippedCapabilities(t)
	for capability, pkg := range shipped {
		switch pkg {
		case "identity", "ingress", "ingress-tailnet", "ingress-tailnet-ts", "ingress-tailnet-public",
			"dns-adguard", "dns-pihole", "vpn-gluetun", "mqtt-mosquitto", "zigbee-zigbee2mqtt",
			"metrics-grafana", "metrics-prometheus", "monitoring", "monitoring-beszel",
			"indexers-prowlarr", "downloads-qbittorrent", "subtitles-bazarr",
			"movies-radarr", "tv-sonarr", "music-lidarr", "captcha-flaresolverr",
			"wildstone-console", "notifications", "dashboard", "dashboard-homarr",
			"filedrop-wildstone", "assistant-wildstone", "homepage-wildstone",
			"chat-matrix-admin", "chat-telegram-admin", "research-ddgs":
			if portal.Requestable(capability) {
				t.Errorf("%s (capability %q) is requestable from the household surface", pkg, capability)
			}
		}
	}
}
