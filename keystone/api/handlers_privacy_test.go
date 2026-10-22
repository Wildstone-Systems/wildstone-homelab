package api

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMemoryPrivacyRequiresFreshOwnerAndPreservesPersonalOwnership(t *testing.T) {
	h := freshnessHarness(t)
	for _, owner := range []string{"household", "sandy"} {
		scope := runtimedb.MemoryScopeHousehold
		audience := runtimedb.ButlerAudienceShared
		if owner != "household" {
			scope = runtimedb.MemoryScopePersonal
			audience = runtimedb.ButlerAudiencePrivate
		}
		seq, err := h.s.DB.RecordMemory(runtimedb.MemoryRecord{Scope: scope, Kind: "fact", Content: "jasmine", Owner: owner, Author: "sandy", Audience: audience, PermissionLabel: "household", SourceKind: "conversation", SourceID: "test"})
		if err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"seq":%d,"localOnly":true,"trustedOnly":true}`, seq)
		fresh := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
		code, out := h.sessionRequest(http.MethodPut, "/api/v1/butler/memory/privacy", body, "mika", fresh)
		want := http.StatusOK
		if owner == "sandy" {
			want = http.StatusNotFound
		}
		if code != want {
			t.Fatalf("%s: %d %v", owner, code, out)
		}
		stale := sessionCookie(h.s.StartConsoleSession("mika", time.Now().Add(-24*time.Hour)))
		code, _ = h.sessionRequest(http.MethodPut, "/api/v1/butler/memory/privacy", body, "mika", stale)
		if code == http.StatusOK {
			t.Fatal("stale session changed privacy")
		}
	}
}

func TestHomepagePrivacyControlsBelongToThePerson(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy", "maya")
	own := remember(t, h, "sandy", "jasmine", runtimedb.ButlerAudiencePrivate)
	other := remember(t, h, "maya", "mint", runtimedb.ButlerAudiencePrivate)
	for _, seq := range []int64{own, other} {
		body := fmt.Sprintf(`{"seq":%d,"localOnly":true,"trustedOnly":true}`, seq)
		res := h.member(http.MethodPut, "/api/v1/assistant/memory/privacy", "sandy", "assistant_users", strings.NewReader(body), "application/json")
		res.Body.Close()
		if seq == own && res.StatusCode != http.StatusOK {
			t.Fatalf("own policy: %d", res.StatusCode)
		}
		if seq == other && res.StatusCode == http.StatusOK {
			t.Fatal("changed another person's privacy")
		}
	}
}

func TestServicePrincipalCannotClearPrivacyForItsPerson(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	seq := remember(t, h, "sandy", "jasmine", runtimedb.ButlerAudiencePrivate)
	if err := h.s.DB.SetMemoryPrivacy("sandy", seq, runtimedb.PrivacyPolicy{LocalOnly: true, TrustedOnly: true}); err != nil {
		t.Fatal(err)
	}
	for _, role := range []runtimedb.Role{runtimedb.RoleButler, runtimedb.RoleOperator, runtimedb.RoleAdmin} {
		h.tokens[role] = h.console(role, "sandy")
		response := h.do(role, http.MethodPut, "/api/v1/butler/memory/privacy", map[string]any{"seq": seq, "localOnly": false, "trustedOnly": false}, nil)
		if response.StatusCode == http.StatusOK {
			t.Errorf("%s changed privacy", role)
		}
	}
	policy, err := h.s.DB.MemoryPrivacy(seq)
	if err != nil || !policy.LocalOnly || !policy.TrustedOnly {
		t.Fatalf("policy weakened: %+v %v", policy, err)
	}
}
func TestHomepageOwnerCannotClearHouseholdPrivacy(t *testing.T) {
	h := homeHarness(t)
	h.owner("sandy")
	seq := remember(t, h, "sandy", "jasmine", runtimedb.ButlerAudienceShared)
	if err := h.s.DB.SetMemoryPrivacy("sandy", seq, runtimedb.PrivacyPolicy{LocalOnly: true, TrustedOnly: true}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"seq":%d,"localOnly":false,"trustedOnly":false}`, seq)
	response := h.member(http.MethodPut, "/api/v1/assistant/memory/privacy", "sandy", "assistant_users", strings.NewReader(body), "application/json")
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("Homepage changed Household privacy: %d", response.StatusCode)
	}
	policy, err := h.s.DB.MemoryPrivacy(seq)
	if err != nil || !policy.LocalOnly || !policy.TrustedOnly {
		t.Fatalf("policy weakened: %+v %v", policy, err)
	}
}

