package research

import (
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"strings"
	"testing"
)

func TestManagedEndpointConfinesConfiguration(t *testing.T) {
	valid := func() resource.Resource {
		r, err := resource.Unmarshal([]byte(`apiVersion: wildstone.dev/v1
kind: Container
metadata: {name: ddgs, node: n1, package: research-ddgs, owner: capability/web-research}
spec:
  image: "registry.example.internal/ddgs@sha256:` + strings.Repeat("1", 64) + `"
  ports: ["127.0.0.1:18081:8080"]
`))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := valid()
	endpoint, err := ManagedEndpoint([]resource.Resource{r}, "n1", ManagedEndpointRef)
	if err != nil || endpoint != "http://127.0.0.1:18081" {
		t.Fatalf("%s %v", endpoint, err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*resource.Resource)
	}{
		{"foreign package", func(r *resource.Resource) { r.Metadata.Package = "other" }},
		{"missing package", func(r *resource.Resource) { r.Metadata.Package = "" }},
		{"wrong node", func(r *resource.Resource) { r.Metadata.Node = "n2" }},
		{"public listener", func(r *resource.Resource) { r.Container.Ports = []string{"18080:8080"} }},
		{"wrong port", func(r *resource.Resource) { r.Container.Ports = []string{"127.0.0.1:18080:8081"} }},
		{"udp", func(r *resource.Resource) { r.Container.Ports = []string{"127.0.0.1:18080:8080/udp"} }},
		{"extra listener", func(r *resource.Resource) { r.Container.Ports = append(r.Container.Ports, "8080:8080") }},
		{"tag", func(r *resource.Resource) { r.Container.Image = "example/ddgs:latest" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := valid()
			tc.mutate(&r)
			if _, err := ManagedEndpoint([]resource.Resource{r}, "n1", ManagedEndpointRef); err == nil {
				t.Fatal("accepted unsafe configuration")
			}
		})
	}
	for _, rs := range [][]resource.Resource{nil, {r, r}} {
		if _, err := ManagedEndpoint(rs, "n1", ManagedEndpointRef); err == nil {
			t.Fatal("accepted missing/duplicate service")
		}
	}
	for _, ref := range []string{"", "service/other", "http://example.com"} {
		if _, err := ManagedEndpoint([]resource.Resource{r}, "n1", ref); err == nil {
			t.Fatal("accepted unrelated endpoint")
		}
	}
	if _, err := ManagedEndpoint([]resource.Resource{r}, "", ManagedEndpointRef); err == nil {
		t.Fatal("accepted unknown Keystone node")
	}
}
