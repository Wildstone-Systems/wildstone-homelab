package api

// The Console's forward-auth path (ADR-W032).
//
// This is the one authentication path that constructs an administrator
// out of two request headers, so the tests here are mostly about what it
// REFUSES. Each of the four conditions is exercised alone, because a
// path that is safe only when all four happen to hold is a path one
// refactor away from not being safe at all.

import (
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// forwarded issues a request the way the identity proxy would: no bearer
// token, a Remote-User, and whatever group list the case is testing.
func (h *harness) forwarded(method, path, user, groups string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, nil)
	if err != nil {
		h.t.Fatalf("NewRequest: %v", err)
	}
	if user != "" {
		req.Header.Set(TrustedIngressHeader, user)
	}
	if groups != "" {
		req.Header.Set(ConsoleGroupsHeader, groups)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	resp.Body.Close()
	return resp
}

// The group is the permission. A directory account in it reaches the
// Console with no token minted for them by hand, which is the whole
// point: membership is managed where every other service's is.
func TestTheConsoleGroupIsWhatGrantsTheConsole(t *testing.T) {
	h := newHarness(t)
	h.s.TrustedIngress = []string{"127.0.0.1", "::1"}

	if res := h.forwarded(http.MethodGet, "/api/v1/butlers", "giulia", ConsoleGroup); res.StatusCode != http.StatusOK {
		t.Fatalf("a person in %s was refused the Console: %d", ConsoleGroup, res.StatusCode)
	}
	// And it is real authority, not a read-only impression of one: the
	// Butlers screen is almost entirely manage-verb endpoints, so a role
	// without manage would render and then refuse every control.
	if res := h.forwarded(http.MethodDelete, "/api/v1/butlers/nobody", "giulia", ConsoleGroup); res.StatusCode == http.StatusForbidden {
		t.Fatal("the forwarded identity holds no manage verb, so the Console would render controls that all 403")
	}
}

// Every refusal, one condition at a time.
func TestTheConsoleForwardAuthRefusesEverythingElse(t *testing.T) {
	h := newHarness(t)
	h.s.TrustedIngress = []string{"127.0.0.1", "::1"}

	for _, c := range []struct {
		what, user, groups string
		trusted            bool
	}{
		{what: "a signed-in person who is not in the group", user: "mika", groups: "media_users", trusted: true},
		{what: "no group header at all", user: "mika", groups: "", trusted: true},
		{what: "no user header", user: "", groups: ConsoleGroup, trusted: true},
		// The peer check is the one that matters most: everything else
		// here is a header the caller controls.
		{what: "the right headers from an untrusted peer", user: "mika", groups: ConsoleGroup, trusted: false},
	} {
		if !c.trusted {
			h.s.TrustedIngress = nil
		} else {
			h.s.TrustedIngress = []string{"127.0.0.1", "::1"}
		}
		if res := h.forwarded(http.MethodGet, "/api/v1/butlers", c.user, c.groups); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", c.what, res.StatusCode)
		}
	}
}

// An unconfigured trust boundary fails closed.
//
// This is the deployment state on a host that has not put the Console
// behind the proxy yet, and it must be "this path does not exist" rather
// than "this path trusts anybody".
func TestTheConsoleForwardAuthIsOffUntilTheProxyIsNamed(t *testing.T) {
	h := newHarness(t)
	if len(h.s.TrustedIngress) != 0 {
		t.Fatal("TrustedIngress is set by default; the header path would be live on a fresh install")
	}
	if res := h.forwarded(http.MethodGet, "/api/v1/butlers", "attacker", ConsoleGroup); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("with no trusted ingress configured the header path answered %d, want 401", res.StatusCode)
	}
}

// A group name is matched exactly.
//
// Group names are the directory's namespace and somebody will eventually
// add a neighbouring one. A prefix or substring match would hand
// "console_users_readonly" everything "console_users" has.
func TestTheConsoleGroupIsMatchedExactly(t *testing.T) {
	h := newHarness(t)
	h.s.TrustedIngress = []string{"127.0.0.1", "::1"}

	for _, groups := range []string{"console_users_readonly", "not_console_users", "console_user", "console_userss"} {
		if res := h.forwarded(http.MethodGet, "/api/v1/butlers", "mika", groups); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("group %q was accepted as %q: %d", groups, ConsoleGroup, res.StatusCode)
		}
	}
	// A list, as Authelia actually sends it.
	if res := h.forwarded(http.MethodGet, "/api/v1/butlers", "mika", "media_users, console_users ,dev"); res.StatusCode != http.StatusOK {
		t.Fatalf("a comma-separated list containing the group was refused: %d", res.StatusCode)
	}
}

