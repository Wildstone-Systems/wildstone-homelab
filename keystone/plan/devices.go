package plan

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// Physical-device ownership (#435). A serial radio or a Bluetooth adapter
// answers exactly one process: two containers handed the same Z-Wave
// stick both start, both open the port, and each reads half of the
// other's frames. Nothing fails loud — the network just goes flaky — so
// the only good moment to refuse is before the plan exists.
//
// The same stick has several names: its /dev/serial/by-id alias and the
// /dev/ttyUSB* node the alias resolves to. Only the node knows which
// alias points where, so the node's radio inventory (ListRadios, protocol
// minor 25, #434) is what folds the names into one device. When the node
// cannot be asked — offline, or an agent older than minor 25 — the check
// falls back to exact cleaned paths: it still catches the same path
// written twice, and it never invents a conflict between two names it
// cannot prove are one device.
//
// The exclusive set is deliberately narrow. /dev/dri render nodes and
// /dev/bus/usb are shared by design (every transcoder wants the GPU), so
// only serial device paths, and whatever path the inventory names as a
// radio, count as ownership.

// RadioInventoryProtocolMinor is the Wardstone protocol minor that first
// carried ListRadios (#434). An older agent answers "unknown operation".
const RadioInventoryProtocolMinor = 25

// Radio is the slice of one ListRadios entry ownership needs: the
// inventory's stable identity and every host path that reaches the
// device (its devPath and its /dev/serial/by-id aliases).
type Radio struct {
	ID    string
	Paths []string
}

