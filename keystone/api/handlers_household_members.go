package api

// Household members (ADR-W020 A3, ADR-W023 A7).
//
// A household member is an `LdapUser` in the Statefile, and nothing
// else. That is the whole point of A3: the account list lives in state,
// in plans, in approval and in audit, rather than in LLDAP's own UI
// where a Statefile restore would bring back every group with nobody in
// them and read as a working system.
//
// These endpoints are what "the admin adds people from the Console"
// (ADR-W023 A7) actually means. Reading is a `read` act — the roster is
// no more sensitive than the resource list it is drawn from. Adding is a
// `propose` act, because it is one: this handler builds the LdapUser
// document and hands it to the same proposal pipeline a human's YAML
// goes through, so a person is added by an approved plan or not at all.
//
// Nothing here deletes. Removing an LdapUser is declaration-only
// everywhere else in the system (ADR-W020 A3), and a screen with a
// "remove" button next to a name would promise something the control
// plane deliberately does not do.

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// memberOwner owns an LdapUser the operator added by hand. It is not a
// capability: a person is not installed, and no capability's removal
// should read as a reason to stop asserting them.
const memberOwner = "system"

// identityCapability is the capability that runs the directory. Naming
// it here keeps the "which node" derivation honest when a deployment has
// the provider installed but no app groups yet.
const identityCapability = "identity"

// maxMemberGroups bounds one add request. A household member is in a
// handful of app groups; a list longer than this is a mistake or an
// attempt to make the rendered user list unbounded.
const maxMemberGroups = 32

// householdMember is one declared person, flattened for the Console.
type householdMember struct {
	Username    string   `json:"username"`
	DisplayName string   `json:"displayName,omitempty"`
	Email       string   `json:"email,omitempty"`
	Groups      []string `json:"groups"`
	// Owner says who declared this person: "system" for someone an
	// operator added, "capability/<name>" for an account a package
	// asserts. The Console shows it because the two are not equally
	// safe to reason about.
	Owner string `json:"owner"`
	// Node is the node running the directory this person is declared
	// against. LdapUser is node-scoped for the same reason LdapGroup
	// is: it is provisioned against one LLDAP.
	Node string `json:"node"`
}

