package api

import (
	"context"
	"errors"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/domestic"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLegacyHouseholdEffectsPassTheStoreFence(t *testing.T) {
	for _, mode := range []string{"coordinator", "non-coordinator", "stopped", "superseded"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			if mode == "non-coordinator" {
				if _, err := h.s.DB.ExecForTest("INSERT INTO coordinator_lease(id,holder,epoch,acquired_at,heartbeat_at) VALUES(1,'other',1,?,?)", time.Now().Format(time.RFC3339), time.Now().Format(time.RFC3339)); err != nil {
					t.Fatal(err)
				}
				if _, err := h.s.DB.AcquireCoordinator("this-process"); !errors.Is(err, runtimedb.ErrCoordinatorHeld) {
					t.Fatal(err)
				}
			} else {
				if _, err := h.s.DB.AcquireCoordinator("this-process"); err != nil {
					t.Fatal(err)
				}
				if mode == "stopped" {
					if err := h.s.DB.EnterRestoreQuarantine("household stop", time.Now()); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "superseded" {
					if _, err := h.s.DB.PromoteCoordinator("replacement"); err != nil {
						t.Fatal(err)
					}
				}
			}
			var notifications, chat atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { notifications.Add(1); w.WriteHeader(http.StatusOK) }))
			defer upstream.Close()
			gateway := &domestic.Gateway{Authority: h.s.DB}
			gateway.Register(&domestic.Notifier{Authority: h.s.DB, BaseURL: upstream.URL})
			gateway.Register(&domestic.ChatSender{Authority: h.s.DB, Capability: "chat", Transport: "telegram", Lookup: func(context.Context, string) (domestic.ChatRecipient, error) {
				return domestic.ChatRecipient{Target: "bound-account"}, nil
			}, Ask: func(context.Context, domestic.ChatRecipient, string, string, string, string) (int64, error) {
				chat.Add(1)
				return 1, nil
			}})
			h.wireHouseholdButler([]*provider.ChatResponse{
				{ToolCalls: []provider.ToolCall{
					{ID: "notice", Name: "do_household_action", Input: map[string]any{"tool": "notifications.notify", "params": map[string]any{"topic": "household", "message": "test notice"}}},
					{ID: "chat", Name: "do_household_action", Input: map[string]any{"tool": "chat.send", "params": map[string]any{"message": "test message"}}},
				}}, {Text: "Finished checking the requests."},
			}, runtimedb.ButlerModeFull)
			h.s.ButlerHost.Household.Vocabulary = func() (butler.HouseholdVocabulary, error) {
				return butler.HouseholdVocabulary{Actions: []butler.HouseholdAction{{Tool: "notifications.notify", Tier: "domestic"}, {Tool: "chat.send", Tier: "domestic"}}}, nil
			}
			var reasons []string
			h.s.ButlerHost.Household.Actions = func(ctx context.Context, c butler.HouseholdActionCall) (string, error) {
				result, err := gateway.Dispatch(ctx, domestic.Call{Tool: c.Tool, Tier: c.Tier, Params: c.Params, Subject: c.Member, Attended: true})
				if err != nil {
					reasons = append(reasons, err.Error())
				}
				return result.Summary, err
			}
			if h.s.Butlers != nil {
				t.Fatal("test must use the legacy path")
			}
			response := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/household/butler/ask", map[string]string{"message": "send both notices"}, nil)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("legacy request: %d", response.StatusCode)
			}
			if mode == "coordinator" {
				if notifications.Load() != 1 || chat.Load() != 1 {
					t.Fatalf("positive control failed: %d %d %v", notifications.Load(), chat.Load(), reasons)
				}
			} else {
				if notifications.Load() != 0 || chat.Load() != 0 {
					t.Fatalf("unfenced effects: %d %d", notifications.Load(), chat.Load())
				}
				if len(reasons) == 0 {
					t.Fatal("test never reached the execution boundary")
				}
				reason := "coordinator"
				if mode == "stopped" {
					reason = "quarantine"
				}
				if !strings.Contains(strings.Join(reasons, " "), reason) {
					t.Fatalf("unexplained fence: %v", reasons)
				}
			}
		})
	}
}

// Existing effectful integration fixtures represent a coordinating Keystone.
// Do not acquire a lease already held elsewhere: negative tests keep that state.
func ownExecutionFixture(t *testing.T, h *harness) {
	t.Helper()
	_, err := h.s.DB.GetCoordinator()
	if errors.Is(err, runtimedb.ErrNotFound) {
		_, err = h.s.DB.AcquireCoordinator("api-effect-fixture")
	}
	if err != nil {
		t.Fatal(err)
	}
}