// A token always decides when one is offered, even a bad one.
//
// Otherwise a revoked or expired credential quietly becomes whatever the
// headers claim — an escalation arriving through the failure path, which
// is the shape of bug that survives review because it looks like a
// fallback.
func TestABadTokenDoesNotFallThroughToTheHeaders(t *testing.T) {
	h := newHarness(t)
	h.s.TrustedIngress = []string{"127.0.0.1", "::1"}

	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/butlers", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer this-token-was-revoked")
	req.Header.Set(TrustedIngressHeader, "giulia")
	req.Header.Set(ConsoleGroupsHeader, ConsoleGroup)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a rejected token fell through to the forwarded headers: %d", res.StatusCode)
	}
}

// The audit trail names the person, not a shared administrator.
//
// "who could have seen this" is the question the whole Butler design
// exists to answer, and it is worth nothing if every forwarded request
// is filed under one name.
func TestAForwardedRequestIsAuditedUnderTheirOwnName(t *testing.T) {
	h := newHarness(t)
	h.s.TrustedIngress = []string{"127.0.0.1", "::1"}

	if res := h.forwarded(http.MethodGet, "/api/v1/butlers", "giulia", ConsoleGroup); res.StatusCode != http.StatusOK {
		t.Fatalf("setup: %d", res.StatusCode)
	}
	records, err := h.s.DB.ListAudit(20)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		if rec.Path == "/api/v1/butlers" {
			if rec.Principal != "giulia" && rec.ActingFor != "giulia" {
				t.Fatalf("audited as principal=%q actingFor=%q, want giulia", rec.Principal, rec.ActingFor)
			}
			return
		}
	}
	t.Fatal("the forwarded request was not audited at all")
}

// The Console is the Owner surface, and the household's own record of a
// person is what withdraws it (design §3.2, §18.1).
//
// The group cannot do that job on this deployment: group membership is
// only ever added, never removed, so without these checks a person who
// was demoted, disabled or offboarded keeps the whole control plane for
// as long as their directory account exists.
func TestForwardedConsoleAuthRefusesAPersonWhoMayNotAdminister(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, h *harness)
		want  int
	}{
		{
			name:  "an active owner is admitted",
			setup: func(t *testing.T, h *harness) { consolePerson(t, h, runtimedb.RoleOwner, nil) },
			want:  http.StatusOK,
		},
		{
			name:  "a member is refused",
			setup: func(t *testing.T, h *harness) { consolePerson(t, h, runtimedb.RoleMember, nil) },
			want:  http.StatusUnauthorized,
		},
		{
			name: "a demoted owner is refused",
			setup: func(t *testing.T, h *harness) {
				consolePerson(t, h, runtimedb.RoleOwner, nil)
				// A second owner, so the demotion is not refused for
				// leaving the household without one (§2.5).
				if err := h.s.DB.RecordButlerMember("other", runtimedb.RoleOwner, "", ""); err != nil {
					t.Fatal(err)
				}
				if err := h.s.DB.AdmitButlerMember("other", "root-admin"); err != nil {
					t.Fatal(err)
				}
				if err := h.s.DB.SetButlerMemberRole("consoleholder", runtimedb.RoleMember, "other"); err != nil {
					t.Fatal(err)
				}
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "a disabled owner is refused",
			setup: func(t *testing.T, h *harness) {
				consolePerson(t, h, runtimedb.RoleOwner, func(t *testing.T, h *harness) {
					if err := h.s.DB.DisableButlerMember("consoleholder", "root-admin"); err != nil {
						t.Fatal(err)
					}
				})
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "an offboarded owner is refused",
			setup: func(t *testing.T, h *harness) {
				consolePerson(t, h, runtimedb.RoleOwner, func(t *testing.T, h *harness) {
					if err := h.s.DB.OffboardButlerMember("consoleholder", "root-admin"); err != nil {
						t.Fatal(err)
					}
				})
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "an un-admitted person is refused",
			setup: func(t *testing.T, h *harness) {
				// Recorded but never admitted.
				if err := h.s.DB.RecordButlerMember("consoleholder", runtimedb.RoleOwner, "", ""); err != nil {
					t.Fatal(err)
				}
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "a name the household has no record of passes while it has no owner",
			// The bootstrap window. Person records are created through
			// an API behind this check, so before the first Owner the
			// directory group is the only statement anybody has made
			// about who administers this house.
			setup: func(t *testing.T, h *harness) {},
			want:  http.StatusOK,
		},
		{
			name: "a name the household has no record of is refused once it has an owner",
			// And the window closes. A directory administrator adding an
			// account to console_users must not hand out the control
			// plane from outside Wildstone (§3.2).
			setup: func(t *testing.T, h *harness) {
				if err := h.s.DB.RecordButlerMember("someone-else", runtimedb.RoleOwner, "", ""); err != nil {
					t.Fatal(err)
				}
				if err := h.s.DB.AdmitButlerMember("someone-else", "root-admin"); err != nil {
					t.Fatal(err)
				}
			},
			want: http.StatusUnauthorized,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.s.TrustedIngress = []string{"127.0.0.1", "::1"}
			tc.setup(t, h)
			res := h.forwarded(http.MethodGet, "/api/v1/butlers", "consoleholder", ConsoleGroup)
			if res.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.want)
			}
		})
	}
}

