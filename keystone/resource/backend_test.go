package resource

import (
	"strings"
	"testing"
)

func TestContainerBackendDefaultsAndBounds(t *testing.T) {
	image := "example/app@sha256:" + strings.Repeat("a", 64)
	base := Resource{ApiVersion: ApiVersion, Kind: KindContainer,
		Metadata:  Metadata{Name: "app", Owner: "capability/tools", Node: "node-a"},
		Container: &ContainerSpec{Image: image}}
	plain, err := Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "backend:") {
		t.Fatalf("legacy Docker resource gained a backend field: %s", plain)
	}
	got, err := Unmarshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if got.Container.Backend != "" {
		t.Fatalf("legacy backend = %q", got.Container.Backend)
	}
	base.Container.Backend = "k3s"
	selected, err := Marshal(base)
	if err != nil || !strings.Contains(string(selected), "backend: k3s") {
		t.Fatalf("k3s choice not serialized: %s, %v", selected, err)
	}
	base.Container.Backend = "other"
	if err := base.Validate(); err == nil {
		t.Fatal("unrecognized backend was accepted")
	}
}

func TestContainerExposeIsBoundedAndUnique(t *testing.T) {
	image := "example/app@sha256:" + strings.Repeat("a", 64)
	base := Resource{ApiVersion: ApiVersion, Kind: KindContainer,
		Metadata:  Metadata{Name: "app", Owner: "capability/tools", Node: "node-a"},
		Container: &ContainerSpec{Image: image, Expose: []uint32{8080}}}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, expose := range [][]uint32{{0}, {65536}, {8080, 8080}} {
		base.Container.Expose = expose
		if err := base.Validate(); err == nil {
			t.Errorf("expose %v was accepted", expose)
		}
	}
}
