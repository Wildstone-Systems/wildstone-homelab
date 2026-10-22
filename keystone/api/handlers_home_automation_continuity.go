package api

// Home-automation continuity (#451): for each installed component, what
// its data is, how it is backed up, what replacing or moving its radio
// keeps, and what removing it stops and keeps. The Console's composed
// status page (S20) explains a radio swap or a removal from this answer
// rather than from words of its own.
//
// It is a read beside GET /api/v1/home-automation, not a field on it:
// the same evaluation, with the facts that only matter once something is
// installed. Volumes, their BackupPlans and each plan's class come from
// the Statefile — what the package actually rendered — so a package
// that moves its data or changes its backup class is described as it
// is. What a protocol keeps WHERE (on the stick, or on the machine) is
// a fact about the protocol and the packages' documented behaviour, and
// lives in the table below.
//
// Nothing here proposes anything. A radio replacement is an ordinary
// capability reconfigure (POST /api/v1/capabilities with the installed
// params and the radio param below); a removal is POST
// /api/v1/capabilities/disable, whose dry run is the authority on what
// is deleted. Volumes and seeded files are never deleted (ADR-W004 A4).

import (
	"net/http"
	"sort"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// Radio outcomes, a closed vocabulary.
const (
	// haKept: paired devices keep working with no restore.
	haKept = "kept"
	// haRestore: kept only by restoring from what Wildstone backs up (or
	// what the app itself takes over from its own backup).
	haRestore = "restore"
)

type haContinuityView struct {
	GeneratedAt time.Time      `json:"generatedAt"`
	Components  []haContinuity `json:"components"`
}

type haContinuity struct {
	Component string `json:"component"`
	Package   string `json:"package,omitempty"`
	Node      string `json:"node,omitempty"`
	// Holds is what the component's data is, in household words.
	Holds   string             `json:"holds,omitempty"`
	Data    []haDataVolume     `json:"data"`
	Radio   *haRadioContinuity `json:"radio,omitempty"`
	Removal haRemoval          `json:"removal"`
}

type haDataVolume struct {
	Volume string `json:"volume"`
	// Repository marks a volume that is itself a local backup copy.
	Repository bool   `json:"repository,omitempty"`
	BackupPlan string `json:"backupPlan,omitempty"`
	// Class is the covering BackupPlan's class: critical, important or
	// rebuildable.
	Class    string `json:"class,omitempty"`
	Schedule string `json:"schedule,omitempty"`
}

type haRadioOutcome struct {
	Outcome string   `json:"outcome"` // kept | restore
	Summary string   `json:"summary"`
	Steps   []string `json:"steps,omitempty"`
	// Otherwise is what happens when the restore is not possible.
	Otherwise string `json:"otherwise,omitempty"`
}

type haRadioContinuity struct {
	// NetworkOn is where the network (keys, pairings, node table) lives:
	// "machine" (the radio holds nothing) or "radio_and_machine".
	NetworkOn string         `json:"networkOn"`
	Moved     haRadioOutcome `json:"moved"`
	Replaced  haRadioOutcome `json:"replaced"`
	// Param is the package param that takes the radio's stable path, and
	// Replacements the free suitable radios with the path each would be
	// given — what a reconfigure sends.
	Param        string          `json:"param,omitempty"`
	Replacements []haReplacement `json:"replacements"`
}

type haReplacement struct {
	RadioID string `json:"radioId"`
	Path    string `json:"path"`
}

type haRemoval struct {
	// Setting: the component is a setting of another package (Bluetooth
	// is Home Assistant's), so it is turned off by reconfiguring that
	// package, not removed.
	Setting bool `json:"setting,omitempty"`
	// Stops are the containers removal stops; Keeps the volumes and
	// seeded files it leaves in place.
	Stops []string `json:"stops"`
	Keeps []string `json:"keeps"`
	// Dependents are installed components that need this one.
	Dependents  []string `json:"dependents"`
	Consequence string   `json:"consequence"`
}

// haContinuitySpec is what a protocol keeps where, from each package's
// documented behaviour (packages/*/package.yaml).
type haContinuitySpec struct {
	Holds       string
	Consequence string
	NetworkOn   string
	Moved       haRadioOutcome
	Replaced    haRadioOutcome
}

var haContinuitySpecs = map[string]haContinuitySpec{
	haMQTT: {
		Holds:       "the broker's settings and retained messages",
		Consequence: "Everything that talks through MQTT, Zigbee devices among them, stops reaching Home Assistant until a broker is back.",
	},
	haHomeAssistant: {
		Holds:       "your automations, dashboards, history and integrations",
		Consequence: "Home Assistant stops, and so do its automations. Its configuration stays on the machine, and reinstalling picks it back up.",
	},
	haExternalHA: {
		Consequence: "The household's Butlers stop seeing your Home Assistant. Home Assistant itself is not touched.",
	},
	haZigbee: {
		Holds:       "the Zigbee network key and every paired device",
		Consequence: "Zigbee devices stop answering Home Assistant. They stay paired to the network kept on this machine, and reinstalling picks them back up.",
		NetworkOn:   "radio_and_machine",
		Moved: haRadioOutcome{Outcome: haKept,
			Summary: "The same stick in another USB port keeps every device: the network is on the stick and in Zigbee2MQTT's data."},
		Replaced: haRadioOutcome{Outcome: haRestore,
			Summary: "A new stick of the same chip family takes the network over from Zigbee2MQTT's own coordinator backup when it first starts, so devices keep working; a few battery devices may need waking.",
			Steps: []string{
				"Unplug the old stick and plug in the new one.",
				"Choose the new stick here and propose the change; approve it on Plans.",
				"If the new stick is a different chip family, set its adapter type in the package form first.",
			},
			Otherwise: "A stick of a different chip family starts a new network: every Zigbee device has to be paired again."},
	},
	haZWave: {
		Holds:       "the Z-Wave security keys, the node table and the nightly controller (NVM) backups",
		Consequence: "Z-Wave devices stop answering Home Assistant. The keys and node table stay on this machine, and reinstalling picks them back up.",
		NetworkOn:   "radio_and_machine",
		Moved: haRadioOutcome{Outcome: haKept,
			Summary: "The same stick in another USB port keeps every device: most of the network lives on the stick, and the keys stay on this machine."},
		Replaced: haRadioOutcome{Outcome: haRestore,
			Summary: "A new stick starts empty. Restoring the controller (NVM) backup Z-Wave JS UI takes every night onto it keeps every device.",
			Steps: []string{
				"Plug in the new stick, choose it here and propose the change; approve it on Plans.",
				"Open Z-Wave JS UI: Advanced actions → NVM Management → restore the newest NVM backup onto the new stick.",
				"Home Assistant's Z-Wave integration keeps working; nothing changes there.",
			},
			Otherwise: "Without an NVM backup every device has to be excluded and included again."},
	},
	haMatter: {
		Holds:       "the Matter fabric: its certificate authority and every commissioned device's credentials",
		Consequence: "Home Assistant can no longer control Matter devices. They stay commissioned to the fabric kept on this machine, and reinstalling picks them back up.",
	},
	haThread: {
		Holds:       "the Thread network's dataset, network key and frame counters",
		Consequence: "Thread devices lose their border router here. The Thread network stays on this machine, and reinstalling picks it back up.",
		NetworkOn:   "machine",
		Moved: haRadioOutcome{Outcome: haKept,
			Summary: "The same dongle in another USB port keeps the network: the dongle holds nothing, and the network is on this machine."},
		Replaced: haRadioOutcome{Outcome: haKept,
			Summary: "A new dongle joins the same Thread network when the border router starts: the network lives on this machine, not on the dongle, so devices keep working.",
			Steps: []string{
				"Flash the new dongle with OpenThread RCP firmware before plugging it in.",
				"Choose it here and propose the change; approve it on Plans.",
			}},
	},
	haBluetooth: {
		Consequence: "Turning Bluetooth off is a change to Home Assistant's settings; nothing is deleted and nothing else stops.",
	},
}

func (s *Server) homeAutomationContinuity(r *http.Request, _ principal.Identity) (any, int, error) {
	b, view, code, err := s.evaluateHomeAutomation(r.Context(), haGoals{requested: map[string]bool{}, modeHint: map[string]string{}})
	if err != nil {
		return nil, code, err
	}
	out := haContinuityView{GeneratedAt: view.GeneratedAt, Components: []haContinuity{}}
	for _, vc := range view.Components {
		c := b.comps[vc.ID]
		if c == nil || !c.installed {
			continue
		}
		out.Components = append(out.Components, b.continuity(c, view.Components))
	}
	return out, http.StatusOK, nil
}

func (b *haBuild) continuity(c *haComponent, all []haComponent) haContinuity {
	spec := haContinuitySpecs[c.ID]
	out := haContinuity{Component: c.ID, Package: c.Package, Node: c.Node, Holds: spec.Holds, Data: []haDataVolume{},
		Removal: haRemoval{Stops: []string{}, Keeps: []string{}, Dependents: []string{}, Consequence: spec.Consequence}}
	for _, d := range all {
		if d.Selection == "installed" && d.ID != c.ID && haContains(d.Requires, c.ID) {
			out.Removal.Dependents = append(out.Removal.Dependents, d.ID)
		}
	}
	if len(c.spec.ModeParams) > 0 {
		out.Removal.Setting = true
		return out
	}

	owner := "capability/" + c.spec.Capability
	mine := func(r resource.Resource) bool {
		return r.Metadata.Owner == owner && (c.pkg == nil || r.Metadata.Package == "" || r.Metadata.Package == c.pkg.Metadata.Name)
	}
	repos := map[string]bool{}
	covering := map[string]*resource.Resource{}
	for i, r := range b.current {
		if r.Kind == resource.KindBackupPlan && r.BackupPlan != nil {
			repos[r.BackupPlan.Repository] = true
			for _, v := range r.BackupPlan.Volumes {
				if covering[v] == nil {
					covering[v] = &b.current[i]
				}
			}
		}
	}
	for _, r := range b.current {
		if !mine(r) {
			continue
		}
		switch r.Kind {
		case resource.KindContainer:
			out.Removal.Stops = append(out.Removal.Stops, r.Metadata.Name)
		case resource.KindSeedFile:
			out.Removal.Keeps = append(out.Removal.Keeps, r.Metadata.Name)
		case resource.KindVolume:
			out.Removal.Keeps = append(out.Removal.Keeps, r.Metadata.Name)
			dv := haDataVolume{Volume: r.Metadata.Name, Repository: repos[r.Metadata.Name]}
			if bp := covering[r.Metadata.Name]; bp != nil {
				dv.BackupPlan, dv.Class, dv.Schedule = bp.Metadata.Name, bp.BackupPlan.Class, bp.BackupPlan.Schedule
			}
			out.Data = append(out.Data, dv)
		}
	}
	sort.Strings(out.Removal.Stops)
	sort.Strings(out.Removal.Keeps)
	sort.Slice(out.Data, func(i, j int) bool {
		if out.Data[i].Repository != out.Data[j].Repository {
			return !out.Data[i].Repository
		}
		return out.Data[i].Volume < out.Data[j].Volume
	})

	if c.spec.RadioClaims && spec.NetworkOn != "" {
		rc := &haRadioContinuity{NetworkOn: spec.NetworkOn, Moved: spec.Moved, Replaced: spec.Replaced, Replacements: []haReplacement{}}
		for _, p := range haRadioParams {
			if declaresParam(c.pkg, p) {
				rc.Param = p
				break
			}
		}
		if n := b.nodes[c.Node]; rc.Param != "" && n != nil && n.RadioProbe != nil && n.RadioProbe.Probe == "ok" {
			owners := plan.RadioOwners(b.current, n.ID, n.inventory)
			for _, id := range b.candidates(c.spec, n, owners, true) {
				for _, r := range n.RadioProbe.Radios {
					if r.ID != id {
						continue
					}
					path := r.DevPath
					if len(r.ByIDPaths) > 0 {
						path = r.ByIDPaths[0]
					}
					if path != "" {
						rc.Replacements = append(rc.Replacements, haReplacement{RadioID: id, Path: path})
					}
				}
			}
		}
		out.Radio = rc
	}
	return out
}
