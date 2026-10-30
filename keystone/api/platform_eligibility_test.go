package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/catalog"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

func platformPackage(t *testing.T, platforms, kind, nodes string) string {
	t.Helper()
	templates := ""
	for i, node := range strings.Split(nodes, ",") {
		name := fmt.Sprintf("fixture-%d", i)
		spec := `spec:
      image: "ghcr.io/example/platform-fixture@` + testDigest + `"
      devices: [{ source: /dev/net/tun }]
      capAdd: [NET_ADMIN]
      runtimeIdentity: { uid: 1000 }`
		if kind == "Job" {
			spec = `spec:
      run:
        image: "ghcr.io/example/platform-fixture@` + testDigest + `"`
		}
		templates += fmt.Sprintf(`  - |
    apiVersion: wildstone.dev/v1
    kind: %s
    metadata: { name: %s, node: %q }
    %s
`, kind, name, node, spec)
	}
	return `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: platform-fixture, version: 1.0.0, capability: platform-fixture }
` + platforms + `params: []
templates:
` + templates
}

func installPlatformPackage(t *testing.T, h *harness, body string) {
	t.Helper()
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "platform-fixture")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := catalog.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v\n%s", err, body)
	}
	h.s.Catalog = c
}

func TestPackagePlatformEligibilityPrecedesProposal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		platforms  string
		kind       string
		nodes      string
		facts      map[string][2]string
		wantCode   int
		want       string
		wantProbes int
		bound      []string
	}{
		{name: "omitted preserves compatibility", kind: "Container", nodes: "n1", wantCode: http.StatusCreated},
		{name: "matching container", platforms: "platforms: [linux/amd64]\n", kind: "Container", nodes: "n1", facts: map[string][2]string{"n1": {"linux", "amd64"}}, wantCode: http.StatusCreated, wantProbes: 1, bound: []string{"linux/amd64"}},
		{name: "matching job", platforms: "platforms: [linux/arm64]\n", kind: "Job", nodes: "n1", facts: map[string][2]string{"n1": {"linux", "arm64"}}, wantCode: http.StatusCreated, wantProbes: 1, bound: []string{"linux/arm64"}},
		{name: "canonical multi-platform set", platforms: "platforms: [linux/arm64, linux/amd64]\n", kind: "Container", nodes: "n1", facts: map[string][2]string{"n1": {"linux", "amd64"}}, wantCode: http.StatusCreated, wantProbes: 1, bound: []string{"linux/amd64", "linux/arm64"}},
		{name: "mismatch", platforms: "platforms: [linux/amd64]\n", kind: "Container", nodes: "n1", facts: map[string][2]string{"n1": {"linux", "arm64"}}, wantCode: http.StatusBadRequest, want: "reports linux/arm64", wantProbes: 1},
		{name: "old agent missing architecture", platforms: "platforms: [linux/amd64]\n", kind: "Container", nodes: "n1", facts: map[string][2]string{"n1": {"linux", ""}}, wantCode: http.StatusBadRequest, want: "protocol minor 35", wantProbes: 1},
		{name: "every selected node", platforms: "platforms: [linux/amd64]\n", kind: "Container", nodes: "n1,n2", facts: map[string][2]string{"n1": {"linux", "amd64"}, "n2": {"linux", "arm64"}}, wantCode: http.StatusBadRequest, want: "node n2 reports linux/arm64", wantProbes: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			installPlatformPackage(t, h, platformPackage(t, tc.platforms, tc.kind, tc.nodes))
			probes := 0
			if tc.facts != nil {
				h.s.Agents = applierFunc(func(_ context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
					switch op.GetOp().(type) {
					case *wsv1.Operation_HostFacts:
						probes++
						facts := tc.facts[node]
						return &wsv1.OpResult{Result: &wsv1.OpResult_HostFacts{HostFacts: &wsv1.HostFactsResult{
							Os: facts[0], Architecture: facts[1],
						}}}, nil
					case *wsv1.Operation_ListRadios:
						// Existing device eligibility remains in the same proposal
						// pipeline; this read-only inventory is not a mutation.
						return &wsv1.OpResult{Result: &wsv1.OpResult_ListRadios{ListRadios: &wsv1.ListRadiosResult{PlatformSupported: true}}}, nil
					default:
						t.Fatalf("platform admission dispatched a mutation: %T", op.GetOp())
						return nil, nil
					}
				})
			}
			var out struct {
				Error string `json:"error"`
			}
			resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/capabilities",
				enableRequest{Package: "platform-fixture"}, &out)
			if resp.StatusCode != tc.wantCode {
				t.Fatalf("enable = %d %q, want %d", resp.StatusCode, out.Error, tc.wantCode)
			}
			if tc.want != "" && !strings.Contains(out.Error, tc.want) {
				t.Fatalf("error %q lacks %q", out.Error, tc.want)
			}
			if probes != tc.wantProbes {
				t.Fatalf("HostFacts probes = %d, want %d", probes, tc.wantProbes)
			}
			plans, err := h.s.DB.ListPlans(0)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantCode == http.StatusCreated && len(plans) != 1 {
				t.Fatalf("accepted platform produced %d plans, want 1", len(plans))
			}
			if tc.wantCode != http.StatusCreated && len(plans) != 0 {
				t.Fatalf("refused platform left a partial plan: %+v", plans)
			}
			if tc.wantCode == http.StatusCreated && len(tc.bound) > 0 {
				if code := h.approve(plans[0].ID); code != http.StatusOK {
					t.Fatalf("approve platform plan = %d", code)
				}
				desired, err := h.s.State.Load()
				if err != nil || len(desired) != len(strings.Split(tc.nodes, ",")) {
					t.Fatalf("approved desired state = %+v, %v", desired, err)
				}
				for _, workload := range desired {
					got := []string(nil)
					if workload.Container != nil {
						got = workload.Container.Platforms
					} else if workload.Job != nil {
						got = workload.Job.Run.Platforms
					}
					if strings.Join(got, ",") != strings.Join(tc.bound, ",") {
						t.Fatalf("approved %s lost its platform constraint: %+v", workload.Kind, got)
					}
				}
			}
		})
	}
}
