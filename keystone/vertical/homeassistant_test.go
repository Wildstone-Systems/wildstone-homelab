package vertical

// The Home Assistant adapter (ADR-W022 A10/A8, work package H5): the
// first third-party service to publish into the household stream.
//
// What needs proving is not that a YAML file exists — it is that the
// file Wildstone renders is one Home Assistant can actually load, that
// it carries the credential reference the node will resolve, and that
// what the package DECLARES it publishes is what the configuration
// actually publishes. That last one is the coupling this test exists
// for: a declaration and a config that drift apart produce a household
// stream full of events nothing declared, or rules waiting on events
// nothing sends.

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

const (
	testIntakeURL = "https://wildstone.example.invalid/api/v1/household/events"
	testPeople    = "person.mika, person.sam"
)

// haConfig is the shape this test reads back out of the rendered file —
// only the parts Wildstone is responsible for getting right.
type haConfig struct {
	RestCommand map[string]struct {
		URL         string            `yaml:"url"`
		Method      string            `yaml:"method"`
		ContentType string            `yaml:"content_type"`
		Headers     map[string]string `yaml:"headers"`
		Payload     string            `yaml:"payload"`
	} `yaml:"rest_command"`
	Automation []struct {
		ID      string `yaml:"id"`
		Alias   string `yaml:"alias"`
		Mode    string `yaml:"mode"`
		Trigger []struct {
			Platform string `yaml:"platform"`
			EntityID string `yaml:"entity_id"`
			To       string `yaml:"to"`
			From     string `yaml:"from"`
			ID       string `yaml:"id"`
		} `yaml:"trigger"`
		Condition []struct {
			Condition     string `yaml:"condition"`
			ValueTemplate string `yaml:"value_template"`
		} `yaml:"condition"`
		Action []struct {
			Service string            `yaml:"service"`
			Data    map[string]string `yaml:"data"`
		} `yaml:"action"`
	} `yaml:"automation"`
}

func expandHomeAutomation(t *testing.T) (*pkgspec.Package, []resource.Resource) {
	t.Helper()
	root := repoRoot(t)
	pkg, err := pkgspec.Load(filepath.Join(root, "packages", "home-automation", "package.yaml"))
	if err != nil {
		t.Fatalf("loading packages/home-automation/package.yaml: %v", err)
	}
	if err := pkg.Validate(); err != nil {
		t.Fatalf("validating the package: %v", err)
	}
	out, err := capability.NewEngine().Expand(pkg, map[string]string{
		"node": "node-a", "intake_url": testIntakeURL, "people": testPeople,
	}, capability.NewCatalog(pkg))
	if err != nil {
		t.Fatalf("expanding the package: %v", err)
	}
	return pkg, out
}

