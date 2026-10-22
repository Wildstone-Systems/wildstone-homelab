package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// seedDirectory declares one group and one person, the shape a
// deployment has once an app package is installed and somebody has been
// added.
func seedDirectory(t *testing.T, h *harness) {
	t.Helper()
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindLdapGroup,
		Metadata:  resource.Metadata{Name: "photos-users", Owner: "capability/photos", Node: "n1"},
		LdapGroup: &resource.LdapGroupSpec{Name: "photos_users"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindLdapUser,
		Metadata: resource.Metadata{Name: "sam", Owner: memberOwner, Node: "n1"},
		LdapUser: &resource.LdapUserSpec{
			Username: "sam", DisplayName: "Sam", Email: "sam@example.com",
			Groups: []string{"photos_users"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.State.Commit("test", "seed", "seed the directory"); err != nil {
		t.Fatal(err)
	}
}

// The roster is the Statefile's LdapUsers, which is what makes the chat
// invite screen able to offer real people instead of a text box.
func TestHouseholdMembersAreTheDeclaredUsers(t *testing.T) {
	h := newHarness(t)

	var empty []householdMember
	if resp := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/household/members", nil, &empty); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(empty) != 0 {
		t.Fatalf("members on a fresh deployment = %+v, want none", empty)
	}

	seedDirectory(t, h)
	var members []householdMember
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/household/members", nil, &members)
	if len(members) != 1 || members[0].Username != "sam" || members[0].DisplayName != "Sam" {
		t.Fatalf("members = %+v, want sam", members)
	}
	if len(members[0].Groups) != 1 || members[0].Groups[0] != "photos_users" {
		t.Fatalf("groups = %v, want photos_users", members[0].Groups)
	}

	var groups []householdGroup
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/household/groups", nil, &groups)
	if len(groups) != 1 || groups[0].Name != "photos_users" {
		t.Fatalf("groups = %+v, want photos_users", groups)
	}
}

// Adding a member proposes a plan and changes nothing until a human
// approves it (ADR-W020 A3): the account is declared state, not a live
// write to the directory.
func TestAddHouseholdMemberProposesAPlan(t *testing.T) {
	h := newHarness(t)
	seedDirectory(t, h)

	var p planView
	resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/members", map[string]any{
		"username": "kim", "displayName": "Kim", "email": "kim@example.com",
		"groups": []string{"photos_users"},
	}, &p)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if p.State != runtimedb.PlanProposed {
		t.Fatalf("plan state = %q, want proposed", p.State)
	}

	// Still not a member: the plan is waiting for approval.
	var members []householdMember
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/household/members", nil, &members)
	for _, m := range members {
		if m.Username == "kim" {
			t.Fatal("kim is a member before the plan was approved")
		}
	}

	// Approving commits the declaration, and only then is she one.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+p.ID+"/approve", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("approve: status = %d, want 200", resp.StatusCode)
	}
	members = nil
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/household/members", nil, &members)
	found := false
	for _, m := range members {
		if m.Username == "kim" {
			found = true
			if m.Owner != memberOwner {
				t.Fatalf("kim owned by %q, want %q", m.Owner, memberOwner)
			}
			// Declared against the node the directory runs on, which
			// the operator never had to know.
			if m.Node != "n1" {
				t.Fatalf("kim declared on node %q, want n1", m.Node)
			}
		}
	}
	if !found {
		t.Fatalf("members after approval = %+v, want kim", members)
	}

	// Adding her twice is a conflict, not a rename.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/members", map[string]any{
		"username": "kim", "displayName": "Kim", "email": "kim@example.com",
	}, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate: status = %d, want 409", resp.StatusCode)
	}
}

