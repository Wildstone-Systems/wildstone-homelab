package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestDemotionRequiresAFreshOwnerSession(t *testing.T) {
	h := freshnessHarness(t)
	if err := h.s.DB.SetButlerMemberRole("sandy", runtimedb.RoleOwner, "mika"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	h.s.Now = func() time.Time { return now }
	stale := h.s.StartConsoleSession("mika", now.Add(-30*time.Minute))
	res := h.consoleRequest(http.MethodPost, "/api/v1/butler/members/sandy/demote", "mika", sessionCookie(stale))
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("stale Owner session demoted someone: %d", res.StatusCode)
	}
	if member, _ := h.s.DB.GetButlerMember("sandy"); member.Role != runtimedb.RoleOwner {
		t.Fatal("stale request changed the role")
	}
	fresh := h.s.StartConsoleSession("mika", now)
	res = h.consoleRequest(http.MethodPost, "/api/v1/butler/members/sandy/demote", "mika", sessionCookie(fresh))
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("fresh Owner could not demote: %d", res.StatusCode)
	}
	if member, _ := h.s.DB.GetButlerMember("sandy"); member.Role != runtimedb.RoleMember {
		t.Fatal("fresh request did not demote")
	}
}

func TestDemotionAcceptsForwardedOwnerCasing(t *testing.T) {
	h := freshnessHarness(t)
	if err := h.s.DB.SetButlerMemberRole("sandy", runtimedb.RoleOwner, "mika"); err != nil {
		t.Fatal(err)
	}
	session := h.s.StartConsoleSession("Mika", time.Now())
	res := h.consoleRequest(http.MethodPost, "/api/v1/butler/members/sandy/demote", "Mika", sessionCookie(session))
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("authenticated Owner with forwarded casing could not demote: %d", res.StatusCode)
	}
	if member, _ := h.s.DB.GetButlerMember("sandy"); member.Role != runtimedb.RoleMember {
		t.Fatal("demotion did not change the target role")
	}
}

func TestDemotionRevokesConsoleAndElevatedSessions(t *testing.T) {
	h := homeHarness(t)
	for member, role := range map[string]string{"mika": runtimedb.RoleOwner, "sandy": runtimedb.RoleMember} {
		if err := h.s.DB.RecordButlerMember(member, role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.AdmitButlerMember(member, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.s.DB.SetButlerMemberRole("sandy", runtimedb.RoleOwner, "mika"); err != nil {
		t.Fatal(err)
	}
	target := h.s.StartConsoleSession("sandy", time.Now())
	actor := h.s.StartConsoleSession("mika", time.Now())
	if res := h.consoleRequest(http.MethodGet, "/api/v1/whoami", "sandy", sessionCookie(target)); res.StatusCode != http.StatusOK {
		t.Fatalf("precondition: target Console access = %d", res.StatusCode)
	}
	res := h.consoleRequest(http.MethodPost, "/api/v1/butler/members/sandy/demote", "mika", sessionCookie(actor))
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("demotion = %d", res.StatusCode)
	}
	if _, ok := h.s.console.sessions.Lookup(target); ok {
		t.Fatal("demoted Owner's elevated session remained stored")
	}
	res = h.consoleRequest(http.MethodGet, "/api/v1/whoami", "sandy", sessionCookie(target))
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("demoted Owner still entered Console: %d", res.StatusCode)
	}
	home := h.member(http.MethodGet, "/api/v1/home/summary", "sandy", "homepage_users", nil, "")
	home.Body.Close()
	if home.StatusCode != http.StatusOK {
		t.Fatalf("demoted Member lost Homepage: %d", home.StatusCode)
	}
}

func TestDemotingTheLastOwnerIsRefusedAtTheConsoleAction(t *testing.T) {
	h := freshnessHarness(t)
	actor := h.s.StartConsoleSession("mika", time.Now())
	res := h.consoleRequest(http.MethodPost, "/api/v1/butler/members/mika/demote", "mika", sessionCookie(actor))
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("last Owner demotion = %d, want 400", res.StatusCode)
	}
	if member, _ := h.s.DB.GetButlerMember("mika"); member.Role != runtimedb.RoleOwner {
		t.Fatal("last Owner changed role")
	}
	if _, ok := h.s.console.sessions.Lookup(actor); !ok {
		t.Fatal("refused demotion ended the Owner session")
	}
}

func TestDemotionCannotBeRequestedByBearerOrMember(t *testing.T) {
	h := freshnessHarness(t)
	if err := h.s.DB.SetButlerMemberRole("sandy", runtimedb.RoleOwner, "mika"); err != nil {
		t.Fatal(err)
	}
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/members/sandy/demote", nil, nil)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("bearer admin demoted a Person: %d", res.StatusCode)
	}
	res = h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/butler/members/sandy/demote", nil, nil)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("non-Owner demoted a Person: %d", res.StatusCode)
	}
	if member, _ := h.s.DB.GetButlerMember("sandy"); member.Role != runtimedb.RoleOwner {
		t.Fatal("unauthorized call changed the role")
	}
}

func TestOwnerMayDemoteSelfOnlyWhileAnotherOwnerRemains(t *testing.T) {
	h := freshnessHarness(t)
	if err := h.s.DB.SetButlerMemberRole("sandy", runtimedb.RoleOwner, "mika"); err != nil {
		t.Fatal(err)
	}
	self := h.s.StartConsoleSession("sandy", time.Now())
	res := h.consoleRequest(http.MethodPost, "/api/v1/butler/members/sandy/demote", "sandy", sessionCookie(self))
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("self-demotion with another Owner = %d", res.StatusCode)
	}
	if _, ok := h.s.console.sessions.Lookup(self); ok {
		t.Fatal("self-demotion kept the Owner session")
	}
}
