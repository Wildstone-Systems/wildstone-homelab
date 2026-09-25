package agentruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
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

// fakeToolset is the slice of a Butler gateway ButlerTools uses.
type fakeToolset struct {
	denied   string
	execErr  error
	executed []string
	readOnly []bool
}

func (f *fakeToolset) OfferedSpecs(readOnly bool) []provider.Tool {
	f.readOnly = append(f.readOnly, readOnly)
	return []provider.Tool{
		{Name: "get_status", Description: "status", InputSchema: map[string]any{"type": "object"}},
		{Name: "bare", Description: "no schema"},
	}
}

func (f *fakeToolset) Execute(_ context.Context, name string, _ map[string]any, readOnly bool) (butler.ToolOutcome, error) {
	f.executed = append(f.executed, name)
	f.readOnly = append(f.readOnly, readOnly)
	if f.execErr != nil {
		return butler.ToolOutcome{}, f.execErr
	}
	return butler.ToolOutcome{Content: "all healthy"}, nil
}

func (f *fakeToolset) SystemPrompt() string { return "" }

func (f *fakeToolset) Permits(name string, _ bool) (bool, string) {
	if name == f.denied {
		return false, "you may not use " + name
	}
	return true, ""
}

func (f *fakeToolset) Capabilities(bool) butler.CapabilityManifest {
	return butler.CapabilityManifest{}
}

func TestButlerTools(t *testing.T) {
	ts := &fakeToolset{denied: "secret"}
	host := ButlerTools{Toolset: ts, ReadOnly: true}

	specs := host.Specs()
	if len(specs) != 2 || specs[1].Parameters["type"] != "object" {
		t.Fatalf("specs %+v", specs)
	}

	res, err := host.Call(context.Background(), "get_status", nil)
	if err != nil || res.Content != "all healthy" || res.Denied || res.Failed {
		t.Fatalf("call: %+v %v", res, err)
	}

	res, _ = host.Call(context.Background(), "secret", nil)
	if !res.Denied || res.Content != "you may not use secret" {
		t.Fatalf("denial: %+v", res)
	}
	for _, name := range ts.executed {
		if name == "secret" {
			t.Fatal("a denied tool was executed")
		}
	}
	for _, ro := range ts.readOnly {
		if !ro {
			t.Fatal("the read-only verdict was not passed through")
		}
	}

	ts.execErr = errors.New("db path /var/secret leaked")
	res, _ = host.Call(context.Background(), "get_status", nil)
	if !res.Failed || res.Content != "error: get_status could not be run" {
		t.Fatalf("error result must not echo the error text: %+v", res)
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