func TestChangingOnePrivacyFlagPreservesTheOtherDirectFlag(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	seq := remember(t, h, "sandy", "jasmine", runtimedb.ButlerAudiencePrivate)
	if err := h.s.DB.SetMemoryPrivacy("sandy", seq, runtimedb.PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"seq":%d,"trustedOnly":true}`, seq)
	response := h.member(http.MethodPut, "/api/v1/assistant/memory/privacy", "sandy", "assistant_users", strings.NewReader(body), "application/json")
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode)
	}
	p, err := h.s.DB.MemoryPrivacy(seq)
	if err != nil || !p.LocalOnly || !p.TrustedOnly {
		t.Fatalf("unrelated flag changed: %+v %v", p, err)
	}
}

func TestLegacyFamilyTurnCarriesPrivacyToActionBoundary(t *testing.T) {
	for _, protected := range []bool{false, true} {
		t.Run(fmt.Sprint(protected), func(t *testing.T) {
			h := newHarness(t)
			h.wireHouseholdButler([]*provider.ChatResponse{{Text: "jasmine"}, {ToolCalls: []provider.ToolCall{{ID: "tc-1", Name: "do_household_action", Input: map[string]any{"tool": "notifications.notify", "params": map[string]any{"topic": "household", "message": "jasmine"}}}}}, {Text: "finished"}}, runtimedb.ButlerModeFull)
			first, _, err := h.s.runFamilySession(context.Background(), "hello", "op", "", "console", false, nil)
			if err != nil {
				t.Fatal(err)
			}
			id := first.(askResponse).SessionID
			seq, err := h.s.DB.RecordMemory(runtimedb.MemoryRecord{Scope: runtimedb.MemoryScopePersonal, Kind: "fact", Content: "jasmine", Owner: "op", Author: "op", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "op", SourceKind: "conversation", SourceID: id})
			if err != nil {
				t.Fatal(err)
			}
			if err = h.s.DB.SetMemoryPrivacy("op", seq, runtimedb.PrivacyPolicy{TrustedOnly: protected}); err != nil {
				t.Fatal(err)
			}
			delivered := 0
			h.s.ButlerHost.Household.Actions = func(ctx context.Context, _ butler.HouseholdActionCall) (string, error) {
				if err := runtimedb.CheckPrivacyDelivery(ctx, runtimedb.MemoryDestination{}); err != nil {
					return "", err
				}
				delivered++
				return "delivered", nil
			}
			_, _, err = h.s.runFamilySession(context.Background(), "notify", "op", "", "console", false, &turnContext{SessionID: id, History: []provider.Message{{Role: "assistant", Text: "jasmine"}}})
			if err != nil {
				t.Fatal(err)
			}
			if (delivered == 1) == protected {
				t.Fatalf("protected=%v deliveries=%d", protected, delivered)
			}
		})
	}
}

type legacyPrivacyChangingProvider struct {
	provider.Provider
	change func() error
}

func (p legacyPrivacyChangingProvider) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	answer, err := p.Provider.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	if err = p.change(); err != nil {
		return nil, err
	}
	return answer, nil
}
func TestLegacyTurnRechecksPolicyAfterModelReturns(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprint(admin), func(t *testing.T) {
			h := newHarness(t)
			h.wireHouseholdButler(answerTurns("jasmine", "jasmine"), runtimedb.ButlerModeFull)
			run := func(prior *turnContext) (any, int, error) {
				if admin {
					return h.s.runAdminButlerSession(context.Background(), "hello", "op", "console", prior)
				}
				return h.s.runFamilySession(context.Background(), "hello", "op", "", "console", false, prior)
			}
			first, _, err := run(nil)
			if err != nil {
				t.Fatal(err)
			}
			id := first.(askResponse).SessionID
			seq, err := h.s.DB.RecordMemory(runtimedb.MemoryRecord{Scope: runtimedb.MemoryScopePersonal, Kind: "fact", Content: "jasmine", Owner: "op", Author: "op", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "op", SourceKind: "conversation", SourceID: id})
			if err != nil {
				t.Fatal(err)
			}
			h.s.ButlerHost.Provider = legacyPrivacyChangingProvider{Provider: h.s.ButlerHost.Provider, change: func() error { return h.s.DB.SetMemoryPrivacy("op", seq, runtimedb.PrivacyPolicy{LocalOnly: true}) }}
			if out, code, err := run(&turnContext{SessionID: id}); err == nil || code == 200 {
				t.Fatalf("changed policy accepted: %d %+v %v", code, out, err)
			}
		})
	}
}

func TestChatReplyRetainsPrivacyThroughOfflineAdapter(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(answerTurns("jasmine"), runtimedb.ButlerModeFull)
	at := h.agentAt(t, "family", "telegram")
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	resp, out := h.send(runtimedb.RoleConcierge, map[string]any{"transport": "telegram", "account": "12345", "conversation": "12345", "text": "hello", "messageId": "privacy-offline"})
	if resp.StatusCode != http.StatusAccepted || out.Queued == 0 {
		t.Fatalf("reply not queued: %d %+v", resp.StatusCode, out)
	}
	session, _, err := h.s.DB.ChatThreadSession(at, "12345", "sam")
	if err != nil {
		t.Fatal(err)
	}
	seq, err := h.s.DB.RecordMemory(runtimedb.MemoryRecord{Scope: runtimedb.MemoryScopePersonal, Kind: "fact", Content: "jasmine", Owner: "sam", Author: "sam", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "sam", SourceKind: "conversation", SourceID: session})
	if err != nil {
		t.Fatal(err)
	}
	messages := h.poll(t, runtimedb.RoleConcierge, "")
	if len(messages) != 1 || !strings.Contains(messages[0].Body, "jasmine") {
		t.Fatalf("positive reply control: %v", messages)
	}
	if err = h.s.DB.SetMemoryPrivacy("sam", seq, runtimedb.PrivacyPolicy{TrustedOnly: true}); err != nil {
		t.Fatal(err)
	}
	messages = h.poll(t, runtimedb.RoleConcierge, "")
	if len(messages) != 1 || strings.Contains(messages[0].Body, "jasmine") {
		t.Fatalf("offline adapter leaked reply: %v", messages)
	}
}

func TestSessionChoicesDoNotDiscloseProtectedSubjects(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(fmt.Sprint(before), func(t *testing.T) {
			h := newHarness(t)
			h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
			at := h.agentAt(t, "family", "telegram")
			h.bindAccount(t, "family", "sam", "telegram", "12345")
			session := insertCandidateSession(t, h, at, "sam", "jasmine private subject")
			seq, err := h.s.DB.RecordMemory(runtimedb.MemoryRecord{Scope: runtimedb.MemoryScopePersonal, Kind: "fact", Content: "jasmine", Owner: "sam", Author: "sam", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "sam", SourceKind: "conversation", SourceID: session})
			if err != nil {
				t.Fatal(err)
			}
			restrict := func() {
				if err := h.s.DB.SetMemoryPrivacy("sam", seq, runtimedb.PrivacyPolicy{TrustedOnly: true}); err != nil {
					t.Fatal(err)
				}
			}
			if before {
				restrict()
			}
			_, out := h.send(runtimedb.RoleConcierge, map[string]any{"transport": "telegram", "account": "12345", "text": "/sessions", "messageId": "private-list"})
			if out.Queued == 0 {
				t.Fatal("missing safe response")
			}
			if !before {
				messages := h.poll(t, runtimedb.RoleConcierge, "")
				if len(messages) != 1 || !strings.Contains(messages[0].Body, "jasmine") {
					t.Fatalf("positive choices: %v", messages)
				}
				restrict()
			}
			messages := h.poll(t, runtimedb.RoleConcierge, "")
			if len(messages) != 1 || strings.Contains(messages[0].Body, "jasmine") {
				t.Fatalf("protected subject disclosed (before=%v): %v", before, messages)
			}
		})
	}
}

type legacyMemorySourceProvider struct {
	provider.Provider
	memory int64
}

func (p legacyMemorySourceProvider) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	if err := runtimedb.TrackMemoryPrivacy(ctx, p.memory, nil); err != nil {
		return nil, err
	}
	return p.Provider.Chat(ctx, req)
}
func TestLegacyFirstTurnKeepsHandleAndSourcesAfterStorageRace(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprint(admin), func(t *testing.T) {
			h := newHarness(t)
			h.wireHouseholdButler(answerTurns("jasmine"), runtimedb.ButlerModeFull)
			source, err := h.s.DB.InsertButlerSession(runtimedb.ButlerSession{Principal: "butler", ActingFor: "op", Surface: runtimedb.ButlerSurfaceFamily, Mode: runtimedb.ButlerModeFull})
			if err != nil {
				t.Fatal(err)
			}
			memory, err := h.s.DB.RecordMemory(runtimedb.MemoryRecord{Scope: runtimedb.MemoryScopePersonal, Kind: "fact", Content: "jasmine", Owner: "op", Author: "op", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "op", SourceKind: "conversation", SourceID: source})
			if err != nil {
				t.Fatal(err)
			}
			h.s.ButlerHost.Provider = legacyMemorySourceProvider{Provider: h.s.ButlerHost.Provider, memory: memory}
			if _, err = h.s.DB.ExecForTest(fmt.Sprintf("CREATE TRIGGER tighten_at_session_storage AFTER INSERT ON butler_sessions BEGIN INSERT INTO content_privacy(object,local_only) VALUES('m:%d',1) ON CONFLICT(object) DO UPDATE SET local_only=1; END", memory)); err != nil {
				t.Fatal(err)
			}
			var out any
			var code int
			if admin {
				out, code, err = h.s.runAdminButlerSession(context.Background(), "hello", "op", "console", nil)
			} else {
				out, code, err = h.s.runFamilySession(context.Background(), "hello", "op", "", "console", false, nil)
			}
			response, ok := out.(askResponse)
			if err == nil || code != http.StatusForbidden || !ok || response.SessionID == "" || response.FinalText != "" {
				t.Fatalf("unsafe/stranded response: %d %+v %v", code, out, err)
			}
			if err = h.s.DB.CheckSessionPrivacy(response.SessionID, runtimedb.ModelCloud, runtimedb.MemoryDestination{BuiltIn: true}); err == nil {
				t.Fatal("first-turn history lost source policy")
			}
		})
	}
}

func TestAPIMemoryDisclosureRequiresHumanConsoleForTrustedOnly(t *testing.T) {
	h := freshnessHarness(t)
	memory := seedMemory(t, h, "jasmine", "household", runtimedb.ButlerAudienceShared)
	if _, err := h.s.DB.CorrectMemory(memory, runtimedb.MemoryRecord{Content: "jasmine updated", SourceKind: "correction", SourceID: "fixture"}, "mika"); err != nil {
		t.Fatal(err)
	}
	plain := seedMemory(t, h, "ordinary", "household", runtimedb.ButlerAudienceShared)
	if _, err := h.s.DB.CorrectMemory(plain, runtimedb.MemoryRecord{Content: "ordinary updated", SourceKind: "correction", SourceID: "plain-fixture"}, "mika"); err != nil {
		t.Fatal(err)
	}
	session := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	for _, protected := range []bool{false, true} {
		if err := h.s.DB.SetMemoryPrivacy("mika", memory, runtimedb.PrivacyPolicy{TrustedOnly: protected}); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/api/v1/butler/memory", "/api/v1/butler/memory/export", "/api/v1/butler/memory/digest"} {
			for _, role := range []runtimedb.Role{runtimedb.RoleOperator, runtimedb.RoleButler} {
				token := h.console(role, "mika")
				var body json.RawMessage
				response := h.doToken(token, http.MethodGet, path, nil, &body)
				want := http.StatusOK
				if !strings.Contains(string(body), "ordinary") || (protected && strings.Contains(string(body), "jasmine")) {
					t.Fatalf("incorrect filtering %s protected=%v: %s", path, protected, body)
				}
				if response.StatusCode != want {
					t.Fatalf("protected=%v role=%s path=%s status=%d want=%d", protected, role, path, response.StatusCode, want)
				}
			}
			code, _ := h.sessionRequest(http.MethodGet, path, "", "mika", session)
			if code != http.StatusOK {
				t.Fatalf("human Console %s: %d", path, code)
			}
		}
	}
}

func TestAPIHistoryDoesNotBypassTrustedMemory(t *testing.T) {
	h := freshnessHarness(t)
	h.person("sam")
	task := failedTurnTask(t, h)
	session, err := h.s.DB.InsertButlerSession(runtimedb.ButlerSession{Principal: "butler", ActingFor: "sam", Surface: runtimedb.ButlerSurfaceFamily, Mode: runtimedb.ButlerModeFull, FinalText: "jasmine"})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.s.DB.SetButlerTaskSession(task, session); err != nil {
		t.Fatal(err)
	}
	memory, err := h.s.DB.RecordMemory(runtimedb.MemoryRecord{Scope: runtimedb.MemoryScopePersonal, Kind: "fact", Content: "jasmine", Owner: "sam", Author: "sam", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "sam", SourceKind: "conversation", SourceID: task})
	if err != nil {
		t.Fatal(err)
	}
	// The follow-up uses protected history but performs no memory recall of its own.
	task = failedTurnTask(t, h)
	if err = h.s.DB.SetButlerTaskSession(task, session); err != nil {
		t.Fatal(err)
	}
	plainTask := failedTurnTask(t, h)
	plainSession, err := h.s.DB.InsertButlerSession(runtimedb.ButlerSession{Principal: "butler", ActingFor: "sam", Surface: runtimedb.ButlerSurfaceFamily, Mode: runtimedb.ButlerModeFull, FinalText: "ordinary"})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.s.DB.SetButlerTaskSession(plainTask, plainSession); err != nil {
		t.Fatal(err)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/tasks/"+plainTask+"/evaluation", map[string]any{"name": "ordinary", "expected": "ordinary", "summary": "ordinary"}, nil); res.StatusCode != http.StatusOK {
		t.Fatal(res.Status)
	}
	if _, err = h.s.DB.EnableButlerDebugCapture(plainTask, "mika", "ordinary", time.Hour); err != nil {
		t.Fatal(err)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/tasks/"+task+"/evaluation", map[string]any{"name": "privacy-history", "expected": "protect the history", "summary": "jasmine"}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("seed evaluation: %d", res.StatusCode)
	}
	if _, err = h.s.DB.EnableButlerDebugCapture(task, "mika", "jasmine", time.Hour); err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	for _, protected := range []bool{false, true} {
		if err = h.s.DB.SetMemoryPrivacy("sam", memory, runtimedb.PrivacyPolicy{TrustedOnly: protected}); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/api/v1/butler/sessions/" + session, "/api/v1/butler/tasks/" + task, "/api/v1/butler/sessions", "/api/v1/butler/tasks", "/api/v1/butler/evaluations", "/api/v1/butler/debug"} {
			var body json.RawMessage
			res := h.do(runtimedb.RoleAdmin, http.MethodGet, path, nil, &body)
			want := http.StatusOK
			single := path == "/api/v1/butler/sessions/"+session || path == "/api/v1/butler/tasks/"+task
			if protected && single {
				want = http.StatusForbidden
			}
			if res.StatusCode != want {
				t.Fatalf("protected=%v path=%s status=%d", protected, path, res.StatusCode)
			}
			if !single {
				plainID, hiddenID := plainTask, task
				if path == "/api/v1/butler/sessions" {
					plainID, hiddenID = plainSession, session
				}
				if !strings.Contains(string(body), `"`+plainID+`"`) || (protected && strings.Contains(string(body), `"`+hiddenID+`"`)) {
					t.Fatalf("incorrect collection %s protected=%v: %s", path, protected, body)
				}
			}
			if code, _ := h.sessionRequest(http.MethodGet, path, "", "mika", cookie); code != http.StatusOK {
				t.Fatalf("Console denied: %s %d", path, code)
			}
		}
	}
}

func TestRetainedEvaluationsRemainReadableOnTrustedScreens(t *testing.T) {
	h := freshnessHarness(t)
	h.person("sam")
	task := failedTurnTask(t, h)
	if _, err := h.s.DB.SaveButlerEvaluation(runtimedb.ButlerEvaluation{Author: "mika", Name: "retained-fixture", SourceTaskID: task, Summary: "retained-private-history", Expected: "retain the lesson"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.ExecForTest("DELETE FROM butler_task_transitions WHERE task=CAST(substr(?,4) AS INTEGER)", task); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.ExecForTest("DELETE FROM butler_tasks WHERE seq=CAST(substr(?,4) AS INTEGER)", task); err != nil {
		t.Fatal(err)
	}
	var body json.RawMessage
	res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butler/evaluations", nil, &body)
	if res.StatusCode != http.StatusOK || strings.Contains(string(body), "retained-private-history") {
		t.Fatalf("unsafe or broken bearer collection: %d %s", res.StatusCode, body)
	}
	cookie := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	code, out := h.sessionRequest(http.MethodGet, "/api/v1/butler/evaluations", "", "mika", cookie)
	raw, _ := json.Marshal(out)
	if code != http.StatusOK || !strings.Contains(string(raw), "retained-private-history") {
		t.Fatalf("retained fixture inaccessible in Console: %d %s", code, raw)
	}
}

func TestGeneratedPlansFollowSourcePrivacyAcrossAPIAndChat(t *testing.T) {
	h := freshnessHarness(t)
	h.person("sam")
	if _, err := h.s.DB.AcquireCoordinator("plan-privacy"); err != nil {
		t.Fatal(err)
	}
	f := wireFailingButler(t, h)
	memory, err := h.s.DB.RecordMemory(runtimedb.MemoryRecord{Scope: runtimedb.MemoryScopePersonal, Kind: "fact", Content: "jasmine", Owner: "sam", Author: "sam", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "sandy", SourceKind: "conversation", SourceID: f.answer.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	response, _ := h.send(runtimedb.RoleConcierge, map[string]any{"transport": "telegram", "account": "12345", "text": "propose grafana", "messageId": "private-plan"})
	if response.StatusCode != http.StatusBadGateway {
		t.Fatal(response.Status)
	}
	plans, err := h.s.DB.ListPlans(10)
	if err != nil || len(plans) != 1 {
		t.Fatalf("proposal missing: %v %v", plans, err)
	}
	planID := plans[0].ID
	if _, err = h.s.DB.InsertPlan("mika", "mika", "ordinary plan", "low", "{}"); err != nil {
		t.Fatal(err)
	}
	at := h.agentAt(t, "admin", "telegram")
	var sources []string
	command := chatCommandCtx{At: at, Arg: planID, ReplySessions: &sources}
	body, err := h.s.chatPlan(command)
	if err != nil || !strings.Contains(body, planID) {
		t.Fatalf("ordinary chat inspection: %s %v", body, err)
	}
	queued := h.s.queueChatReply(at, "room", body, 0, "", "plan-privacy", sources...)
	if queued == 0 {
		t.Fatal("reply missing")
	}
	cookie := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	for _, protected := range []bool{false, true} {
		if err = h.s.DB.SetMemoryPrivacy("sam", memory, runtimedb.PrivacyPolicy{TrustedOnly: protected}); err != nil {
			t.Fatal(err)
		}
		var result json.RawMessage
		response = h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/plans/"+planID, nil, &result)
		want := http.StatusOK
		if protected {
			want = http.StatusForbidden
		}
		if response.StatusCode != want {
			t.Errorf("protected=%v direct Plan: %d %s", protected, response.StatusCode, result)
		}
		response = h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/plans", nil, &result)
		if response.StatusCode != http.StatusOK || !strings.Contains(string(result), "ordinary plan") || (protected && strings.Contains(string(result), `"`+planID+`"`)) {
			t.Errorf("incorrect Plan list: %d %s", response.StatusCode, result)
		}
		for _, read := range []func(chatCommandCtx) (string, error){h.s.chatPlans, h.s.chatPlan} {
			text, _ := read(command)
			if protected && strings.Contains(text, planID) {
				t.Errorf("protected Plan leaked in chat: %s", text)
			}
		}
		if code, _ := h.sessionRequest(http.MethodGet, "/api/v1/plans/"+planID, "", "mika", cookie); code != http.StatusOK {
			t.Errorf("Console lost Plan: %d", code)
		}
	}
	messages, err := h.s.DB.ChatOutboundAfter(at, 0, 10)
	if err != nil || len(messages) != 1 || strings.Contains(messages[0].Body, planID) {
		t.Fatalf("queued Plan escaped updated privacy: %v %v", messages, err)
	}
}

func TestGeneratedPlanIsPrivateUntilSourceSessionIsDurable(t *testing.T) {
	h := freshnessHarness(t)
	h.person("sam")
	f := wireFailingButler(t, h)
	ids, errs := h.s.submitSessionProposals(principal.Identity{Name: "butler", Role: runtimedb.RoleButler, ActingFor: "sam"}, f.answer.Calls)
	if len(errs) != 0 || len(ids) != 1 {
		t.Fatal(ids, errs)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/plans/"+ids[0], nil, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("in-flight Plan exposed without provenance: %d", res.StatusCode)
	}
	cookie := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	if code, _ := h.sessionRequest(http.MethodGet, "/api/v1/plans/"+ids[0], "", "mika", cookie); code != http.StatusOK {
		t.Fatalf("Console cannot inspect retained Plan: %d", code)
	}
}
