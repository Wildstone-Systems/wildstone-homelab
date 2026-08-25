package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

func TestBackupEndpoints(t *testing.T) {
	h := newHarness(t)

	// Unwired: 503.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/backups/photos/run", nil, nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unwired = %d, want 503", resp.StatusCode)
	}

	var ran, restored []string
	h.s.BackupRun = func(_ context.Context, plan string) error {
		ran = append(ran, plan)
		if plan == "broken" {
			return errors.New("no BackupPlan broken")
		}
		return nil
	}
	h.s.BackupRestore = func(_ context.Context, plan string) error {
		restored = append(restored, plan)
		return nil
	}

	// approve verb: the Butler (propose-only) can never trigger a run or
	// restore — the structural safety claim carried through (ADR-W015 A2).
	for _, path := range []string{"/api/v1/backups/photos/run", "/api/v1/backups/photos/restore"} {
		if resp := h.do(runtimedb.RoleButler, http.MethodPost, path, nil, nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("butler on %s = %d, want 403", path, resp.StatusCode)
		}
	}
	if len(ran)+len(restored) != 0 {
		t.Fatal("butler request reached the engine")
	}

	// The operator can run and restore.
	if resp := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/backups/photos/run", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("run = %d", resp.StatusCode)
	}
	if resp := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/backups/photos/restore", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("restore = %d", resp.StatusCode)
	}
	if len(ran) != 1 || ran[0] != "photos" || len(restored) != 1 || restored[0] != "photos" {
		t.Fatalf("engine calls = run %v, restore %v", ran, restored)
	}

	// Engine failure surfaces.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/backups/broken/run", nil, nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("broken run = %d, want 502", resp.StatusCode)
	}

	// Ledger list (read verb — the Butler may read).
	if _, err := h.s.DB.StartBackupRun("photos", runtimedb.BackupRunBackup, "plan-1"); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Runs []runtimedb.BackupRun `json:"runs"`
	}
	if resp := h.do(runtimedb.RoleButler, http.MethodGet, "/api/v1/backups", nil, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", resp.StatusCode)
	}
	if len(out.Runs) != 1 || out.Runs[0].PlanName != "photos" {
		t.Fatalf("runs = %+v", out.Runs)
	}
}

func TestContainerLogsEndpointDemuxes(t *testing.T) {
	h := newHarness(t)
	// Framed docker stream: stdout "hello\n", stderr "oops\n".
	framed := append(frame(1, "hello\n"), frame(2, "oops\n")...)
	h.s.Agents = applierFunc(func(_ context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
		logsOp := op.GetContainerLogs()
		if logsOp == nil || node != "n1" {
			t.Fatalf("unexpected dispatch: node=%s op=%v", node, op)
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_ContainerLogs{ContainerLogs: &wsv1.ContainerLogsResult{
			Logs: framed,
		}}}, nil
	})

	var out struct {
		Logs string `json:"logs"`
	}
	if resp := h.do(runtimedb.RoleButler, http.MethodGet, "/api/v1/nodes/n1/containers/app/logs?tail=50", nil, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("logs = %d", resp.StatusCode)
	}
	if out.Logs != "hello\noops\n" {
		t.Fatalf("logs = %q, want demuxed text", out.Logs)
	}
}

func TestDemuxDockerStream(t *testing.T) {
	if got := demuxDockerStream([]byte("raw tty output")); got != "raw tty output" {
		t.Fatalf("unframed passthrough = %q", got)
	}
	if got := demuxDockerStream(nil); got != "" {
		t.Fatalf("empty = %q", got)
	}
	// Truncated frame header falls back to raw.
	trunc := frame(1, "x")[:5]
	if got := demuxDockerStream(trunc); got != string(trunc) {
		t.Fatalf("truncated = %q", got)
	}
}

func frame(stream byte, payload string) []byte {
	n := len(payload)
	head := []byte{stream, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	return append(head, payload...)
}

// applierFunc adapts a function to reconcile.Applier for tests.
type applierFunc func(ctx context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error)

func (f applierFunc) Apply(ctx context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	return f(ctx, node, op)
}
