package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/catalog"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

const (
	zwaveAlias = "/dev/serial/by-id/usb-Zooz_ZST10_700-if00"
	zwaveTTY   = "/dev/ttyUSB0"
	zwaveID    = "usb:0658:0200:0001"
)

func deviceContainerResource(name, pkg, node string, sources ...string) resource.Resource {
	spec := &resource.ContainerSpec{Image: "ghcr.io/example/app@" + testDigest}
	for _, s := range sources {
		spec.Devices = append(spec.Devices, resource.DeviceMapping{Source: s})
	}
	return resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindContainer,
		Metadata:  resource.Metadata{Name: name, Owner: "capability/" + pkg, Package: pkg, Node: node},
		Container: spec,
	}
}

func deviceContainerDoc(t *testing.T, name, pkg, node string, sources ...string) string {
	t.Helper()
	data, err := resource.Marshal(deviceContainerResource(name, pkg, node, sources...))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(data)
}

// seedDesired commits resources straight to the Statefile, as an earlier
// approved plan would have.
func seedDesired(t *testing.T, h *harness, rs ...resource.Resource) {
	t.Helper()
	for _, r := range rs {
		if err := h.s.State.Put(r); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := h.s.State.Commit("test", "seed", "seed"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// radioAgent answers ListRadios for n1 with the Z-Wave stick, and fails
// every other node as though it were offline.
func radioAgent(t *testing.T, calls *int) applierFunc {
	return func(_ context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
		if _, ok := op.GetOp().(*wsv1.Operation_ListRadios); !ok {
			t.Fatalf("unexpected op %v", op)
		}
		*calls++
		if node != "n1" {
			return nil, errors.New("node not connected")
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_ListRadios{ListRadios: &wsv1.ListRadiosResult{
			PlatformSupported: true,
			Radios: []*wsv1.Radio{{
				Id: zwaveID, Kind: wsv1.RadioKind_RADIO_KIND_ZWAVE, KernelName: "ttyUSB0",
				DevPath: zwaveTTY, ByIdPaths: []string{zwaveAlias},
			}},
		}}}, nil
	}
}

// TestProposalRejectsDuplicateRadioOwnership drives #435 through the one
// proposal pipeline: a second Container claiming a radio another already
// owns is refused before a plan exists, naming both Containers and
// packages, while shareable devices and other nodes pass.
func TestProposalRejectsDuplicateRadioOwnership(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing resource.Resource
		propose  func(t *testing.T) string
		agent    bool
		wantCode int
		wantMsg  []string
	}{
		{
			name:     "exact duplicate, no agent plane",
			existing: deviceContainerResource("zwave-js-ui", "zwave-js-ui", "n1", zwaveAlias),
			propose: func(t *testing.T) string {
				return deviceContainerDoc(t, "homeassistant", "home-assistant", "n1", zwaveAlias)
			},
			wantCode: http.StatusConflict,
			wantMsg:  []string{"Container/zwave-js-ui (package zwave-js-ui)", "Container/homeassistant (package home-assistant)", "node n1"},
		},
		{
			name:     "alias and resolved tty via inventory",
			existing: deviceContainerResource("zwave-js-ui", "zwave-js-ui", "n1", zwaveAlias),
			propose: func(t *testing.T) string {
				return deviceContainerDoc(t, "homeassistant", "home-assistant", "n1", zwaveTTY)
			},
			agent:    true,
			wantCode: http.StatusConflict,
			wantMsg:  []string{"radio " + zwaveID, zwaveAlias, zwaveTTY},
		},
		{
			name:     "offline node falls back to exact paths",
			existing: deviceContainerResource("zwave-js-ui", "zwave-js-ui", "n2", zwaveAlias),
			propose: func(t *testing.T) string {
				return deviceContainerDoc(t, "homeassistant", "home-assistant", "n2", zwaveTTY)
			},
			agent:    true,
			wantCode: http.StatusCreated,
		},
		{
			name:     "offline node still refuses an exact duplicate",
			existing: deviceContainerResource("zwave-js-ui", "zwave-js-ui", "n2", zwaveAlias),
			propose: func(t *testing.T) string {
				return deviceContainerDoc(t, "homeassistant", "home-assistant", "n2", zwaveAlias)
			},
			agent:    true,
			wantCode: http.StatusConflict,
		},
		{
			name:     "same path on a different node",
			existing: deviceContainerResource("zwave-js-ui", "zwave-js-ui", "n2", zwaveAlias),
			propose: func(t *testing.T) string {
				return deviceContainerDoc(t, "zwave-js-ui-n1", "zwave-js-ui", "n1", zwaveAlias)
			},
			agent:    true,
			wantCode: http.StatusCreated,
		},
		{
			name:     "shareable GPU path",
			existing: deviceContainerResource("jellyfin", "media-jellyfin", "n1", "/dev/dri/renderD128", "/dev/bus/usb"),
			propose: func(t *testing.T) string {
				return deviceContainerDoc(t, "frigate", "cameras-frigate", "n1", "/dev/dri/renderD128", "/dev/bus/usb")
			},
			agent:    true,
			wantCode: http.StatusCreated,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			calls := 0
			if tc.agent {
				h.s.Agents = radioAgent(t, &calls)
			}
			seedDesired(t, h, tc.existing)
			var out struct {
				Error string `json:"error"`
			}
			resp := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/plans",
				proposalRequest{Summary: "add a radio user", Put: []string{tc.propose(t)}}, &out)
			if resp.StatusCode != tc.wantCode {
				t.Fatalf("propose = %d (%s), want %d", resp.StatusCode, out.Error, tc.wantCode)
			}
			for _, w := range tc.wantMsg {
				if !strings.Contains(out.Error, w) {
					t.Errorf("error %q lacks %q", out.Error, w)
				}
			}
			if tc.wantCode != http.StatusCreated {
				if plans, _ := h.s.DB.ListPlans(0); len(plans) != 0 {
					t.Fatalf("a refused proposal recorded a plan: %+v", plans)
				}
			}
		})
	}
}

