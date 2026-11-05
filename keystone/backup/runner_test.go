package backup

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// scriptedApplier answers every op with success, except InspectContainer,
// which returns the scripted results in order (the last one repeats). It
// records the op sequence so tests can check what ran and in what order.
type scriptedApplier struct {
	inspects []*wsv1.OpResult
	ops      []string
	secrets  [][]string
	// removeErr, when set, answers every RemoveContainer with it.
	removeErr *wsv1.OpError
}

func (s *scriptedApplier) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	switch op.GetOp().(type) {
	case *wsv1.Operation_ListSecrets:
		s.ops = append(s.ops, "list")
		var names []string
		if len(s.secrets) != 0 {
			names = s.secrets[0]
			if len(s.secrets) > 1 {
				s.secrets = s.secrets[1:]
			}
		}
		result := &wsv1.ListSecretsResult{}
		for _, name := range names {
			result.Secrets = append(result.Secrets, &wsv1.SecretInfo{Name: name, Version: 1})
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_ListSecrets{ListSecrets: result}}, nil
	case *wsv1.Operation_PullImage:
		s.ops = append(s.ops, "pull")
	case *wsv1.Operation_EnsureContainer:
		s.ops = append(s.ops, "ensure")
	case *wsv1.Operation_StopContainer:
		s.ops = append(s.ops, "stop")
	case *wsv1.Operation_RemoveContainer:
		s.ops = append(s.ops, "remove")
		if s.removeErr != nil {
			return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: s.removeErr}}, nil
		}
	case *wsv1.Operation_InspectContainer:
		s.ops = append(s.ops, "inspect")
		res := s.inspects[0]
		if len(s.inspects) > 1 {
			s.inspects = s.inspects[1:]
		}
		return res, nil
	}
	return &wsv1.OpResult{}, nil
}

func inspected(running bool, status string, code int32) *wsv1.OpResult {
	return &wsv1.OpResult{Result: &wsv1.OpResult_InspectContainer{InspectContainer: &wsv1.InspectContainerResult{
		Running: running, Status: status, ExitCode: code,
	}}}
}

func notFound() *wsv1.OpResult {
	return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{Code: wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, Message: "no such container"}}}
}

func runScripted(t *testing.T, inspects ...*wsv1.OpResult) (*scriptedApplier, error) {
	t.Helper()
	app := &scriptedApplier{inspects: inspects}
	r := &ApplierRunner{Applier: app, Poll: time.Millisecond, Timeout: time.Second}
	err := r.RunContainer(context.Background(), "n1", "backup-photos", resource.ContainerSpec{Image: testResticImage})
	return app, err
}

// TestRunnerExitZeroSucceeds: running, then exited 0, is success.
func TestRunnerExitZeroSucceeds(t *testing.T) {
	app, err := runScripted(t, inspected(true, "running", 0), inspected(false, "exited", 0))
	if err != nil {
		t.Fatalf("exit 0: %v", err)
	}
	// A leftover of the same name is cleared before ensure, and the
	// container is removed after it finishes.
	want := "pull,remove,ensure,inspect,inspect,stop,remove"
	if got := strings.Join(app.ops, ","); got != want {
		t.Fatalf("ops = %s, want %s", got, want)
	}
}

func TestRunnerRechecksPasswordImmediatelyBeforeContainerMutation(t *testing.T) {
	app := &scriptedApplier{}
	r := &ApplierRunner{Applier: app, Poll: time.Millisecond, Timeout: time.Second}
	err := r.RunContainer(context.Background(), "n1", "backup-photos", resource.ContainerSpec{
		Image: testResticImage,
		Env:   map[string]string{"RESTIC_PASSWORD": "secret/restic-photos"},
	})
	if err == nil || !strings.Contains(err.Error(), "secret/restic-photos") {
		t.Fatalf("error = %v", err)
	}
	if got := strings.Join(app.ops, ","); got != "list" {
		t.Fatalf("container path mutated before dependency refusal: %v", app.ops)
	}
}

