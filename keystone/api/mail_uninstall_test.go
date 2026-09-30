package api

// Epic #326 S08: removing the package that bundles the mail adapter
// fences every Personal monitor it runs, then removes their adapter
// containers, then proves their credentials erased — in that order —
// and puts the adapter's rendered resources (owned by
// integration/<connection>, which a capability removal never reaches)
// into the removal Plan. A dry run changes nothing.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestRemovingTheMailPackageFencesThenRemovesThenErases(t *testing.T) {
	c := newCredentialHarness(t)
	installMailForTest(t, c)
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword}, nil); code != http.StatusOK {
		t.Fatalf("intake = %d %s", code, body)
	}
	authorizeSam(t, c)
	// sam's adapter as an approved render left it in desired state.
	pkg, _ := c.s.Catalog.Get("mail-roundcube", "")
	in, _ := pkg.AdapterFor("mail")
	for _, n := range []string{"n1", "n2"} {
		if err := c.s.State.Put(resource.Resource{
			ApiVersion: resource.ApiVersion, Kind: resource.KindContainer,
			Metadata:  resource.Metadata{Name: capability.AdapterContainerName("mail", "sam-mail", n), Owner: capability.AdapterOwner("sam-mail"), Node: n},
			Container: &resource.ContainerSpec{Image: in.Adapter.Image},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.s.State.Commit("test", "render", "sam-mail adapter"); err != nil {
		t.Fatal(err)
	}
	disable := func(dry bool) map[string]any {
		t.Helper()
		var out map[string]any
		code, body := c.consoleCall(c.mika, http.MethodPost, "/api/v1/capabilities/disable",
			map[string]any{"package": "mail-roundcube", "dryRun": dry}, &out)
		if code != http.StatusOK && code != http.StatusCreated {
			t.Fatalf("disable(dry=%v) = %d %s", dry, code, body)
		}
		c.assertValueNowhere(t, []string{body}, appPassword)
		return out
	}

	// A dry run names what it would fence and touches nothing.
	out := disable(true)
	if got, _ := out["adapterWouldFence"].([]any); len(got) != 2 {
		t.Fatalf("dry run = %v", out)
	}
	if _, err := c.s.DB.LiveBackgroundAuthorization("sam-mail"); err != nil {
		t.Fatal("a dry run revoked sam's authorization")
	}
	if _, ok := c.nodes.holds("n1", "pc.sam-mail.v1"); !ok {
		t.Fatal("a dry run erased sam's credential")
	}

	// The real removal: at the moment each copy is erased, the fence has
	// already happened and the adapter that held the value is gone.
	before, _ := c.s.DB.GetConnection("sam-mail")
	c.nodes.onErase = func(node, name string) {
		if !strings.HasPrefix(name, "pc.sam-mail.") {
			return
		}
		conn, _ := c.s.DB.GetConnection("sam-mail")
		if conn.LeaseEpoch <= before.LeaseEpoch {
			t.Errorf("%s on %s erased before the fence", name, node)
		}
		if _, err := c.s.DB.LiveBackgroundAuthorization("sam-mail"); err == nil {
			t.Errorf("%s on %s erased while the authorization was live", name, node)
		}
		c.nodes.mu.Lock()
		running := c.nodes.containers[node]["mail-adapter-sam-mail-"+node]
		c.nodes.mu.Unlock()
		if running {
			t.Errorf("%s on %s erased while its adapter container still ran", name, node)
		}
	}
	out = disable(false)
	fenced, _ := out["adapterFenced"].([]any)
	if len(fenced) != 2 || fenced[1] != "sam-mail" {
		t.Fatalf("fenced = %v", out["adapterFenced"])
	}
	for _, n := range []string{"n1", "n2"} {
		for _, name := range []string{"pc.sam-mail.v1", "pc.sam-mail.fk.v1"} {
			if _, ok := c.nodes.holds(n, name); ok {
				t.Fatalf("%s is still on %s after the package was removed", name, n)
			}
		}
	}
	if pending, _ := c.s.DB.OutstandingCredentialErasures("sam-mail"); len(pending) != 0 {
		t.Fatalf("unproven erasures: %+v", pending)
	}
	if cred, err := c.s.DB.ActivePersonalCredential("sam-mail", runtimedb.MailboxCredentialNeed); err == nil {
		t.Fatalf("the credential is still active: %+v", cred)
	}
	// The removal Plan takes the adapter's desired state with it.
	id, _ := out["id"].(string)
	rec, err := c.s.DB.GetPlan(id)
	if err != nil {
		t.Fatalf("plan %q: %v (%v)", id, err, out)
	}
	for _, n := range []string{"n1", "n2"} {
		if !strings.Contains(rec.PlanJSON, capability.AdapterContainerName("mail", "sam-mail", n)) {
			t.Fatalf("the removal Plan leaves sam's adapter on %s: %s", n, rec.PlanJSON)
		}
	}
}