// listHouseholdMembers answers "who is in this household" from the
// Statefile. It is the list the chat invite screen picks from, so that
// binding a chat account to a person cannot name somebody who does not
// exist.
func (s *Server) listHouseholdMembers(_ *http.Request, _ principal.Identity) (any, int, error) {
	current, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("loading statefile: %w", err)
	}
	out := []householdMember{}
	seen := map[string]bool{}
	for _, r := range current {
		if r.Kind != resource.KindLdapUser || r.LdapUser == nil || r.LdapUser.Username == "" {
			continue
		}
		// Two packages may name the same person and the directory has
		// one account either way — the same rule the rendered user list
		// applies (keystone/reconcile/lldapusers.go), so the roster and
		// the file the provisioning job reads agree on who exists.
		if seen[r.LdapUser.Username] {
			continue
		}
		seen[r.LdapUser.Username] = true
		groups := append([]string(nil), r.LdapUser.Groups...)
		sort.Strings(groups)
		if groups == nil {
			groups = []string{}
		}
		out = append(out, householdMember{
			Username:    r.LdapUser.Username,
			DisplayName: r.LdapUser.DisplayName,
			Email:       r.LdapUser.Email,
			Groups:      groups,
			Owner:       r.Metadata.Owner,
			Node:        r.Metadata.Node,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, http.StatusOK, nil
}

// householdGroup is one declared LdapGroup, which is the closed set of
// groups a new member may be put in.
type householdGroup struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Node  string `json:"node"`
}

// listHouseholdGroups answers what groups exist to join. The add-member
// form offers these and nothing else: a membership in a group no
// package declares is one the provisioning job cannot create, so it
// would fail silently on the node hours later.
func (s *Server) listHouseholdGroups(_ *http.Request, _ principal.Identity) (any, int, error) {
	groups, err := s.declaredGroups()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := make([]householdGroup, 0, len(groups))
	for name, g := range groups {
		out = append(out, householdGroup{Name: name, Owner: g.owner, Node: g.node})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, http.StatusOK, nil
}

// declaredGroup is where one group came from.
type declaredGroup struct {
	owner string
	node  string
}

// declaredGroups maps every declared LdapGroup name to where it came
// from.
func (s *Server) declaredGroups() (map[string]declaredGroup, error) {
	current, err := s.State.Load()
	if err != nil {
		return nil, fmt.Errorf("loading statefile: %w", err)
	}
	groups := map[string]declaredGroup{}
	for _, r := range current {
		if r.Kind == resource.KindLdapGroup && r.LdapGroup != nil && r.LdapGroup.Name != "" {
			if _, ok := groups[r.LdapGroup.Name]; !ok {
				groups[r.LdapGroup.Name] = declaredGroup{owner: r.Metadata.Owner, node: r.Metadata.Node}
			}
		}
	}
	return groups, nil
}

// directoryNode is the node this deployment'"'"'s directory runs on, which is
// the node an LdapUser must be declared against (the kind is node-scoped
// for the same reason LdapGroup is).
//
// It is derived rather than asked for: the operator adding their sister
// to the photo library does not know which machine LLDAP is on, and a
// wrong answer would put a person on a node with no directory, where
// nothing would ever provision them and nothing would ever say so. When
// the deployment genuinely has more than one candidate, the request must
// name one — guessing is what this function exists to avoid.
func directoryNode(current []resource.Resource, requested string) (string, error) {
	nodes := map[string]bool{}
	for _, r := range current {
		switch {
		case r.Kind == resource.KindLdapGroup || r.Kind == resource.KindLdapUser:
			// The directory kinds themselves: the strongest signal.
		case r.Metadata.Owner == "capability/"+identityCapability:
			// A deployment with identity installed and no groups yet.
		default:
			continue
		}
		if r.Metadata.Node != "" {
			nodes[r.Metadata.Node] = true
		}
	}
	if requested != "" {
		if !nodes[requested] {
			return "", fmt.Errorf("node %q is not running the directory", requested)
		}
		return requested, nil
	}
	switch len(nodes) {
	case 0:
		return "", fmt.Errorf(
			"no directory to add a person to: install the %s capability first", identityCapability)
	case 1:
		for n := range nodes {
			return n, nil
		}
	}
	names := make([]string, 0, len(nodes))
	for n := range nodes {
		names = append(names, n)
	}
	sort.Strings(names)
	return "", fmt.Errorf("more than one node runs a directory (%s): name the one to add this person to",
		strings.Join(names, ", "))
}

type addMemberRequest struct {
	Username    string   `json:"username"`
	DisplayName string   `json:"displayName"`
	Email       string   `json:"email"`
	Groups      []string `json:"groups,omitempty"`
	// Node names the directory node. Optional: it is derived, and only
	// needed when a deployment runs more than one directory.
	Node string `json:"node,omitempty"`
}

// addHouseholdMember proposes the plan that declares a person.
//
// It does NOT create an account: it produces a `proposed` plan a human
// approves on the Plans screen, after which the reconciler renders the
// user list and the identity capability's provisioning job creates the
// account in LLDAP — with no password, because a credential never lives
// in a spec (ADR-W020 A4). The person gets their first password through
// Authelia's own reset flow (ADR-W023 A7).
// proposeMemberResource renders one LdapUser and proposes it, shared by
// adding and updating so both take the same path to Plans.
func (s *Server) proposeMemberResource(res resource.Resource, id principal.Identity, summary string) (any, int, error) {
	doc, err := resource.Marshal(res)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("rendering the member: %w", err)
	}
	out, status, err := s.submitProposal(id, proposalRequest{Summary: summary, Put: []string{string(doc)}})
	if err != nil {
		return nil, status, err
	}
	s.event("household.member_proposed", res.LdapUser.Username, id,
		fmt.Sprintf("%s; it takes effect when the plan is approved", summary))
	return out, status, nil
}

// updateHouseholdMember proposes a change to somebody already declared.
//
// It is a proposal like adding is, for the same reason: the account
// list belongs in state, in plans, in approval and in audit. Nothing
// here writes to the directory.
//
// Groups may only be ADDED. The provisioning job is additive in both
// directions — it creates a user the directory lacks and adds a
// membership it is missing, and never removes either — so a request
// that dropped a group would change the Statefile, apply cleanly, and
// leave the person in the group. The screen would say removed and
// nothing would be. Refusing is the honest answer, and it names the
// group so the operator knows what the control plane cannot do rather
// than discovering it from an app that still opens.
func (s *Server) updateHouseholdMember(r *http.Request, id principal.Identity) (any, int, error) {
	username := strings.TrimSpace(r.PathValue("username"))
	var req addMemberRequest
	if st, err := decodeChatBody(r, &req); err != nil {
		return nil, st, err
	}
	if len(req.Groups) > maxMemberGroups {
		return nil, http.StatusBadRequest, fmt.Errorf("at most %d groups", maxMemberGroups)
	}
	current, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("loading statefile: %w", err)
	}
	var existing *resource.Resource
	for i, res := range current {
		if res.Kind == resource.KindLdapUser && res.LdapUser != nil && res.LdapUser.Username == username {
			existing = &current[i]
			break
		}
	}
	if existing == nil {
		return nil, http.StatusNotFound, fmt.Errorf("%q is not a household member", username)
	}

	had := map[string]bool{}
	for _, g := range existing.LdapUser.Groups {
		had[g] = true
	}
	want := map[string]bool{}
	for _, g := range req.Groups {
		if g = strings.TrimSpace(g); g != "" {
			want[g] = true
		}
	}
	for g := range had {
		if !want[g] {
			return nil, http.StatusBadRequest, fmt.Errorf(
				"this cannot take %q away from %s: the provisioning job never removes a membership, "+
					"so the plan would apply and they would still be in the group. Remove it in the "+
					"directory itself", g, username)
		}
	}

	declared, err := s.declaredGroups()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	node := existing.Metadata.Node
	groups := make([]string, 0, len(want))
	for g := range want {
		d, ok := declared[g]
		if !ok {
			return nil, http.StatusBadRequest, fmt.Errorf(
				"no capability declares the group %q: install the app that owns it first, or leave it out", g)
		}
		if d.node != node {
			return nil, http.StatusBadRequest, fmt.Errorf(
				"group %q is declared on node %s, not on the directory %s is on (%s)", g, d.node, username, node)
		}
		groups = append(groups, g)
	}
	sort.Strings(groups)

	updated := *existing
	spec := *existing.LdapUser
	spec.Groups = groups
	if n := strings.TrimSpace(req.DisplayName); n != "" {
		spec.DisplayName = n
	}
	if e := strings.TrimSpace(req.Email); e != "" {
		spec.Email = e
	}
	updated.LdapUser = &spec
	if err := updated.Validate(); err != nil {
		return nil, http.StatusBadRequest, err
	}
	return s.proposeMemberResource(updated, id,
		fmt.Sprintf("update household member %s", username))
}

