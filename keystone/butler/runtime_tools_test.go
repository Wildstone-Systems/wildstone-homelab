package butler

import (
	"context"
	"errors"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
)

// fakeToolset is the slice of a Butler gateway RuntimeTools uses.
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

func (f *fakeToolset) Execute(_ context.Context, name string, _ map[string]any, readOnly bool) (ToolOutcome, error) {
	f.executed = append(f.executed, name)
	f.readOnly = append(f.readOnly, readOnly)
	if f.execErr != nil {
		return ToolOutcome{}, f.execErr
	}
	return ToolOutcome{Content: "all healthy"}, nil
}

func (f *fakeToolset) SystemPrompt() string { return "" }

func (f *fakeToolset) Permits(name string, _ bool) (bool, string) {
	if name == f.denied {
		return false, "you may not use " + name
	}
	return true, ""
}

func (f *fakeToolset) Capabilities(bool) CapabilityManifest {
	return CapabilityManifest{}
}

func TestRuntimeTools(t *testing.T) {
	ts := &fakeToolset{denied: "secret"}
	host := RuntimeTools{Toolset: ts, ReadOnly: true}

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
