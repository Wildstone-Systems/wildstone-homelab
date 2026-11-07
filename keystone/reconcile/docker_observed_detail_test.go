package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

type observationFailureApplier struct {
	transport bool
}

func (a observationFailureApplier) Apply(_ context.Context, _ string, _ *wsv1.Operation) (*wsv1.OpResult, error) {
	if a.transport {
		return nil, errors.New("private runtime detail")
	}
	return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{
		Code: wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, Message: "private runtime detail",
	}}}, nil
}

func TestDockerObservedDetailUsesClosedBoundedClasses(t *testing.T) {
	tests := []struct {
		name    string
		got     *wsv1.InspectContainerResult
		matches bool
		want    string
	}{
		{"exited", &wsv1.InspectContainerResult{Status: "exited", ExitCode: 137}, true, "exited: Docker container stopped (exit code 137)"},
		{"other stopped", &wsv1.InspectContainerResult{Status: "private/path/value"}, true, "stopped: Docker container is not running"},
		{"unhealthy", &wsv1.InspectContainerResult{Running: true, AppHealth: "unhealthy"}, true, "unhealthy: Docker healthcheck reported unhealthy"},
		{"starting", &wsv1.InspectContainerResult{Running: true, AppHealth: "starting"}, true, "starting: Docker healthcheck has not reported ready"},
		{"spec drift", &wsv1.InspectContainerResult{Running: true, AppHealth: "private-token-value"}, false, "drifted: managed Docker configuration differs from desired state"},
		{"healthy requested change", &wsv1.InspectContainerResult{Running: true, AppHealth: "healthy"}, false, "running: Docker container is healthy; managed configuration differs from desired state"},
		{"healthy and matching", &wsv1.InspectContainerResult{Running: true, AppHealth: "healthy"}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dockerObservedDetail(tt.got, tt.matches)
			if got != tt.want {
				t.Fatalf("detail = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, "private") || strings.Contains(got, "token") || strings.Contains(got, "path") {
				t.Fatalf("runtime-originated text escaped the closed vocabulary: %q", got)
			}
		})
	}
}

func TestMissingDockerObservationIsTypedAndUnavailableDoesNotInventCause(t *testing.T) {
	want := resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: "app", Owner: "capability/test", Node: "n1"},
		Container:  &resource.ContainerSpec{Image: "docker.io/library/nginx@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	}
	r := &Reconciler{Applier: observationFailureApplier{}}
	observed, _, _, details, err := r.observe(t.Context(), "n1", []resource.Resource{want}, map[resKey]ClusterResult{})
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 0 || details["Container/app"] != "missing: Docker container was not found" {
		t.Fatalf("observed = %+v, details = %+v", observed, details)
	}
	if strings.Contains(details["Container/app"], "private") {
		t.Fatalf("runtime error detail escaped the missing class: %q", details["Container/app"])
	}

	r.Applier = observationFailureApplier{transport: true}
	_, _, _, unavailable, err := r.observe(t.Context(), "n1", []resource.Resource{want}, map[resKey]ClusterResult{})
	if err == nil {
		t.Fatal("unavailable observation unexpectedly succeeded")
	}
	if len(unavailable) != 0 {
		t.Fatalf("unavailable observation invented a cause: %+v", unavailable)
	}
}