func (s *Server) addHouseholdMember(r *http.Request, id principal.Identity) (any, int, error) {
	var req addMemberRequest
	if st, err := decodeChatBody(r, &req); err != nil {
		return nil, st, err
	}
	req.Username = strings.TrimSpace(req.Username)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.Email = strings.TrimSpace(req.Email)
	if len(req.Groups) > maxMemberGroups {
		return nil, http.StatusBadRequest, fmt.Errorf("at most %d groups", maxMemberGroups)
	}

	// Refuse a group nothing declares, here rather than on the node. The
	// provisioning job is additive and best-effort: an undeclared group
	// does not fail the plan, it fails one membership in a container log
	// nobody reads, and the person quietly cannot open the app they were
	// added for.
	declared, err := s.declaredGroups()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	// The person cannot already be declared. Re-declaring would compute
	// an update plan whose diff looks like a rename, and the operator
	// asked to ADD somebody.
	current, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("loading statefile: %w", err)
	}
	for _, res := range current {
		if res.Kind == resource.KindLdapUser && res.LdapUser != nil && res.LdapUser.Username == req.Username {
			return nil, http.StatusConflict, fmt.Errorf("%q is already a household member", req.Username)
		}
	}

	node, err := directoryNode(current, strings.TrimSpace(req.Node))
	if err != nil {
		return nil, http.StatusBadRequest, err
	}

	// A membership is only real on the directory the group is declared
	// against: the reconciler renders the user list per node, so a group
	// from node A named on a person declared on node B cannot be created
	// by B's provisioning job. Nothing fails — the plan applies, the
	// account appears, and the person silently lacks the app they were
	// added for.
	groups := make([]string, 0, len(req.Groups))
	for _, g := range req.Groups {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		d, ok := declared[g]
		if !ok {
			return nil, http.StatusBadRequest, fmt.Errorf(
				"no capability declares the group %q: install the app that owns it first, or leave it out", g)
		}
		if d.node != node {
			return nil, http.StatusBadRequest, fmt.Errorf(
				"group %q is declared on node %s, not on the directory this person goes on (%s)", g, d.node, node)
		}
		groups = append(groups, g)
	}
	sort.Strings(groups)

	res := resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindLdapUser,
		Metadata:   resource.Metadata{Name: req.Username, Owner: memberOwner, Node: node},
		LdapUser: &resource.LdapUserSpec{
			Username:    req.Username,
			DisplayName: req.DisplayName,
			Email:       req.Email,
			Groups:      groups,
		},
	}
	// Validate before proposing so the operator gets the field-level
	// message ("that is not an LLDAP username") instead of a plan that
	// is refused as a whole.
	if err := res.Validate(); err != nil {
		return nil, http.StatusBadRequest, err
	}
	doc, err := resource.Marshal(res)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("rendering the member: %w", err)
	}

	out, status, err := s.submitProposal(id, proposalRequest{
		Summary: fmt.Sprintf("add household member %s", req.Username),
		Put:     []string{string(doc)},
	})
	if err != nil {
		return nil, status, err
	}
	s.event("household.member_proposed", req.Username, id,
		fmt.Sprintf("household member %q proposed on node %s with groups [%s]; the account is created when the plan is approved",
			req.Username, node, strings.Join(groups, " ")))
	return out, status, nil
}
