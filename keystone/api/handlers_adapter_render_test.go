package api

// Epic #326: a Personal connection's adapter placement reaches desired
// state only as a Plan an Owner approves, only while the credential, the
// executable and the Person's live authorization under an approved
// subscription all hold, and leaves it — again by Plan — when any stops
// holding. Installing the package renders nothing.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// installMailForTest stands in for an approved mail-roundcube install:
// its container in desired state and its install origin (params) on an
// approved Plan, which is all packageIsInstalled and installedFromPlans
// read.
func installMailForTest(t *testing.T, c *credentialHarness) *pkgspec.Package {
	t.Helper()
	cat, err := catalog.LoadDir("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	c.s.Catalog = cat
	c.s.APIURL = "http://192.0.2.10:9441"
	pkg, err := cat.Get("mail-roundcube", "")
	if err != nil {
		t.Fatal(err)
	}
	in, _ := pkg.AdapterFor("mail")
	names := pkgspec.ContainerNames(pkg)
	if len(names) == 0 {
		t.Fatal("mail-roundcube declares no container")
	}
	if err := c.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindContainer,
		Metadata:  resource.Metadata{Name: names[0], Owner: "capability/mail", Package: "mail-roundcube", Node: "n1"},
		Container: &resource.ContainerSpec{Image: in.Adapter.Image},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.s.State.Commit("test", "install", "mail-roundcube"); err != nil {
		t.Fatal(err)
	}
	head, err := c.s.State.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.s.DB.SetStatefileHead(head); err != nil {
		t.Fatal(err)
	}
	stored, _ := json.Marshal(plan.StoredProposal{Origin: &plan.CapabilityOrigin{
		Package: "mail-roundcube", Version: pkg.Metadata.Version, Params: map[string]string{
			"public_domain": "example.internal", "node": "n1", "imap_host": "imap.example.internal",
			"smtp_host": "smtp.example.internal", "username_domain": "example.internal",
		}}})
	id, err := c.s.DB.InsertPlan("root-admin", "", "enable capability mail", "low", string(stored))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.s.DB.TransitionPlan(id, runtimedb.PlanApproved, "mika"); err != nil {
		t.Fatal(err)
	}
	return pkg
}

// adapterResources is what desired state holds for the connection.
func adapterResources(t *testing.T, c *credentialHarness, connection string) []resource.Resource {
	t.Helper()
	current, err := c.s.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	var out []resource.Resource
	for _, r := range current {
		if r.Metadata.Owner == capability.AdapterOwner(connection) {
			out = append(out, r)
		}
	}
	return out
}

// proposedRenders are the undecided render Plans for the connection.
func proposedRenders(t *testing.T, c *credentialHarness, connection string) []runtimedb.PlanRecord {
	t.Helper()
	plans, err := c.s.DB.ListPlans(0)
	if err != nil {
		t.Fatal(err)
	}
	var out []runtimedb.PlanRecord
	for _, p := range plans {
		if p.State == runtimedb.PlanProposed && strings.HasPrefix(p.Summary, adapterPlanPrefix(connection)) {
			out = append(out, p)
		}
	}
	return out
}

func (c *credentialHarness) approve(t *testing.T, planID string) {
	t.Helper()
	if code, body := c.consoleCall(c.mika, http.MethodPost, "/api/v1/plans/"+planID+"/approve", nil, nil); code != http.StatusOK {
		t.Fatalf("approving %s = %d %s", planID, code, body)
	}
}

// authorizeSam runs the Person/Owner intersection for sam-mail: preview,
// an Owner-approved scheduled subscription naming it, sam's
// authorization of that subscription.
func authorizeSam(t *testing.T, c *credentialHarness) int64 {
	t.Helper()
	req := map[string]any{"agent": c.agent, "targetCalendar": "sam-family",
		"scope": map[string]any{"folders": []string{"INBOX"}}}
	var preview struct {
		SubscriptionMatch map[string]string `json:"subscriptionMatch"`
	}
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/authorization", req, &preview); code != http.StatusOK {
		t.Fatalf("preview = %d %s", code, body)
	}
	match, _ := json.Marshal(preview.SubscriptionMatch)
	sub, err := c.s.DB.ProposeAgentSubscription(runtimedb.AgentSubscriptionSpec{
		Butler: c.agent, TriggerKind: runtimedb.SubscriptionTriggerSchedule, MatchKind: "mail.changes",
		MatchJSON: string(match), Audience: runtimedb.AudienceOwner, Capabilities: []string{"mail.changes"},
		SourceConnection: "sam-mail", Cadence: 15 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.s.DB.ApproveAgentSubscription(sub, "mika"); err != nil {
		t.Fatal(err)
	}
	req["subscription"] = sub
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/authorization", req, nil); code != http.StatusOK {
		t.Fatalf("authorize = %d %s", code, body)
	}
	return sub
}

func containersByNode(rs []resource.Resource) map[string]resource.Resource {
	out := map[string]resource.Resource{}
	for _, r := range rs {
		if r.Kind == resource.KindContainer {
			out[r.Metadata.Node] = r
		}
	}
	return out
}

func TestAdapterPlacementFollowsTheConnectionByPlan(t *testing.T) {
	c := newCredentialHarness(t)
	pkg := installMailForTest(t, c)
	exe := "mail-roundcube@" + pkg.Metadata.Version + "/wild-mail-adapter"

	// Installed, nobody watching anything: nothing is rendered.
	if got := adapterResources(t, c, "sam-mail"); len(got) != 0 {
		t.Fatalf("installing mail-roundcube rendered %d adapter resources", len(got))
	}

	// A Member cannot prepare a placement; an Owner can, and it files no
	// containers while nothing is authorized — only the executable and
	// the node-bound tokens.
	if code, _ := c.homeCall("sam", http.MethodPost, "/api/v1/integrations/sam-mail/adapter/render", nil, nil); code == http.StatusOK {
		t.Fatal("a Member rendered an adapter placement")
	}
	var out map[string]any
	if code, body := c.consoleCall(c.mika, http.MethodPost, "/api/v1/integrations/sam-mail/adapter/render", nil, &out); code != http.StatusOK {
		t.Fatalf("render = %d %s", code, body)
	}
	if out["eligible"] != false || out["plan"] != nil {
		t.Fatalf("with no credential the render filed something: %v", out)
	}
	if conn, _ := c.s.DB.GetConnection("sam-mail"); conn.Executable != exe {
		t.Fatalf("executable = %q, want %q", conn.Executable, exe)
	}
	for _, n := range []string{"n1", "n2"} {
		if _, ok := c.nodes.holds(n, capability.AdapterTokenSecretName("mail", "sam-mail", n)); !ok {
			t.Fatalf("%s has no adapter token", n)
		}
		p, err := c.s.DB.GetPrincipal(capability.AdapterPrincipalName("mail", "sam-mail", n))
		if err != nil || p.Role != runtimedb.RoleIntegrationAdapter {
			t.Fatalf("%s's adapter principal = %+v, %v", n, p, err)
		}
	}

	// Credential, then the Person's authorization under an approved
	// subscription: a render Plan is filed, and nothing is desired until
	// an Owner approves it.
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword}, nil); code != http.StatusOK {
		t.Fatalf("intake = %d %s", code, body)
	}
	if got := proposedRenders(t, c, "sam-mail"); len(got) != 0 {
		t.Fatalf("a credential alone filed a render: %v", got[0].Summary)
	}
	authorizeSam(t, c)
	renders := proposedRenders(t, c, "sam-mail")
	if len(renders) != 1 {
		t.Fatalf("%d render plans after authorizing, want 1", len(renders))
	}
	if got := adapterResources(t, c, "sam-mail"); len(got) != 0 {
		t.Fatal("the adapter reached desired state before an Owner approved it")
	}
	c.approve(t, renders[0].ID)
	running := containersByNode(adapterResources(t, c, "sam-mail"))
	if len(running) != 2 {
		t.Fatalf("containers rendered on %d nodes, want one per placement node", len(running))
	}
	for _, n := range []string{"n1", "n2"} {
		ct, ok := running[n]
		if !ok || ct.Metadata.Name != capability.AdapterContainerName("mail", "sam-mail", n) {
			t.Fatalf("no adapter on %s: %+v", n, running)
		}
		env := ct.Container.Env
		if env["WILD_SECRET_MAILBOX_APP_PASSWORD"] != "secret/pc.sam-mail.v1" ||
			env["KEYSTONE_TOKEN"] != "secret/"+capability.AdapterTokenSecretName("mail", "sam-mail", n) ||
			env["WILD_MAILBOX_USERNAME"] != "sam@example.internal" || !strings.Contains(env["WILD_MAILBOX_FILTERS"], "INBOX") ||
			env["WILD_NODE"] != n || env["KEYSTONE_API"] != "http://192.0.2.10:9441" {
			t.Fatalf("%s env = %v", n, env)
		}
		for _, v := range env {
			if strings.Contains(v, appPassword) {
				t.Fatal("the credential value reached desired state")
			}
		}
	}

	// Replacing the credential revokes the authorization bound to the
	// old one (S02): removal is proposed, by Plan.
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword2}, nil); code != http.StatusOK {
		t.Fatalf("replace = %d %s", code, body)
	}
	renders = proposedRenders(t, c, "sam-mail")
	if len(renders) != 1 || !strings.Contains(renders[0].Summary, "remove") {
		t.Fatalf("after replacement: %+v", renders)
	}
	c.approve(t, renders[0].ID)
	if got := adapterResources(t, c, "sam-mail"); len(got) != 0 {
		t.Fatalf("%d adapter resources survive the approved removal", len(got))
	}

	// Authorized again for the new credential: re-rendered with it.
	authorizeSam(t, c)
	renders = proposedRenders(t, c, "sam-mail")
	if len(renders) != 1 {
		t.Fatalf("%d render plans after re-authorizing, want 1", len(renders))
	}
	stale := renders[0].ID

	// Revoked before an Owner decided: the undecided render is
	// superseded, so it can never be approved into a revoked placement.
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/authorization/revoke", nil, nil); code != http.StatusOK {
		t.Fatalf("revoke = %d %s", code, body)
	}
	if rec, _ := c.s.DB.GetPlan(stale); rec.State != runtimedb.PlanSuperseded {
		t.Fatalf("the stale render is %s, want superseded", rec.State)
	}
	if got := proposedRenders(t, c, "sam-mail"); len(got) != 0 {
		t.Fatalf("revocation with nothing rendered filed %d plans", len(got))
	}

	// And the re-render itself: authorize again and approve — the new
	// credential version is what the adapter is given.
	authorizeSam(t, c)
	renders = proposedRenders(t, c, "sam-mail")
	if len(renders) != 1 {
		t.Fatalf("%d render plans, want 1", len(renders))
	}
	c.approve(t, renders[0].ID)
	for n, ct := range containersByNode(adapterResources(t, c, "sam-mail")) {
		if got := ct.Container.Env["WILD_SECRET_MAILBOX_APP_PASSWORD"]; got != "secret/pc.sam-mail.v2" {
			t.Fatalf("%s re-rendered with %s, want the replacement", n, got)
		}
	}
}

