package reconcile

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func matrixAppService(name, localpart string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindMatrixAppService,
		Metadata:   resource.Metadata{Name: name, Node: "node-a", Owner: "capability/chat-matrix-" + name},
		MatrixAppService: &resource.MatrixAppServiceSpec{
			BotLocalpart: localpart,
			AsTokenRef:   "secret/chat-matrix-" + name + "-token",
			HsTokenRef:   "secret/chat-matrix-" + name + "-hs-token",
		},
	}
}

// splitRegistrations does what the matrix-synapse package's Job does with
// the merged file: cut it at each "# registration: <id>" line. Parsing
// each piece as YAML here is what proves the Job's output is loadable.
func splitRegistrations(t *testing.T, content string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	var id string
	var cur strings.Builder
	flush := func() {
		if id == "" {
			return
		}
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(cur.String()), &doc); err != nil {
			t.Fatalf("registration %s is not valid YAML: %v\n%s", id, err, cur.String())
		}
		out[id] = doc
	}
	for _, line := range strings.Split(content, "\n") {
		if rest, ok := strings.CutPrefix(line, "# registration: "); ok {
			flush()
			id, cur = rest, strings.Builder{}
			continue
		}
		if id != "" {
			cur.WriteString(line + "\n")
		}
	}
	flush()
	return out
}

func TestMatrixAppServicesRenderOneLoadableRegistrationPerBot(t *testing.T) {
	desired := []resource.Resource{
		matrixAppService("joan-2", "wildstone-joan-2"),
		matrixAppService("joan", "wildstone-joan"),
		{Kind: resource.KindVolume, Metadata: resource.Metadata{Name: "unrelated"}, Volume: &resource.VolumeSpec{Name: "unrelated"}},
	}
	content := string(renderMatrixAppServices(desired))
	regs := splitRegistrations(t, content)
	if len(regs) != 2 {
		t.Fatalf("rendered %d registrations, want 2:\n%s", len(regs), content)
	}
	if strings.Index(content, "# registration: joan\n") > strings.Index(content, "# registration: joan-2\n") {
		t.Error("registrations are not sorted by id; a reorder would restart Synapse for nothing")
	}
	joan := regs["joan"]
	for key, want := range map[string]any{
		"id":       "joan",
		"as_token": "secret/chat-matrix-joan-token",
		"hs_token": "secret/chat-matrix-joan-hs-token",
		// Not the bot: Synapse refuses /sync to a registration's sender.
		"sender_localpart": "wildstone-joan-as",
		"rate_limited":     false,
	} {
		if joan[key] != want {
			t.Errorf("%s = %#v, want %#v", key, joan[key], want)
		}
	}
	// Synapse refuses a registration with no url key; null is the
	// explicit "push nothing" it accepts.
	if v, ok := joan["url"]; !ok || v != nil {
		t.Errorf("url must be an explicit null, got present=%v value=%#v", ok, v)
	}
	users := joan["namespaces"].(map[string]any)["users"].([]any)
	if len(users) != 1 {
		t.Fatalf("want exactly one user namespace, got %v", users)
	}
	ns := users[0].(map[string]any)
	if ns["exclusive"] != true {
		t.Error("the bot's namespace must be exclusive, or a person could claim its user")
	}
	re := regexp.MustCompile("^(?:" + ns["regex"].(string) + ")$")
	for user, want := range map[string]bool{
		"@wildstone-joan:matrix.example.internal":    true,
		"@wildstone-joan-as:matrix.example.internal": true,
		"@wildstone-joan-2:matrix.example.internal":  false,
		"@wildstone-joanx:matrix.example.internal":   false,
		"@alice:matrix.example.internal":             false,
	} {
		if re.MatchString(user) != want {
			t.Errorf("joan's namespace matches %s = %v, want %v", user, !want, want)
		}
	}
}

func TestMatrixAppServicesWithNoBotsCarryNoReference(t *testing.T) {
	content := string(renderMatrixAppServices(nil))
	// The node refuses a materializing write with nothing to materialize,
	// and every install has zero bots before it has one.
	if configFileNeedsMaterialization(content) {
		t.Fatalf("the empty registrations file carries a secret reference:\n%s", content)
	}
	if strings.Contains(content, "# registration:") {
		t.Fatal("the empty registrations file declares a registration")
	}
}

func TestMatrixAppServicesAreAPrereqAndInTheDigest(t *testing.T) {
	const prefix = "/etc/wildstone"
	desired := []resource.Resource{matrixAppService("joan", "wildstone-joan")}
	var found bool
	for _, f := range nodePrereqFiles(prefix, desired) {
		if f.path != matrixAppServicesPath(prefix) {
			continue
		}
		found = true
		if !f.materialize {
			t.Error("the registrations file carries tokens but is written without materialization")
		}
	}
	if !found {
		t.Fatal("the merged registrations are not a node prereq, so nothing writes them")
	}
	byKey := map[resKey]resource.Resource{{resource.KindMatrixAppService, "joan"}: desired[0]}
	if _, ok := managedConfigContents(prefix, byKey)[matrixAppServicesPath(prefix)]; !ok {
		t.Fatal("the registrations are not in the config digest, so a new bot never restarts Synapse")
	}
}
