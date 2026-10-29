package api

// This file is the repository-owned, synthetic ADR-W049 conformance gate.
// It deliberately uses only fake issuer claims and example.internal URLs.
// Passing it is not evidence that Authelia, Hermes, or a household deployment
// was configured or exercised; issue #68 retains that owner-operated proof.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi/httpclient"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentgateway"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentmcp"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerstore"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/oauthresource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

const syntheticOAuthBearer = "opaque-oauth-access-token"

type oauthConformanceFixture struct {
	h            *harness
	introspector *fakeOAuthIntrospector
	identity     oauthresource.Identity
	agent        runtimedb.Butler
	ledger       *butlerstore.Ledger
	gateway      *agentgateway.Gateway
}

func newOAuthConformanceFixture(t *testing.T) oauthConformanceFixture {
	t.Helper()
	h, introspector := activeOAuthHarness(t)
	auth, _, err := h.s.OAuthResource.Resolve(context.Background(), syntheticOAuthBearer)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := h.s.DB.AcquireCoordinator("oauth-conformance")
	if err != nil {
		t.Fatal(err)
	}
	ledger := butlerstore.New(h.s.DB, lease)
	ledger.DerivedAuthorityChecks()
	gateway := &agentgateway.Gateway{
		DB: h.s.DB, Ledger: ledger, Gateways: conformanceGateways(), Clock: conformanceClock,
		Proposals: h.s.ProposalDeps(),
	}
	h.s.AgentGateway = agentgateway.InProcess(gateway)
	h.s.Ledger = ledger
	agent, err := h.s.DB.GetButlerBySeq(auth.Identity.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	return oauthConformanceFixture{h: h, introspector: introspector, identity: auth.Identity,
		agent: agent, ledger: ledger, gateway: gateway}
}

func (f oauthConformanceFixture) client(baseURL string) *httpclient.Client {
	return &httpclient.Client{BaseURL: baseURL, Bearer: syntheticOAuthBearer}
}

func (f oauthConformanceFixture) admit(t *testing.T, client *httpclient.Client, objective, session, key string) agentapi.TaskAdmission {
	t.Helper()
	admitted, err := client.AdmitTask(context.Background(), agentapi.TaskAdmissionRequest{
		Objective: objective, ClientSessionID: session, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("AdmitTask: %v", err)
	}
	return admitted
}

func mcpResult(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("MCP status = %d: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil || envelope.Error != nil {
		t.Fatalf("MCP response = %s, error=%v decode=%v", rec.Body.String(), envelope.Error, err)
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		t.Fatalf("MCP result = %s: %v", envelope.Result, err)
	}
}

func toolNames(specs []agentapi.ToolSpec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	sort.Strings(names)
	return names
}

func mcpToolNames(tools []agentmcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestExternalOAuthSyntheticConformance(t *testing.T) {
	t.Run("direct admission, REST-MCP parity, audit identity, and Plan ceiling", func(t *testing.T) {
		f := newOAuthConformanceFixture(t)
		client := f.client(f.h.srv.URL)
		admitted := f.admit(t, client, "inspect synthetic state", "parity-session", "turn-1")

		manifest, err := client.Manifest(context.Background(), admitted.Handle)
		if err != nil {
			t.Fatal(err)
		}
		var listed agentmcp.ToolsListResult
		mcpResult(t, doOAuthMCP(t, f.h, admitted.MCPSession, "https://keystone.example.internal",
			agentmcp.ProtocolVersion, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), &listed)
		if got, want := mcpToolNames(listed.Tools), toolNames(manifest.Tools); !reflect.DeepEqual(got, want) {
			t.Fatalf("MCP tools = %v, REST tools = %v", got, want)
		}
		if got, want := toolNames(manifest.Tools), []string{"peek", "propose_change", "remember"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("assigned discovery = %v, want only %v", got, want)
		}

		restResult, err := client.Invoke(context.Background(), agentapi.InvokeRequest{Handle: admitted.Handle, Tool: "peek"})
		if err != nil || restResult.Outcome != agentapi.OutcomeOK {
			t.Fatalf("REST invoke = %+v, %v", restResult, err)
		}
		var mcpCall agentmcp.ToolCallResult
		mcpResult(t, doOAuthMCP(t, f.h, admitted.MCPSession, "", agentmcp.ProtocolVersion,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"peek","arguments":{}}}`), &mcpCall)
		if len(mcpCall.Content) != 1 || mcpCall.IsError || mcpCall.Content[0].Text != restResult.Content {
			t.Fatalf("MCP result = %+v, REST result = %+v", mcpCall, restResult)
		}

		proposal, err := client.Invoke(context.Background(), agentapi.InvokeRequest{
			Handle: admitted.Handle, Tool: "propose_change", Args: json.RawMessage(`{"target":"n1"}`),
		})
		if err != nil || proposal.Outcome != agentapi.OutcomeOK || proposal.Proposal == nil {
			t.Fatalf("proposal invoke = %+v, %v", proposal, err)
		}
		stored, err := f.h.s.DB.GetPlan(proposal.Proposal.ID)
		task, taskErr := f.h.s.DB.GetButlerTask(admitted.TaskID)
		if err != nil || taskErr != nil || stored.State != runtimedb.PlanProposed || stored.Principal != f.identity.RuntimePrincipal || stored.ActingFor != task.Author {
			t.Fatalf("stored proposal = %+v, %v", stored, err)
		}

		trace, err := f.h.s.DB.ListButlerTrace(admitted.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		toolResults := 0
		for _, event := range trace {
			if event.Type == runtimedb.TraceToolResult {
				toolResults++
				if event.Actor != task.Author || event.PermissionLabel != f.agent.Name || event.Audience != runtimedb.ButlerAudiencePrivate {
					t.Fatalf("tool audit identity = %+v", event)
				}
			}
		}
		if toolResults != 3 {
			t.Fatalf("tool result audit events = %d, want 3", toolResults)
		}
		proof, _ := json.Marshal(struct {
			REST  agentapi.InvokeResult
			MCP   agentmcp.ToolCallResult
			Trace []runtimedb.ButlerTraceEvent
			Plan  runtimedb.PlanRecord
		}{restResult, mcpCall, trace, stored})
		for _, forbidden := range []string{syntheticOAuthBearer, "byte-exact-subject"} {
			if bytes.Contains(proof, []byte(forbidden)) {
				t.Fatalf("conformance evidence disclosed %q", forbidden)
			}
		}
	})

	t.Run("cross-owner isolation and closed OAuth profile", func(t *testing.T) {
		f := newOAuthConformanceFixture(t)
		client := f.client(f.h.srv.URL)
		admitted := f.admit(t, client, "inspect synthetic state", "isolation-session", "turn-1")

		nativeSeq, err := f.h.s.DB.CreateButler(runtimedb.Butler{Name: "mika-native", Surface: runtimedb.ButlerSurfaceFamily,
			AgentClass: runtimedb.AgentClassMember, Model: "external-model"}, "root-admin")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.h.s.DB.GrantButlerAccess("mika-native", "mika", "root-admin"); err != nil {
			t.Fatal(err)
		}
		if err := f.h.s.DB.SetButlerRuntime(nativeSeq, runtimedb.ButlerRuntime{Type: agentapi.RuntimeNative,
			Delivery: agentapi.DeliveryPush, ProcessingClass: agentapi.ProcessingLocalOnly}, "root-admin"); err != nil {
			t.Fatal(err)
		}
		native, err := f.h.s.DB.GetButler("mika-native")
		if err != nil {
			t.Fatal(err)
		}
		foreignTask, foreignHandle := openAgentTask(t, f.h.s.DB, f.ledger, native)
		nativeResult, err := f.h.s.AgentGateway.Invoke(context.Background(), agentapi.InvokeRequest{Handle: foreignHandle, Tool: "peek"})
		if err != nil || nativeResult.Outcome != agentapi.OutcomeOK {
			t.Fatalf("native Agent canonical gateway = %+v, %v", nativeResult, err)
		}
		denied, err := client.Invoke(context.Background(), agentapi.InvokeRequest{Handle: foreignHandle, Tool: "peek"})
		if err != nil || denied.Outcome != agentapi.OutcomeDenied {
			t.Fatalf("cross-Person/Agent invoke = %+v, %v", denied, err)
		}

		unbound := doOAuthMCP(t, f.h, "", "", agentmcp.ProtocolVersion,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"peek"}}`)
		if unbound.Code != http.StatusForbidden {
			t.Fatalf("sole-pending MCP borrowing = %d %s", unbound.Code, unbound.Body.String())
		}
		wrongSession := doOAuthMCP(t, f.h, "not-this-task", "", agentmcp.ProtocolVersion,
			`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"peek"}}`)
		if wrongSession.Code != http.StatusForbidden {
			t.Fatalf("wrong MCP session = %d %s", wrongSession.Code, wrongSession.Body.String())
		}

		original := f.introspector.claims
		mutations := []struct {
			name string
			edit func(*oauthresource.Claims)
		}{
			{"issuer", func(c *oauthresource.Claims) { c.Issuer = "https://other.example" }},
			{"client", func(c *oauthresource.Claims) { c.ClientID += "-other" }},
			{"resource", func(c *oauthresource.Claims) { c.Audience = []string{"https://other.example/mcp"} }},
			{"scope", func(c *oauthresource.Claims) { c.Scope = []string{"openid"} }},
		}
		for _, tc := range mutations {
			t.Run(tc.name, func(t *testing.T) {
				f.introspector.claims = original
				tc.edit(&f.introspector.claims)
				rec := doOAuthMCP(t, f.h, admitted.MCPSession, "", agentmcp.ProtocolVersion,
					`{"jsonrpc":"2.0","id":5,"method":"tools/list"}`)
				if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
					t.Fatalf("wrong %s = %d %s", tc.name, rec.Code, rec.Body.String())
				}
			})
		}
		f.introspector.claims = original

		body := &observedReader{r: strings.NewReader(`{"private":"synthetic"}`)}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/plans", body)
		req.Header.Set("Authorization", "Bearer "+syntheticOAuthBearer)
		rec := httptest.NewRecorder()
		f.h.s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || body.read {
			t.Fatalf("general REST bypass = %d body-read=%v", rec.Code, body.read)
		}
		if tasks, err := f.h.s.DB.ListOpenButlerTasksByButler(native.Name); err != nil || len(tasks) != 1 || tasks[0].ID != foreignTask {
			t.Fatalf("native tasks changed = %+v, %v", tasks, err)
		}
		if deliveries, err := f.h.s.DB.ListAgentDeliveries(native.Seq, 10); err != nil || len(deliveries) != 0 {
			t.Fatalf("external work transferred to native Agent: %+v, %v", deliveries, err)
		}
	})

	t.Run("mid-boundary revocation and issuer outage", func(t *testing.T) {
		cases := []struct {
			name   string
			revoke func(testing.TB, oauthConformanceFixture)
		}{
			{"token", func(_ testing.TB, f oauthConformanceFixture) { f.introspector.claims.Active = false }},
			{"client", func(_ testing.TB, f oauthConformanceFixture) { f.introspector.claims.ClientID += "-revoked" }},
			{"Person", func(t testing.TB, f oauthConformanceFixture) {
				if err := f.h.s.DB.DisableButlerMember("sandy", "root-admin"); err != nil {
					t.Fatal(err)
				}
			}},
			{"registration", func(t testing.TB, f oauthConformanceFixture) {
				if _, err := f.h.s.DB.RevokeExternalClientRegistration(f.identity.RegistrationID, "root-admin"); err != nil {
					t.Fatal(err)
				}
			}},
			{"issuer outage", func(_ testing.TB, f oauthConformanceFixture) {
				f.introspector.err = errors.New("synthetic issuer outage")
			}},
		}
		for _, tc := range cases {
			t.Run("invocation/"+tc.name, func(t *testing.T) {
				f := newOAuthConformanceFixture(t)
				admitted := f.admit(t, f.client(f.h.srv.URL), "inspect synthetic state", "revoke-session", "turn-1")
				f.introspector.calls = 0
				f.introspector.beforeCall = func(call int) {
					if call == 2 {
						tc.revoke(t, f)
					}
				}
				rec := doOAuthMCP(t, f.h, admitted.MCPSession, "", agentmcp.ProtocolVersion,
					`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"peek"}}`)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("mid-invocation %s = %d %s", tc.name, rec.Code, rec.Body.String())
				}
				if records := f.gateway.Records(admitted.TaskID); len(records) != 0 {
					t.Fatalf("protected work ran after %s: %+v", tc.name, records)
				}
			})

			t.Run("waiting poll/"+tc.name, func(t *testing.T) {
				f := newOAuthConformanceFixture(t)
				f.h.s.deliveryPollWaiting = func() { tc.revoke(t, f) }
				req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/deliveries?wait=1s", nil)
				req.Header.Set("Authorization", "Bearer "+syntheticOAuthBearer)
				rec := httptest.NewRecorder()
				f.h.s.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("mid-poll %s = %d %s", tc.name, rec.Code, rec.Body.String())
				}
				if deliveries, err := f.h.s.DB.ListAgentDeliveries(f.agent.Seq, 10); err != nil || len(deliveries) != 0 {
					t.Fatalf("%s transferred or disclosed work: %+v, %v", tc.name, deliveries, err)
				}
			})
		}

		t.Run("acknowledgment/registration", func(t *testing.T) {
			f := newOAuthConformanceFixture(t)
			client := f.client(f.h.srv.URL)
			admitted := f.admit(t, client, "inspect synthetic state", "ack-session", "turn-1")
			if _, err := f.h.s.DB.RecordAgentDeliveryForTask(f.agent.Seq, agentapi.DeliveryTurn, admitted.TaskID,
				`{"text":"synthetic delivery"}`); err != nil {
				t.Fatal(err)
			}
			deliveries, err := client.Deliveries(context.Background(), "", 0)
			if err != nil || len(deliveries) != 1 {
				t.Fatalf("delivery before revocation = %+v, %v", deliveries, err)
			}
			if _, err := f.h.s.DB.RevokeExternalClientRegistration(f.identity.RegistrationID, "root-admin"); err != nil {
				t.Fatal(err)
			}
			if err := client.Ack(context.Background(), deliveries[0].ID); err == nil {
				t.Fatal("registration revocation did not fence acknowledgment")
			}
			stored, err := f.h.s.DB.ListAgentDeliveries(f.agent.Seq, 10)
			if err != nil || len(stored) != 1 || !stored[0].AckedAt.IsZero() {
				t.Fatalf("revoked acknowledgment changed delivery: %+v, %v", stored, err)
			}
		})
	})

	t.Run("restart retry and lost acknowledgment", func(t *testing.T) {
		f := newOAuthConformanceFixture(t)
		first := f.admit(t, f.client(f.h.srv.URL), "inspect synthetic state", "restart-session", "turn-1")
		if _, err := f.h.s.DB.RecordAgentDeliveryForTask(f.agent.Seq, agentapi.DeliveryTurn, first.TaskID,
			`{"text":"synthetic delivery"}`); err != nil {
			t.Fatal(err)
		}

		f.h.srv.Close()
		restarted := httptest.NewServer(f.h.s.Handler())
		t.Cleanup(restarted.Close)
		client := f.client(restarted.URL)
		retry := f.admit(t, client, "inspect synthetic state", "restart-session", "turn-1")
		if retry != first {
			t.Fatalf("restart retry = %+v, want existing %+v", retry, first)
		}
		tasks, err := f.h.s.DB.ListOpenButlerTasksByButler(f.agent.Name)
		if err != nil || len(tasks) != 1 || tasks[0].ID != first.TaskID {
			t.Fatalf("durable task effect duplicated: %+v, %v", tasks, err)
		}
		firstOffer, err := client.Deliveries(context.Background(), "", 0)
		if err != nil || len(firstOffer) != 1 {
			t.Fatalf("first offer = %+v, %v", firstOffer, err)
		}

		restarted.Close()
		restartedAgain := httptest.NewServer(f.h.s.Handler())
		t.Cleanup(restartedAgain.Close)
		client = f.client(restartedAgain.URL)
		reoffered, err := client.Deliveries(context.Background(), "", 0)
		if err != nil || len(reoffered) != 1 || reoffered[0].ID != firstOffer[0].ID {
			t.Fatalf("lost acknowledgment was skipped: %+v, %v", reoffered, err)
		}
		if err := client.Ack(context.Background(), reoffered[0].ID); err != nil {
			t.Fatal(err)
		}
		settled, err := client.Deliveries(context.Background(), "", 0)
		if err != nil || len(settled) != 0 {
			t.Fatalf("acknowledged delivery reoffered: %+v, %v", settled, err)
		}
	})
}