func TestRunnerCatchesPasswordDisappearanceAtEnsureBoundary(t *testing.T) {
	app := &scriptedApplier{secrets: [][]string{{"restic-photos"}, {}}}
	r := &ApplierRunner{Applier: app, Poll: time.Millisecond, Timeout: time.Second}
	err := r.RunContainer(context.Background(), "n1", "backup-photos", resource.ContainerSpec{
		Image: testResticImage,
		Env:   map[string]string{"RESTIC_PASSWORD": "secret/restic-photos"},
	})
	if err == nil || !strings.Contains(err.Error(), "secret/restic-photos") {
		t.Fatalf("error = %v", err)
	}
	if got := strings.Join(app.ops, ","); got != "list,pull,remove,list" {
		t.Fatalf("ops = %s, want no EnsureContainer after the dependency disappeared", got)
	}
}

// TestRunnerNonZeroExitIsExitError: the exit code reaches the engine.
func TestRunnerNonZeroExitIsExitError(t *testing.T) {
	_, err := runScripted(t, inspected(false, "exited", 12))
	code, ok := exitCode(err)
	if !ok || code != 12 {
		t.Fatalf("err = %v, want an ExitError with code 12", err)
	}
}

// TestRunnerDeadIsFailure: a dead container with a non-zero code fails.
func TestRunnerDeadIsFailure(t *testing.T) {
	if _, err := runScripted(t, inspected(false, "dead", 137)); err == nil {
		t.Fatal("a dead container must fail the run")
	}
}

// TestRunnerCreatedKeepsPolling: "created" is not finished.
func TestRunnerCreatedKeepsPolling(t *testing.T) {
	app, err := runScripted(t, inspected(false, "created", 0), inspected(false, "exited", 0))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(app.ops, ","), "inspect"); n != 2 {
		t.Fatalf("inspects = %d, want 2 (created is not terminal)", n)
	}
}

// TestRunnerUnknownStatusFails: an agent that cannot report exit status
// (pre protocol minor 11) never yields a success.
func TestRunnerUnknownStatusFails(t *testing.T) {
	_, err := runScripted(t, inspected(false, "", 0))
	if err == nil || !strings.Contains(err.Error(), "protocol minor 11") {
		t.Fatalf("err = %v, want the unknown-status failure", err)
	}
}

// TestRunnerVanishedContainerFails: a container that disappears took its
// exit code with it; that is not success.
func TestRunnerVanishedContainerFails(t *testing.T) {
	_, err := runScripted(t, notFound())
	if err == nil || !strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("err = %v, want the vanished-container failure", err)
	}
}

// TestRunnerTimeout: still running at the deadline is a failure.
func TestRunnerTimeout(t *testing.T) {
	app := &scriptedApplier{inspects: []*wsv1.OpResult{inspected(true, "running", 0)}}
	r := &ApplierRunner{Applier: app, Poll: time.Millisecond, Timeout: 20 * time.Millisecond}
	err := r.RunContainer(context.Background(), "n1", "backup-photos", resource.ContainerSpec{Image: testResticImage})
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("err = %v, want a timeout", err)
	}
}

// TestRunnerAbortsWhenLeftoverWontClear: a leftover that cannot be
// removed is never reused — EnsureContainer would keep it, stale command,
// mounts and credentials included — so the run stops before ensure.
func TestRunnerAbortsWhenLeftoverWontClear(t *testing.T) {
	app := &scriptedApplier{
		inspects:  []*wsv1.OpResult{inspected(false, "exited", 0)},
		removeErr: &wsv1.OpError{Code: wsv1.ErrorCode_ERROR_CODE_RUNTIME, Message: "device busy"},
	}
	r := &ApplierRunner{Applier: app, Poll: time.Millisecond, Timeout: time.Second}
	err := r.RunContainer(context.Background(), "n1", "backup-photos", resource.ContainerSpec{Image: testResticImage})
	if err == nil || !strings.Contains(err.Error(), "clearing a leftover") {
		t.Fatalf("err = %v, want the clear-out failure", err)
	}
	for _, op := range app.ops {
		if op == "ensure" {
			t.Fatalf("ops = %v: ensured after a failed clear-out", app.ops)
		}
	}
}