// consolePerson records and admits "consoleholder" with the given role,
// then runs an optional lifecycle step against them.
func consolePerson(t *testing.T, h *harness, role string, after func(*testing.T, *harness)) {
	t.Helper()
	if err := h.s.DB.RecordButlerMember("consoleholder", role, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AdmitButlerMember("consoleholder", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if role == runtimedb.RoleOwner && after != nil {
		// Disabling or offboarding the only Owner is refused by the
		// Last Owner invariant, so give the household a second one.
		if err := h.s.DB.RecordButlerMember("spare-owner", runtimedb.RoleOwner, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.AdmitButlerMember("spare-owner", "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if after != nil {
		after(t, h)
	}
}

// Spelling must not be a way past the lifecycle check.
//
// Handles are compared exactly everywhere else (ADR-W032 A4, still
// open), so `Admin` and `admin` are two records. An exact lookup in the
// Console guard would miss the Person whenever the proxy asserted a
// different capitalisation and fall into the permissive "no record"
// branch — turning a known data-hygiene defect into a way to keep the
// control plane after being disabled.
func TestForwardedConsoleAuthIsNotFooledByCapitalisation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded string
		asserted string
		want     int
	}{
		{"exact match, still refused", "consoleholder", "consoleholder", http.StatusUnauthorized},
		{"asserted lower, recorded upper", "ConsoleHolder", "consoleholder", http.StatusUnauthorized},
		{"asserted upper, recorded lower", "consoleholder", "CONSOLEHOLDER", http.StatusUnauthorized},
		{"mixed both ways", "CoNsOlEhOlDeR", "cOnSoLeHoLdEr", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.s.TrustedIngress = []string{"127.0.0.1", "::1"}
			// Recorded as a Member, so they may not hold the Console
			// whatever the proxy spells them as.
			if err := h.s.DB.RecordButlerMember(tc.recorded, runtimedb.RoleMember, "", ""); err != nil {
				t.Fatal(err)
			}
			if err := h.s.DB.AdmitButlerMember(tc.recorded, "root-admin"); err != nil {
				t.Fatal(err)
			}
			res := h.forwarded(http.MethodGet, "/api/v1/butlers", tc.asserted, ConsoleGroup)
			if res.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.want)
			}
		})
	}
}

