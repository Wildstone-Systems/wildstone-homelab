package agentruntime

import (
	"context"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestStateClasses(t *testing.T) {
	for _, c := range []struct {
		s                            State
		terminal, settled, resumable bool
	}{
		{StateQueued, false, false, false},
		{StateRunning, false, false, false},
		{StateWaitingForTool, false, false, false},
		{StateWaitingForUser, false, true, true},
		{StateBlocked, false, true, true},
		{StateCompleted, true, true, false},
		{StateFailed, true, true, false},
		{StateCancelled, true, true, false},
	} {
		if c.s.Terminal() != c.terminal || c.s.Settled() != c.settled || c.s.Resumable() != c.resumable {
			t.Errorf("%s: terminal %v settled %v resumable %v", c.s, c.s.Terminal(), c.s.Settled(), c.s.Resumable())
		}
	}
}

func TestLimitsDefaults(t *testing.T) {
	l := Limits{MaxTurns: 2, JobTimeout: time.Second}.WithDefaults()
	if l.MaxTurns != 2 || l.JobTimeout != time.Second {
		t.Fatalf("set fields overwritten: %+v", l)
	}
	if l.ToolTimeout != DefaultLimits.ToolTimeout || l.MaxToolCalls != DefaultLimits.MaxToolCalls {
		t.Fatalf("zero fields not defaulted: %+v", l)
	}
}

func TestGuard(t *testing.T) {
	g := NewGuard(Limits{MaxTurns: 2, MaxToolCalls: 3, MaxRepeatedCalls: 2, MaxToolFailures: 2, MaxInvalidActions: 1})

	if g.Turn() != "" || g.Turn() != "" || g.Turn() == "" {
		t.Fatal("turn budget")
	}
	if g.Invalid() != "" || g.Invalid() == "" {
		t.Fatal("invalid budget")
	}
	g.NewRun()
	if g.Turns() != 0 || g.Turn() != "" {
		t.Fatal("NewRun did not reset turns")
	}

	// Key order does not make a call new.
	if g.BeforeTool("a", map[string]any{"x": 1, "y": 2}) != "" {
		t.Fatal("first call refused")
	}
	if g.BeforeTool("a", map[string]any{"y": 2, "x": 1}) != "" {
		t.Fatal("second identical call refused")
	}
	if g.BeforeTool("a", map[string]any{"x": 1, "y": 2}) == "" {
		t.Fatal("third identical call allowed")
	}
	if g.BeforeTool("b", nil) != "" {
		t.Fatal("different call refused")
	}
	if g.BeforeTool("c", nil) == "" {
		t.Fatal("tool budget not enforced")
	}
	if g.ToolCalls() != 3 {
		t.Fatalf("ToolCalls %d", g.ToolCalls())
	}

	if g.AfterTool(true) != "" || g.AfterTool(false) != "" || g.AfterTool(true) != "" || g.AfterTool(true) == "" {
		t.Fatal("failure streak: a success must reset it, two in a row must block")
	}
}

type appender struct{ got []runtimedb.Event }

func (a *appender) AppendEvent(e runtimedb.Event) (int64, error) {
	a.got = append(a.got, e)
	return int64(len(a.got)), nil
}

func TestEventLogRecorder(t *testing.T) {
	ap := &appender{}
	err := EventLogRecorder{Events: ap}.Record(context.Background(), AuditEvent{
		JobID: "j1", Runtime: "native", Type: AuditToolDenied, Tool: "unlock", CallID: "c1",
		Actor: "alice", Detail: "not permitted", At: time.UnixMilli(1000),
	})
	if err != nil || len(ap.got) != 1 {
		t.Fatalf("%v %+v", err, ap.got)
	}
	e := ap.got[0]
	if e.Type != "agent.tool.denied" || e.Name != "j1" || e.Message != "not permitted" ||
		e.Attrs["tool"] != "unlock" || e.Attrs["actor"] != "alice" || e.Attrs["at"] != "1000" {
		t.Fatalf("event %+v", e)
	}
}