// Disabling the subscription the Person authorized takes the adapter
// down, by Plan.
func TestAdapterPlacementLeavesWhenTheSubscriptionIsDisabled(t *testing.T) {
	c := newCredentialHarness(t)
	installMailForTest(t, c)
	if code, body := c.consoleCall(c.mika, http.MethodPost, "/api/v1/integrations/sam-mail/adapter/render", nil, nil); code != http.StatusOK {
		t.Fatalf("render = %d %s", code, body)
	}
	if code, _ := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword}, nil); code != http.StatusOK {
		t.Fatal("intake failed")
	}
	sub := authorizeSam(t, c)
	c.approve(t, proposedRenders(t, c, "sam-mail")[0].ID)
	if len(adapterResources(t, c, "sam-mail")) == 0 {
		t.Fatal("nothing rendered")
	}
	if code, body := c.consoleCall(c.mika, http.MethodPost, "/api/v1/agent-subscriptions/"+itoa(sub)+"/control",
		map[string]string{"action": "disable"}, nil); code != http.StatusOK {
		t.Fatalf("disable = %d %s", code, body)
	}
	renders := proposedRenders(t, c, "sam-mail")
	if len(renders) != 1 || !strings.Contains(renders[0].Summary, "not approved and enabled") {
		t.Fatalf("after disabling: %+v", renders)
	}
}