// An agent below the radio-inventory minor is never asked: the check
// falls back to exact paths rather than waiting on "unknown operation".
func TestDuplicateRadioCheckSkipsOldAgents(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.s.Agents = radioAgent(t, &calls)
	h.s.LiveNodes = func() []LiveNode { return []LiveNode{{NodeID: "n1", ProtocolMinor: 24}} }
	seedDesired(t, h, deviceContainerResource("zwave-js-ui", "zwave-js-ui", "n1", zwaveAlias))
	resp := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/plans", proposalRequest{
		Summary: "hass", Put: []string{deviceContainerDoc(t, "homeassistant", "home-assistant", "n1", zwaveTTY)},
	}, nil)
	if resp.StatusCode != http.StatusCreated || calls != 0 {
		t.Fatalf("propose = %d, agent calls = %d; want 201 with no ListRadios", resp.StatusCode, calls)
	}
}

// The capability install and its preview (dryRun) funnel into the same
// pipeline, so a package whose stick is already owned is refused before
// the Console ever shows an approvable plan.
func TestCapabilityPreviewRejectsDuplicateRadio(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.s.Agents = radioAgent(t, &calls)
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "zwave-test")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: zwave-test, version: 1.0.0, capability: zwave }
params:
  - { name: node, required: true, source: node }
  - { name: usb_device, required: true }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: zwave-test, node: "{{ .Params.node }}" }
    spec:
      image: "ghcr.io/example/zwave@` + testDigest + `"
      devices: [{ source: "{{ .Params.usb_device }}" }]
`
	if err := os.WriteFile(filepath.Join(pkgDir, "package.yaml"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := catalog.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	h.s.Catalog = c
	seedDesired(t, h, deviceContainerResource("homeassistant", "home-assistant", "n1", zwaveTTY))

	for _, dry := range []bool{true, false} {
		var out struct {
			Error string `json:"error"`
		}
		resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/capabilities", enableRequest{
			Package: "zwave-test", Params: map[string]string{"node": "n1", "usb_device": zwaveAlias}, DryRun: dry,
		}, &out)
		if resp.StatusCode != http.StatusConflict ||
			!strings.Contains(out.Error, "Container/zwave-test (package zwave-test)") ||
			!strings.Contains(out.Error, "Container/homeassistant (package home-assistant)") {
			t.Fatalf("dryRun=%v enable = %d %q", dry, resp.StatusCode, out.Error)
		}
	}
}