// exclusiveDevicePatterns are the host paths that are ownership claims
// even when the node's inventory is unavailable: serial ttys and their
// udev aliases. Kept in step with the serial entries of the device
// allowlist (resource.DeviceAllowed).
var exclusiveDevicePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^/dev/tty(USB|ACM|AMA)[0-9]+$`),
	regexp.MustCompile(`^/dev/serial/by-(id|path)/[^/]+$`),
}

// DeviceClaim is one Container's claim on one physical device.
type DeviceClaim struct {
	Node string `json:"node"`
	// Device is the identity the claim was folded under: the inventory's
	// radio id when the node resolved the path, else "path:<cleaned path>".
	Device string `json:"device"`
	// RadioID is the inventory id, empty when the path was not resolved
	// against an inventory (offline or older node, or an unlisted path).
	RadioID   string `json:"radioId,omitempty"`
	Container string `json:"container"`
	// Package and Owner are the Container's provenance (Metadata.Package,
	// Metadata.Owner); either is empty for a hand-written resource.
	Package string `json:"package,omitempty"`
	Owner   string `json:"owner,omitempty"`
	// Source is the host path as the Container spec wrote it.
	Source string `json:"source"`
}

// who names a claim's container and its owning package for a message.
func (c DeviceClaim) who() string {
	switch {
	case c.Package != "":
		return fmt.Sprintf("Container/%s (package %s)", c.Container, c.Package)
	case c.Owner != "":
		return fmt.Sprintf("Container/%s (%s)", c.Container, c.Owner)
	default:
		return fmt.Sprintf("Container/%s (no package)", c.Container)
	}
}

// DeviceClaims lists every exclusive device claim among the desired
// Containers, sorted by node, device, then container. radios maps a node
// to its inventory; a node absent from the map (or mapped to nil) is
// treated as unknown and its claims fold only on exact cleaned paths. A
// container naming one device twice (alias and tty) yields one claim.
func DeviceClaims(desired []resource.Resource, radios map[string][]Radio) []DeviceClaim {
	byPath := map[string]map[string]string{} // node -> cleaned path -> radio id
	for node, rs := range radios {
		m := map[string]string{}
		for _, r := range rs {
			if r.ID == "" {
				continue
			}
			for _, p := range r.Paths {
				if p != "" {
					m[path.Clean(p)] = r.ID
				}
			}
		}
		byPath[node] = m
	}
	var out []DeviceClaim
	for _, r := range desired {
		if r.Kind != resource.KindContainer || r.Container == nil {
			continue
		}
		seen := map[string]bool{}
		for _, d := range r.Container.Devices {
			if d.Source == "" {
				continue
			}
			src := path.Clean(d.Source)
			c := DeviceClaim{
				Node: r.Metadata.Node, Container: r.Metadata.Name,
				Package: r.Metadata.Package, Owner: r.Metadata.Owner, Source: d.Source,
			}
			if id, ok := byPath[r.Metadata.Node][src]; ok {
				c.Device, c.RadioID = id, id
			} else if exclusivePath(src) {
				c.Device = "path:" + src
			} else {
				continue
			}
			if seen[c.Device] {
				continue
			}
			seen[c.Device] = true
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		if a.Device != b.Device {
			return a.Device < b.Device
		}
		return a.Container < b.Container
	})
	return out
}

func exclusivePath(p string) bool {
	for _, re := range exclusiveDevicePatterns {
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// RadioOwners maps each radio id on one node to the desired Containers
// that claim it — "who owns this stick" for the radio picker and the
// aggregate home-automation view. inventory is that node's ListRadios
// result; nil means the node could not be asked, in which case only
// exact serial paths are reported, keyed "path:<cleaned path>".
func RadioOwners(desired []resource.Resource, node string, inventory []Radio) map[string][]DeviceClaim {
	var radios map[string][]Radio
	if inventory != nil {
		radios = map[string][]Radio{node: inventory}
	}
	out := map[string][]DeviceClaim{}
	for _, c := range DeviceClaims(desired, radios) {
		if c.Node == node {
			out[c.Device] = append(out[c.Device], c)
		}
	}
	return out
}

// DeviceConflict is one physical device claimed by more than one
// Container on the same node. Claims are sorted by container name.
type DeviceConflict struct {
	Node   string
	Device string
	Claims []DeviceClaim
}

// DeviceConflicts returns every device two or more desired Containers on
// the same node claim.
func DeviceConflicts(desired []resource.Resource, radios map[string][]Radio) []DeviceConflict {
	var out []DeviceConflict
	claims := DeviceClaims(desired, radios)
	for i := 0; i < len(claims); {
		j := i + 1
		for j < len(claims) && claims[j].Node == claims[i].Node && claims[j].Device == claims[i].Device {
			j++
		}
		if j-i > 1 {
			out = append(out, DeviceConflict{Node: claims[i].Node, Device: claims[i].Device, Claims: claims[i:j]})
		}
		i = j
	}
	return out
}

// Error is the actionable refusal for one conflict: both Containers, their
// packages, the paths each wrote, and what to do about it.
func (c DeviceConflict) Error() string {
	parts := make([]string, 0, len(c.Claims))
	for _, cl := range c.Claims {
		parts = append(parts, fmt.Sprintf("%s via %s", cl.who(), cl.Source))
	}
	what := "device " + strings.TrimPrefix(c.Device, "path:")
	if c.Claims[0].RadioID != "" {
		what = "radio " + c.Device
	}
	return fmt.Sprintf("on node %s, %s is claimed by %s; a radio answers one program at a time — "+
		"remove the device from all but one of them, or plug in a second adapter and give each its own",
		c.Node, what, strings.Join(parts, " and "))
}

// DuplicateDeviceOwnership refuses a desired state in which two
// Containers on one node claim the same physical device, reporting only
// conflicts that involve a Container in touched (by name) — a duplicate
// already in the Statefile is not the proposer's to fix, and must not
// block every unrelated proposal. A nil touched checks every Container.
func DuplicateDeviceOwnership(desired []resource.Resource, radios map[string][]Radio, touched map[string]bool) error {
	var msgs []string
	for _, c := range DeviceConflicts(desired, radios) {
		relevant := touched == nil
		for _, cl := range c.Claims {
			if touched[cl.Container] {
				relevant = true
			}
		}
		if relevant {
			msgs = append(msgs, c.Error())
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("duplicate physical-device ownership: %s", strings.Join(msgs, "; "))
}
