package agentgateway

// The processing-class disclosure filter (ADR-W039 §3.5 rule 3,
// #345): the gateway-side replacement for
// keystone/butlerlive.privacyProvider (C10). A runtime declared
// local_only receives local-only content at every boundary the gateway
// hands it something; a runtime declared cloud, or one with no
// declaration at all (fail closed, §3.5's "undeclared fails closed"),
// never does. The class always comes from butler_runtimes — never from
// a request or a heartbeat, neither of which carries a field for one
// (agentapi.RecallRequest, InvokeRequest and Heartbeat all lack one, so
// there is nothing here to smuggle it through even by mistake).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// putRestrictedMemory records one local-only-flagged memory item owned
// by mika on joan and returns its seq. kind picks whether Recall can
// find it (MemoryFact) or the preamble embeds it in the system prompt
// (MemoryPreference, butlerstore/memory.go's preambleKinds).
func putRestrictedMemory(t *testing.T, f *fixture, kind, content string) int64 {
	t.Helper()
	m := runtimedb.MemoryRecord{
		Scope: runtimedb.MemoryScopePersonal, Kind: kind, Content: content,
		Owner: "mika", Author: "mika", Butler: "joan",
		Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "household",
		SourceKind: "conversation", SourceID: "disclosure-fixture",
	}
	seq, err := f.db.RecordMemory(m)
	if err != nil {
		t.Fatalf("RecordMemory: %v", err)
	}
	if _, err := f.db.IndexDocumentInto(runtimedb.IndexDocument{
		MemorySeq: seq, SourceKind: m.SourceKind, SourceID: m.SourceID, Content: content,
		Owner: "mika", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "household",
	}); err != nil {
		t.Fatalf("IndexDocumentInto: %v", err)
	}
	if err := f.db.SetMemoryPrivacy("mika", seq, runtimedb.PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatalf("SetMemoryPrivacy: %v", err)
	}
	return seq
}

// setProcessingClass is the ONLY thing these tests use to declare a
// runtime's class — a direct write to butler_runtimes, exactly as the
// Owner's PUT .../runtime route would leave it (runtimedb.
// SetButlerRuntime), never anything a request or heartbeat carries.
func setProcessingClass(t *testing.T, f *fixture, butlerName string, class agentapi.ProcessingClass) {
	t.Helper()
	b, err := f.db.GetButler(butlerName)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := f.db.GetButlerRuntime(b.Seq)
	if err != nil {
		t.Fatal(err)
	}
	rt.ProcessingClass = class
	if err := f.db.SetButlerRuntime(b.Seq, rt, "mika"); err != nil {
		t.Fatalf("SetButlerRuntime: %v", err)
	}
}

// TestDisclosureFilterOverRecallAndInvoke is the table over two of the
// four surfaces the filter guards: Recall's structured hits, and the
// SAME content reached generically through Invoke (the "recall" tool
// call every runtime — native or external — actually dispatches).
// Manifest has its own test below, because a refused manifest and a
// filtered hit list are different shapes of "withheld".
func TestDisclosureFilterOverRecallAndInvoke(t *testing.T) {
	const marker = "the safe combination is nine four one seven"
	for _, tc := range []struct {
		name          string
		class         agentapi.ProcessingClass
		wantDisclosed bool
	}{
		{"cloud never receives it", agentapi.ProcessingCloud, false},
		{"undeclared fails closed", "", false},
		{"local_only receives it", agentapi.ProcessingLocalOnly, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.g.Gateways = butlerGatewayFunc(f)
			if tc.class != "" {
				setProcessingClass(t, f, "joan", tc.class)
			}
			putRestrictedMemory(t, f, runtimedb.MemoryFact, marker)
			ctx := context.Background()

			t.Run("recall", func(t *testing.T) {
				_, h := f.openTask(t, "joan", "mika")
				hits, err := f.g.Recall(ctx, agentapi.RecallRequest{Handle: h, Text: "safe combination"})
				if err != nil {
					t.Fatalf("Recall: %v", err)
				}
				got := len(hits) > 0
				if got != tc.wantDisclosed {
					t.Fatalf("Recall disclosed=%v, want %v: %+v", got, tc.wantDisclosed, hits)
				}
			})

			t.Run("invoke_recall_tool", func(t *testing.T) {
				_, h := f.openTask(t, "joan", "mika")
				args, err := json.Marshal(map[string]any{"text": "safe combination", "limit": 5})
				if err != nil {
					t.Fatal(err)
				}
				res, err := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: h, Tool: "recall", Args: args})
				if err != nil {
					t.Fatalf("Invoke: %v", err)
				}
				if tc.wantDisclosed {
					if res.Outcome != agentapi.OutcomeOK || !strings.Contains(res.Content, marker) {
						t.Fatalf("expected the marker disclosed via Invoke: %+v", res)
					}
					return
				}
				if res.Outcome != agentapi.OutcomeDenied {
					t.Fatalf("Invoke outcome = %s, want denied: %+v", res.Outcome, res)
				}
				// Same wording as the provider wrapper's refusal
				// (privacy_test.go:32 pins runtimedb.ErrPrivacy).
				if !strings.Contains(res.Reason, runtimedb.ErrPrivacy.Error()) {
					t.Fatalf("Invoke reason = %q, want it to carry runtimedb.ErrPrivacy's wording", res.Reason)
				}
				if strings.Contains(res.Content, marker) || strings.Contains(res.Reason, marker) {
					t.Fatalf("the restricted content itself leaked into the refusal: %+v", res)
				}
			})
		})
	}
}

// TestDisclosureFilterOverManifest is Manifest's own case: a memory
// preamble is disclosure exactly like a recall hit, but a manifest
// cannot hand back "everything except the restricted part" the way a
// hit list can — a runtime with no system prompt at all cannot run — so
// the whole call is refused instead, the gateway's version of the
// provider wrapper failing the whole turn (privacy_test.go:32).
func TestDisclosureFilterOverManifest(t *testing.T) {
	const marker = "family passphrase: gingerbread horizon"
	for _, tc := range []struct {
		name   string
		class  agentapi.ProcessingClass
		wantOK bool
	}{
		{"cloud is refused the manifest", agentapi.ProcessingCloud, false},
		{"undeclared fails closed", "", false},
		{"local_only gets the preamble", agentapi.ProcessingLocalOnly, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.g.Gateways = butlerGatewayFunc(f)
			if tc.class != "" {
				setProcessingClass(t, f, "joan", tc.class)
			}
			putRestrictedMemory(t, f, runtimedb.MemoryPreference, marker)
			_, h := f.openTask(t, "joan", "mika")

			m, err := f.g.Manifest(context.Background(), h)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("Manifest: %v", err)
				}
				if !strings.Contains(m.SystemPrompt, marker) {
					t.Fatalf("local_only manifest is missing its own preamble: %q", m.SystemPrompt)
				}
				return
			}
			if err == nil {
				t.Fatalf("Manifest should have refused a restricted preamble, got %+v", m)
			}
			if !errors.Is(err, runtimedb.ErrPrivacy) {
				t.Fatalf("Manifest error = %v, want it to wrap runtimedb.ErrPrivacy", err)
			}
		})
	}
}
