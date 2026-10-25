package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

type readinessApplier struct {
	reply *wsv1.OpResult
	err   error
	calls int
}

func (a *readinessApplier) Apply(ctx context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	a.calls++
	if node != "node-a" || op.GetInspectContainer().GetName() != "ddgs" {
		return nil, errors.New("unexpected operation")
	}
	return a.reply, a.err
}

func TestDDGSDependencyObservationFailsClosed(t *testing.T) {
	for _, mode := range []string{"success", "stopped", "wrong image", "wrong hash", "absent", "foreign", "old agent", "nil reply", "transport", "paused", "unhealthy", "starting", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			desired := containerResource("ddgs", testDigestImage)
			// Obtain the receipt from the same operation sent to Wardstone, rather
			// than inventing a test-only hash. Full gateway lifecycle is tested too.
			labels := docker.ensureContainer("ddgs", desired.Container, ContainerNetwork, "", nil).GetEnsureContainer().GetLabels()
			got := &wsv1.InspectContainerResult{Running: true, Image: testDigestImage, Labels: labels, Status: "running", AppHealth: "healthy"}
			a := &readinessApplier{reply: &wsv1.OpResult{Result: &wsv1.OpResult_InspectContainer{InspectContainer: got}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "stopped":
				got.Running = false
			case "wrong image":
				got.Image = "other"
			case "wrong hash":
				got.Labels = map[string]string{specHashLabel: "stale"}
			case "paused":
				got.Status = "paused"
			case "unhealthy", "starting":
				got.AppHealth = mode
			case "transport":
				a.err = errors.New("private transport detail")
			case "nil reply":
				a.reply = nil
			case "cancelled":
				cancel()
			case "absent", "foreign", "old agent":
				code := wsv1.ErrorCode_ERROR_CODE_NOT_FOUND
				if mode == "foreign" {
					code = wsv1.ErrorCode_ERROR_CODE_FORBIDDEN
				}
				if mode == "old agent" {
					code = wsv1.ErrorCode_ERROR_CODE_INVALID
				}
				a.reply = &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{Code: code}}}
			}
			r := &Reconciler{Applier: a}
			err := r.VerifyRunningContainer(ctx, "node-a", "ddgs", []resource.Resource{desired})
			if (err == nil) != (mode == "success") {
				t.Fatalf("result=%v", err)
			}
			if a.calls > 1 {
				t.Fatal("inspection retried or fell back")
			}
		})
	}
}