// With two records differing only by case there is no way to tell which
// one the proxy meant, so the check refuses rather than guessing.
func TestForwardedConsoleAuthRefusesAnAmbiguousHandle(t *testing.T) {
	h := newHarness(t)
	h.s.TrustedIngress = []string{"127.0.0.1", "::1"}
	for handle, role := range map[string]string{
		"twin": runtimedb.RoleOwner,
		"Twin": runtimedb.RoleMember,
	} {
		if err := h.s.DB.RecordButlerMember(handle, role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.AdmitButlerMember(handle, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if res := h.forwarded(http.MethodGet, "/api/v1/butlers", "twin", ConsoleGroup); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an ambiguous handle was admitted: %d", res.StatusCode)
	}
}

// bearer issues a request with a specific token rather than the
// harness's stock one for a role, because these tests are about a token
// that names a PERSON and what happens to it when that person changes.
func (h *harness) bearer(t *testing.T, token, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	res.Body.Close()
	return res
}

// Demoting an Owner invalidates the elevated session they are HOLDING,
// not just the next door they walk through (design §3.2, §18.1 —
// "Demoting an Owner immediately invalidates Console access/session").
//
// The forward-auth door already refuses them on their next request, and
// that is tested above. A token is the other half and the harder one: it
// is minted once and then presented, so nothing re-derives the holder's
// standing unless something asks. Authenticate checked the token was not
// revoked or expired and the PRINCIPAL was not disabled — never that the
// person it acts for is still someone who may hold this.
//
// So an Owner could mint themselves an admin token, be demoted, and keep
// the whole control plane until the token expired. "Immediately" has to
// mean the credential in their hand, or demotion is a door that locks
// while the window stays open.
func TestDemotionInvalidatesAnElevatedTokenHeldByThatPerson(t *testing.T) {
	h := newHarness(t)
	// A second Owner, so the demotion is not refused for leaving the
	// household without one (§2.5).
	for _, who := range []string{"consoleholder", "other"} {
		if err := h.s.DB.RecordButlerMember(who, runtimedb.RoleOwner, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.AdmitButlerMember(who, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	token, err := h.s.Principals.Mint("root-admin", "consoleholder", 0)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}

	// It works while they are an Owner.
	if res := h.bearer(t, token, http.MethodGet, "/api/v1/butlers"); res.StatusCode != http.StatusOK {
		t.Fatalf("an owner's token: status = %d, want 200", res.StatusCode)
	}

	if err := h.s.DB.SetButlerMemberRole("consoleholder", runtimedb.RoleMember, "other"); err != nil {
		t.Fatal(err)
	}

	if res := h.bearer(t, token, http.MethodGet, "/api/v1/butlers"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a demoted owner's token still works: status = %d, want 401", res.StatusCode)
	}
}

// The same for the rest of the lifecycle, because demotion is only one
// of the ways a person stops being someone who may hold this. An
// offboarded or disabled person's token is the same hole wearing a
// different hat.
func TestAnElevatedTokenDiesWithThePersonItActsFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, h *harness)
	}{
		{"disabled", func(t *testing.T, h *harness) {
			if err := h.s.DB.DisableButlerMember("consoleholder", "other"); err != nil {
				t.Fatal(err)
			}
		}},
		{"offboarded", func(t *testing.T, h *harness) {
			if err := h.s.DB.OffboardButlerMember("consoleholder", "other"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			for _, who := range []string{"consoleholder", "other"} {
				if err := h.s.DB.RecordButlerMember(who, runtimedb.RoleOwner, "", ""); err != nil {
					t.Fatal(err)
				}
				if err := h.s.DB.AdmitButlerMember(who, "root-admin"); err != nil {
					t.Fatal(err)
				}
			}
			token, err := h.s.Principals.Mint("root-admin", "consoleholder", 0)
			if err != nil {
				t.Fatalf("minting: %v", err)
			}
			if res := h.bearer(t, token, http.MethodGet, "/api/v1/butlers"); res.StatusCode != http.StatusOK {
				t.Fatalf("before: status = %d, want 200", res.StatusCode)
			}
			tc.end(t, h)
			if res := h.bearer(t, token, http.MethodGet, "/api/v1/butlers"); res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("after: status = %d, want 401", res.StatusCode)
			}
		})
	}
}

// The counterpart, and the one that stops the fix above from being a
// blunt instrument: a NON-elevated credential acting for a Member keeps
// working, because that is what a household Butler is.
//
// A concierge acting for a Member holds converse and nothing else. If
// the acting-person check demanded Owner of every token, every family
// chat binding would stop the moment it shipped — the fix would have
// closed an escalation by breaking the ordinary case, which is how a
// security change gets reverted rather than kept.
func TestAMembersOwnCredentialIsNotRevokedByBeingAMember(t *testing.T) {
	h := newHarness(t)
	if err := h.s.DB.RecordButlerMember("sam", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AdmitButlerMember("sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Principals.Create("family-bot", runtimedb.RoleConcierge, ""); err != nil {
		t.Fatal(err)
	}
	token, err := h.s.Principals.Mint("family-bot", "sam", 0)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	// A route a concierge may reach: converse, not manage.
	res := h.bearer(t, token, http.MethodGet, "/api/v1/chat/outbound?butler=nobody")
	if res.StatusCode == http.StatusUnauthorized {
		t.Fatal("a member's own concierge credential was refused for being a member's")
	}

	// But offboarding them still stops it: lifecycle applies to
	// everybody, and it is only the OWNER requirement that is scoped to
	// elevated credentials.
	if err := h.s.DB.OffboardButlerMember("sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if res := h.bearer(t, token, http.MethodGet, "/api/v1/chat/outbound?butler=nobody"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an offboarded member's credential: status = %d, want 401", res.StatusCode)
	}
}

// A machine credential that acts for nobody is untouched.
//
// Most of the control plane runs on these — the adapters, the node
// agent, root-admin itself before anybody is on record — and a check
// that resolved "" to "no such person" would have failed them all
// closed. Worth its own test because the empty case is the one a later
// refactor drops.
func TestACredentialActingForNobodyIsUnaffected(t *testing.T) {
	h := newHarness(t)
	if err := h.s.DB.RecordButlerMember("consoleholder", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	token, err := h.s.Principals.Mint("root-admin", "", 0)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if res := h.bearer(t, token, http.MethodGet, "/api/v1/butlers"); res.StatusCode != http.StatusOK {
		t.Fatalf("a credential acting for nobody: status = %d, want 200", res.StatusCode)
	}
}

// Refusing an unknown name must not be a way to lock a household out of
// its own control plane.
//
// This is the test that makes the refusal above safe to ship rather than
// merely correct. Person records are created through an API that lives
// behind the Console check, so a rule that refused unknown names
// unconditionally would leave a fresh deployment with no way in and no
// way to fix it. Two properties keep that from happening, and both are
// asserted here: before the first Owner the group still decides, and a
// bearer token acting for nobody is unaffected either way — which is the
// bootstrap credential the installer (§2.4) and the root CLI (§2.6) will
// formalise.
func TestTheBootstrapPathSurvivesRefusingUnknownNames(t *testing.T) {
	h := newHarness(t)
	h.s.TrustedIngress = []string{"127.0.0.1", "::1"}

	// Nothing provisioned: the group decides, so somebody can get in and
	// create the first Owner.
	if res := h.forwarded(http.MethodGet, "/api/v1/butlers", "installer", ConsoleGroup); res.StatusCode != http.StatusOK {
		t.Fatalf("an un-provisioned household refused its own installer: %d", res.StatusCode)
	}

	// They create it. From here the window is shut.
	if err := h.s.DB.RecordButlerMember("first-owner", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AdmitButlerMember("first-owner", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if res := h.forwarded(http.MethodGet, "/api/v1/butlers", "installer", ConsoleGroup); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the window did not close: %d", res.StatusCode)
	}
	// And the Owner they created can still get in, which is the point of
	// closing it rather than the cost of it.
	if res := h.forwarded(http.MethodGet, "/api/v1/butlers", "first-owner", ConsoleGroup); res.StatusCode != http.StatusOK {
		t.Fatalf("the owner was refused: %d", res.StatusCode)
	}

	// The bootstrap token is the break-glass and is untouched by any of
	// it: it acts for nobody, so there is nothing to re-derive.
	token, err := h.s.Principals.Mint("root-admin", "", 0)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if res := h.bearer(t, token, http.MethodGet, "/api/v1/butlers"); res.StatusCode != http.StatusOK {
		t.Fatalf("the bootstrap token was refused: %d", res.StatusCode)
	}
}

// A refusal is a refusal, not a lesser role (design §3.2).
//
// Asserted on the IDENTITY rather than the status code, because those
// are different claims. A handler can answer 401 for a dozen reasons; a
// degraded Console identity — admitted, but as something narrower —
// would be a caller the rest of the API was never written for, and every
// screen it reached would answer 403 while looking signed in. The
// endpoint test above cannot tell the two apart. This one can.
func TestAConsoleRefusalIsNotADegradedIdentity(t *testing.T) {
	h := newHarness(t)
	h.s.TrustedIngress = []string{"127.0.0.1", "::1"}
	// A Member who IS in the Console group: the central case, refused
	// for who they are rather than for anything about the request.
	if err := h.s.DB.RecordButlerMember("consoleholder", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AdmitButlerMember("consoleholder", "root-admin"); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/butlers", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(TrustedIngressHeader, "consoleholder")
	req.Header.Set(ConsoleGroupsHeader, ConsoleGroup)
	req.RemoteAddr = "127.0.0.1:1234"

	id, ok := h.s.consoleForwardAuth(req)
	if ok {
		t.Fatal("a member in the console group was admitted")
	}
	if id != (principal.Identity{}) {
		t.Fatalf("a refusal produced an identity: %+v", id)
	}
}
