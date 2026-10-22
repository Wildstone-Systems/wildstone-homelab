package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/reconcile"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/research"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

// Fake worker and dependency observation only: never release or live evidence.
func TestManagedResearchProbeRequiresReleaseAndCurrentDependency(t *testing.T) {
	for _, mode := range []string{"success", "no release", "wrong image", "absent", "stale before", "stale after", "changed head", "dirty state", "changed during probe", "upstream failure", "cancelled", "credential", "wrong ref"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			var onSearch func()
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if onSearch != nil {
					onSearch()
				}
				if r.URL.Path != "/v1/search" || r.Method != "POST" {
					t.Error("probe did not use search contract")
				}
				w.Header().Set("Content-Type", "application/json")
				if mode == "upstream failure" {
					w.WriteHeader(503)
					fmt.Fprint(w, `{"version":1,"error":{"code":"provider_unavailable","message":"fixture"}}`)
					return
				}
				fmt.Fprint(w, `{"version":1,"data":{"items":[],"provider":"ddgs","backend":"duckduckgo","retrievedAt":"2026-09-30T00:00:00Z"}}`)
			}))
			defer worker.Close()
			state, err := statefile.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s := &Server{State: state, NodeID: "n1"}
			image := "registry.example.internal/ddgs@sha256:" + strings.Repeat("1", 64)
			container, err := resource.Unmarshal([]byte(`apiVersion: wildstone.dev/v1
kind: Container
metadata: {name: ddgs, node: n1, package: research-ddgs, owner: capability/web-research}
spec:
 image: "` + image + `"
 ports: ["` + strings.TrimPrefix(worker.URL, "http://") + `:8080"]
`))
			if err != nil {
				t.Fatal(err)
			}
			put := func(commit bool) {
				t.Helper()
				if err := state.Put(container); err != nil {
					t.Fatal(err)
				}
				if commit {
					if err := state.Commit("test", "fixture", "dependency"); err != nil {
						t.Fatal(err)
					}
				}
			}
			put(true)
			if mode == "changed during probe" {
				onSearch = func() {
					container.Container.Env = map[string]string{"REVISION": "changed"}
					if err := state.Put(container); err != nil {
						t.Error(err)
					}
					if err := state.Commit("test", "fixture", "changed during probe"); err != nil {
						t.Error(err)
					}
				}
			}
			if mode == "absent" {
				if err := state.Delete(resource.KindContainer, "ddgs"); err != nil {
					t.Fatal(err)
				}
				if err := state.Commit("test", "fixture", "remove"); err != nil {
					t.Fatal(err)
				}
			}
			checked := 0
			check := func(ctx context.Context, node, name string, desired []resource.Resource) error {
				checked++
				if node != "n1" || name != "ddgs" || len(desired) != 1 {
					t.Fatal("wrong dependency")
				}
				if mode == "stale before" || (mode == "stale after" && checked == 2) {
					return errors.New("stale")
				}
				if mode == "changed head" || mode == "dirty state" {
					container.Container.Env = map[string]string{"REVISION": "changed"}
					put(mode == "changed head")
				}
				return nil
			}
			pin := image
			if mode == "no release" {
				pin = ""
			}
			if mode == "wrong image" {
				pin = strings.Replace(image, strings.Repeat("1", 64), strings.Repeat("2", 64), 1)
			}
			p := research.Provider{Config: research.Config{Name: "Fixture", Kind: research.DDGS, EndpointRef: research.ManagedEndpointRef}}
			if mode == "credential" {
				p.CredentialRef = "secret/ddgs"
			}
			if mode == "wrong ref" {
				p.EndpointRef = "service/other"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			err = s.managedResearchTest(check, pin)(ctx, p)
			if (err == nil) != (mode == "success") {
				t.Fatalf("probe result: %v", err)
			}
			switch mode {
			case "success":
				if calls.Load() != 1 || checked != 2 {
					t.Fatalf("calls=%d inspections=%d", calls.Load(), checked)
				}
			case "no release", "wrong image", "absent", "stale before", "cancelled", "credential", "wrong ref":
				if calls.Load() != 0 {
					t.Fatal("unverified dependency contacted")
				}
			}
		})
	}
}

func TestManagedResearchProductionRemainsDormantWithoutRelease(t *testing.T) {
	if research.ManagedReleaseImage != "" {
		t.Skip("release pin now exists; release verification owns artifact evidence")
	}
	s := &Server{}
	s.WireManagedResearch(&reconcile.Reconciler{})
	if s.ResearchTest == nil {
		t.Fatal("production probe not wired")
	}
	if err := s.ResearchTest(context.Background(), research.Provider{}); err == nil {
		t.Fatal("missing release became ready")
	}
}
