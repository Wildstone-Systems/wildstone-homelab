package agentgateway

// Tools (#364): the Agent's own tool set for a runtime that lists its
// tools before it holds any task.

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// For an admitted person on the task's own Connection, the Agent-level
// list is exactly what that task's manifest offers: nothing is listed
// that a person-facing session would not list too.
func TestToolsListsWhatATaskManifestWould(t *testing.T) {
	f := newFixture(t)
	// A fresh surface per call, as cmd/keystone's factory builds one: the
	// fixture's own caches one per Butler, and a family surface is bound
	// to the member it was first built for.
	g := &Gateway{DB: f.db, Ledger: f.ledger, Clock: f.g.Clock,
		Gateways: butlerlive.GatewayFunc(func(b runtimedb.Butler, turn butlerlive.Turn) (butler.Toolset, error) {
			return f.surfaceFor(b, turn), nil
		}),
	}
	for _, tc := range []struct{ butler, owner, principal string }{
		{"joan", "sam", "runtime-joan-1"},
		{"alfred", "mika", "runtime-alfred-1"},
	} {
		t.Run(tc.butler, func(t *testing.T) {
			f.bindRuntime(t, tc.principal, tc.butler)
			ctx := WithPrincipal(context.Background(), tc.principal)
			got, err := g.Tools(ctx)
			if err != nil {
				t.Fatalf("Tools: %v", err)
			}
			if len(got) == 0 {
				t.Fatal("Tools listed nothing")
			}
			_, h := f.openTask(t, tc.butler, tc.owner)
			m, err := g.Manifest(ctx, h)
			if err != nil {
				t.Fatalf("Manifest: %v", err)
			}
			if !reflect.DeepEqual(toolNames(got), toolNames(m.Tools)) {
				t.Fatalf("Tools = %v\nwant the task manifest's %v", toolNames(got), toolNames(m.Tools))
			}
			for i := range got {
				if got[i].ReadOnlyHint != m.Tools[i].ReadOnlyHint {
					t.Errorf("%s: readOnlyHint %v, manifest says %v", got[i].Name, got[i].ReadOnlyHint, m.Tools[i].ReadOnlyHint)
				}
			}
		})
	}
}

// Which Agent comes from the runtime principal and nothing else.
func TestToolsNeedsARuntimePrincipal(t *testing.T) {
	f := newFixture(t)
	if _, err := f.g.Tools(context.Background()); !errors.Is(err, ErrRefused) {
		t.Fatalf("Tools with no principal = %v, want refused", err)
	}
	if _, err := f.g.Tools(WithPrincipal(context.Background(), "not-a-runtime")); err == nil {
		t.Fatal("Tools for an unbound principal succeeded")
	}
}