// Everything the request can get wrong is refused here, where the
// operator is standing, rather than in a provisioning job's container
// log hours later.
func TestAddHouseholdMemberRefusals(t *testing.T) {
	h := newHarness(t)
	seedDirectory(t, h)

	for name, body := range map[string]map[string]any{
		"username is not an LLDAP name":    {"username": "Kim Smith", "email": "kim@example.com"},
		"no email at all":                  {"username": "kim"},
		"email a shell could break out of": {"username": "kim", "email": "kim@exa`mple.com"},
		"a group nothing declares":         {"username": "kim", "email": "kim@example.com", "groups": []string{"media_users"}},
	} {
		if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/members", body, nil); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, resp.StatusCode)
		}
	}

	// With no directory at all there is nowhere to add anybody, and the
	// refusal says which capability is missing.
	bare := newHarness(t)
	if resp := bare.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/members", map[string]any{
		"username": "kim", "email": "kim@example.com",
	}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no directory: status = %d, want 400", resp.StatusCode)
	}

	// A node that runs no directory is refused rather than accepted and
	// never provisioned.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/members", map[string]any{
		"username": "kim", "email": "kim@example.com", "node": "n2",
	}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong node: status = %d, want 400", resp.StatusCode)
	}

	// A group declared on ANOTHER directory is refused. The reconciler
	// renders the user list per node, so B's provisioning job cannot
	// create a membership in a group only A declares — the plan would
	// apply, the account would appear, and the person would silently
	// lack the app they were added for.
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindLdapGroup,
		Metadata:  resource.Metadata{Name: "media-users", Owner: "capability/media", Node: "n2"},
		LdapGroup: &resource.LdapGroupSpec{Name: "media_users"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.State.Commit("test", "seed", "a second directory"); err != nil {
		t.Fatal(err)
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/members", map[string]any{
		"username": "kim", "email": "kim@example.com", "node": "n1", "groups": []string{"media_users"},
	}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("group on another directory: status = %d, want 400", resp.StatusCode)
	}
	// The same group on its own directory is fine.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/members", map[string]any{
		"username": "kim", "email": "kim@example.com", "node": "n2", "groups": []string{"media_users"},
	}, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("group on its own directory: status = %d, want 201", resp.StatusCode)
	}

	// Adding a person is a propose act: a concierge relaying chat cannot
	// invent one.
	if resp := h.do(runtimedb.RoleConcierge, http.MethodPost, "/api/v1/household/members", map[string]any{
		"username": "kim", "email": "kim@example.com",
	}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("concierge add: status = %d, want 403", resp.StatusCode)
	}
}

// Two members cannot share an email, whether added or edited into it.
// Accepted, the plan would apply and the directory would refuse the
// account, failing the provisioning job for every plan after it.
func TestHouseholdMemberEmailMustBeUnique(t *testing.T) {
	h := newHarness(t)
	seedDirectory(t, h)

	var body map[string]any
	resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/members", map[string]any{
		"username": "kim", "email": "SAM@example.com", "groups": []string{"photos_users"},
	}, &body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("adding with sam's email = %d, want 422", resp.StatusCode)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "sam") {
		t.Fatalf("the refusal did not name the member who has the address: %v", body)
	}

	var p planView
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/members", map[string]any{
		"username": "kim", "email": "kim@example.com", "groups": []string{"photos_users"},
	}, &p); resp.StatusCode != http.StatusCreated {
		t.Fatalf("adding with her own email = %d, want 201", resp.StatusCode)
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+p.ID+"/approve", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d", resp.StatusCode)
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/household/members/kim", map[string]any{
		"email": "sam@example.com", "groups": []string{"photos_users"},
	}, nil); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("editing onto sam's email = %d, want 422", resp.StatusCode)
	}
}

// Editing a member cannot take a group away.
//
// The provisioning job is additive in both directions: it adds a
// membership it is missing and never removes one. So a plan that
// dropped a group would apply cleanly and leave the person in the
// group — the screen saying "removed" while the app still opens for
// them. Refusing names the group and says where it can be done.
func TestEditingAMemberCannotRemoveAGroup(t *testing.T) {
	h := newHarness(t)
	seedDirectory(t, h)

	var p planView
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/members", map[string]any{
		"username": "kim", "displayName": "Kim", "email": "kim@example.com", "groups": []string{"photos_users"},
	}, &p); resp.StatusCode != http.StatusCreated {
		t.Fatalf("adding = %d", resp.StatusCode)
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+p.ID+"/approve", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d", resp.StatusCode)
	}

	// A name change is fine, and is a proposal like everything else.
	var edit planView
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/household/members/kim", map[string]any{
		"displayName": "Kim H", "groups": []string{"photos_users"},
	}, &edit); resp.StatusCode != http.StatusCreated {
		t.Fatalf("editing = %d", resp.StatusCode)
	}
	if edit.State != runtimedb.PlanProposed {
		t.Fatalf("an edit applied without approval: %q", edit.State)
	}

	// Dropping a group is refused, by name.
	var body map[string]any
	resp := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/household/members/kim", map[string]any{
		"groups": []string{},
	}, &body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("dropping a group = %d, want 400", resp.StatusCode)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "photos_users") {
		t.Fatalf("the refusal did not name the group: %v", body)
	}
}
