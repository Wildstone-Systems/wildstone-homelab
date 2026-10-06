package pkgtool

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// Every home-automation controller keeps identity-bearing state: the keys
// and network parameters its paired devices trust. Lose that volume and
// the household re-pairs, re-includes or re-commissions every device, so
// each controller package must back it up, daily, with the class that
// says how much it matters (docs/recovery.md, "Home automation
// controllers", is the human copy of this table; #453).
//
// The check is on what the package RENDERS, not its YAML: the volume must
// be the one mounted at the path the app writes its identity to, in the
// container that runs the app, and the BackupPlan must cover exactly that
// volume. A plan that names the right volume while the container mounts
// another is a backup of nothing.
func TestEveryControllerBacksUpItsIdentity(t *testing.T) {
	controllers := []struct {
		pkg, container, volume, target, plan, class, why string
	}{
		{"zigbee-zigbee2mqtt", "zigbee2mqtt", "zigbee2mqtt-data", "/app/data", "zigbee", "critical",
			"the network key, PAN ids and every pairing"},
		{"zwave-zwave-js-ui", "zwave-js-ui", "zwave-js-ui-store", "/usr/src/app/store", "zwave", "critical",
			"the S0/S2 security keys every secure device is bound to"},
		{"thread-otbr", "otbr", "otbr-data", "/var/lib/thread", "thread", "critical",
			"the Thread network key and active dataset"},
		{"matter-matterjs", "matter-server", "matter-data", "/data", "matter", "critical",
			"the Matter fabric's root CA and every node's credentials"},
		// Home Assistant's config is the household's automations, users and
		// integration entries: painful to rebuild, but no device is paired
		// to it (the controllers above hold the radio identities).
		{"home-assistant", "home-assistant", "home-assistant-config", "/config", "home-assistant", "important",
			"automations, users and integration entries"},
	}
	all := renderShippedPackages(t)
	for _, c := range controllers {
		t.Run(c.pkg, func(t *testing.T) {
			rendered, ok := all[c.pkg]
			if !ok {
				t.Fatalf("package %s is not shipped", c.pkg)
			}
			var mounted bool
			for _, m := range findContainer(t, rendered, c.container).Mounts {
				if m.Source == c.volume && m.Target == c.target && !m.ReadOnly {
					mounted = true
				}
			}
			if !mounted {
				t.Errorf("Container/%s does not mount %s writable at %s, where it keeps %s", c.container, c.volume, c.target, c.why)
			}
			var plans []*resource.BackupPlanSpec
			for _, r := range rendered {
				if r.Kind != resource.KindBackupPlan || r.BackupPlan == nil {
					continue
				}
				plans = append(plans, r.BackupPlan)
				if r.Metadata.Name != c.plan {
					t.Errorf("BackupPlan is named %q, want %q (docs/recovery.md and the runbook restore it by that name)", r.Metadata.Name, c.plan)
				}
			}
			if len(plans) != 1 {
				t.Fatalf("want exactly one BackupPlan covering %s (%s), got %d", c.volume, c.why, len(plans))
			}
			bp := plans[0]
			if len(bp.Volumes) != 1 || bp.Volumes[0] != c.volume {
				t.Errorf("BackupPlan volumes = %v, want [%s]: it holds %s", bp.Volumes, c.volume, c.why)
			}
			if bp.Class != c.class {
				t.Errorf("BackupPlan class = %q, want %q: it holds %s", bp.Class, c.class, c.why)
			}
			if bp.Schedule != "daily" {
				t.Errorf("BackupPlan schedule = %q, want daily", bp.Schedule)
			}
			if bp.Repository == "" || bp.Repository == c.volume {
				t.Errorf("BackupPlan repository = %q: it needs its own repository volume", bp.Repository)
			}
		})
	}
}

// The broker is the one home-automation package with no identity to back
// up: its passwords are Wildstone secrets (restored with the node's
// secrets, and rendered into the password file from them), and the data
// volume holds only retained messages and persistence that the clients
// republish. If it ever grows a BackupPlan or a hand-kept credential, the
// recovery documentation for it is wrong and must change with it.
func TestBrokerKeepsNoIdentityOfItsOwn(t *testing.T) {
	rendered, ok := renderShippedPackages(t)["mqtt-mosquitto"]
	if !ok {
		t.Fatal("mqtt-mosquitto is not shipped")
	}
	for _, r := range rendered {
		if r.Kind == resource.KindBackupPlan {
			t.Errorf("mqtt-mosquitto renders BackupPlan/%s: update docs/recovery.md's broker row (#453)", r.Metadata.Name)
		}
		if r.Kind == resource.KindSeedFile {
			t.Errorf("mqtt-mosquitto renders SeedFile/%s: app-owned state the recovery docs say it does not have", r.Metadata.Name)
		}
	}
}