func TestHomeAssistantAdapterExpandsToLoadableConfig(t *testing.T) {
	pkg, out := expandHomeAutomation(t)

	// The shape is "config" (ADR-W022 A10): one managed file and nothing
	// else. No container — the household already runs Home Assistant,
	// and adopting it is a different package.
	if pkg.Publishes == nil || pkg.Publishes.Shape != "config" {
		t.Fatalf("publishes = %+v, want shape config", pkg.Publishes)
	}
	var cfg *resource.Resource
	var publisherSecret *resource.Resource
	for i := range out {
		switch out[i].Kind {
		case resource.KindConfigFile:
			cfg = &out[i]
		case resource.KindSecret:
			publisherSecret = &out[i]
		default:
			t.Fatalf("unexpected %s/%s: this package should render a ConfigFile and its publisher Secret, nothing else",
				out[i].Kind, out[i].Metadata.Name)
		}
	}
	if cfg == nil {
		t.Fatal("no ConfigFile rendered")
	}
	if cfg.ConfigFile.Filename != "wildstone.yaml" || cfg.Metadata.Owner != "capability/home-automation" || cfg.Metadata.Node != "node-a" {
		t.Fatalf("config file = %+v / %+v", cfg.Metadata, cfg.ConfigFile)
	}

	// Declaring publishes earns the capability a publisher credential,
	// provided rather than generated: Keystone mints the token and puts
	// it on the node, so the reconciler must not invent one.
	if publisherSecret == nil || publisherSecret.Secret.Name != "home-automation-publisher" ||
		!publisherSecret.Secret.Provided || publisherSecret.Metadata.Node != "node-a" {
		t.Fatalf("publisher secret = %+v", publisherSecret)
	}

	// The rendered file must be YAML Home Assistant can load. Nothing
	// upstream checks this: a ConfigFile's content is an opaque string to
	// the resource schema, so a broken render reaches the node intact and
	// fails there, in Home Assistant's log, at 3am.
	content := cfg.ConfigFile.Content
	var ha haConfig
	if err := yaml.Unmarshal([]byte(content), &ha); err != nil {
		t.Fatalf("rendered config is not valid YAML: %v\n---\n%s", err, content)
	}

	// The Go raw-string wrappers that protect Jinja from Wildstone's own
	// templating must be gone, and the Jinja they protected must remain.
	if strings.Contains(content, "`") {
		t.Errorf("a Go raw-string delimiter survived into the rendered file")
	}
	for _, jinja := range []string{
		`{{ trigger.entity_id.split('.')[1] }}`,
		`{{ trigger.to_state.last_changed.isoformat() }}`,
		`{{ trigger.from_state.state != trigger.to_state.state }}`,
		`{{ event_id }}`,
	} {
		if !strings.Contains(content, jinja) {
			t.Errorf("Jinja %q did not survive expansion", jinja)
		}
	}

	// The call Home Assistant will make: our intake, our credential by
	// reference (never a value — the node resolves it at write time).
	rc, ok := ha.RestCommand["wildstone_household_event"]
	if !ok {
		t.Fatalf("no wildstone_household_event rest_command: %+v", ha.RestCommand)
	}
	if rc.URL != testIntakeURL || rc.Method != "POST" || rc.ContentType != "application/json" {
		t.Fatalf("rest_command = %+v", rc)
	}
	if rc.Headers["Authorization"] != "Bearer secret/home-automation-publisher" {
		t.Fatalf("authorization = %q, want a secret reference", rc.Headers["Authorization"])
	}
	// Every envelope field the intake requires has to be in the payload,
	// or Home Assistant posts 400s forever.
	for _, field := range []string{"eventId", "occurredAt", "type", "subject", "payload"} {
		if !strings.Contains(rc.Payload, `"`+field+`"`) {
			t.Errorf("payload omits the required envelope field %q: %s", field, rc.Payload)
		}
	}
}

// TestHomeAssistantDeclarationMatchesItsConfig is the coupling check: the
// event types the package declares under publishes are exactly the ones
// its Home Assistant automation actually sends. The trigger's own id IS
// the canonical type, so the two cannot drift without this failing.
func TestHomeAssistantDeclarationMatchesItsConfig(t *testing.T) {
	pkg, out := expandHomeAutomation(t)

	var declared []string
	for _, ev := range pkg.Publishes.Events {
		declared = append(declared, ev.Type)
		if ev.Subject != "member" {
			t.Errorf("%s declares subject %q; this adapter maps a person entity to a member (ADR-W022 A8)", ev.Type, ev.Subject)
		}
	}
	sort.Strings(declared)

	var ha haConfig
	for _, r := range out {
		if r.Kind == resource.KindConfigFile {
			if err := yaml.Unmarshal([]byte(r.ConfigFile.Content), &ha); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(ha.Automation) != 1 {
		t.Fatalf("want one automation, got %d", len(ha.Automation))
	}
	auto := ha.Automation[0]

	var sent []string
	for _, tr := range auto.Trigger {
		sent = append(sent, tr.ID)
		if tr.EntityID != testPeople {
			t.Errorf("trigger watches %q, want the people param %q", tr.EntityID, testPeople)
		}
	}
	sort.Strings(sent)
	if strings.Join(sent, ",") != strings.Join(declared, ",") {
		t.Fatalf("the package declares %v but its automation sends %v", declared, sent)
	}

	// The event type on the wire is the trigger id, so the two stay
	// equal by construction rather than by care.
	if len(auto.Action) != 1 || auto.Action[0].Data["event_type"] != "{{ trigger.id }}" {
		t.Fatalf("action = %+v; the canonical type must come from the trigger id", auto.Action)
	}
	// The subject is the person entity's object id — the mapping this
	// adapter declares it can achieve (ADR-W022 A8).
	if auto.Action[0].Data["subject"] != `{{ trigger.entity_id.split('.')[1] }}` {
		t.Fatalf("subject = %q, want the person entity's object id", auto.Action[0].Data["subject"])
	}
	// An attribute-only state change is not a presence event.
	if len(auto.Condition) != 1 || !strings.Contains(auto.Condition[0].ValueTemplate, "from_state.state != trigger.to_state.state") {
		t.Fatalf("condition = %+v; an attribute-only change must not publish", auto.Condition)
	}
	// Two people arriving together are two facts.
	if auto.Mode != "queued" {
		t.Fatalf("automation mode = %q, want queued", auto.Mode)
	}
}
