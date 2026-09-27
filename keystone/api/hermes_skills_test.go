package api

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"gopkg.in/yaml.v3"
)

// A skill Hermes writes is one store per install, listed in every
// session, so on an Agent several people talk to it would carry one
// person's conversation into another's (#389). Every profile renders
// skill writes as needing approval, which nobody gives on an unattended
// install, while the skills toolset itself stays readable.
func TestHermesCannotCommitTheSkillsItWrites(t *testing.T) {
	c, err := catalog.LoadDir("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := c.Get("agent-hermes", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"platform-only", "sandboxed", "host-admin"} {
		t.Run(profile, func(t *testing.T) {
			rendered, err := capability.NewEngine().Expand(pkg, map[string]string{
				"instance": "health", "node": "n1", "keystone_url": "http://100.64.0.1:9441",
				"model_name": "llama3.2", "execution_profile": profile,
			}, capability.NewCatalog(pkg).WithCurrent(nil))
			if err != nil {
				t.Fatalf("expanding: %v", err)
			}
			var cfg struct {
				Skills struct {
					WriteApproval *bool `yaml:"write_approval"`
				} `yaml:"skills"`
				PlatformToolsets map[string][]string `yaml:"platform_toolsets"`
			}
			if err := yaml.Unmarshal([]byte(configContentFor(t, rendered, "agent-hermes-health-config")), &cfg); err != nil {
				t.Fatalf("the managed config is not YAML: %v", err)
			}
			if cfg.Skills.WriteApproval == nil || !*cfg.Skills.WriteApproval {
				t.Error("skills.write_approval is not true: a skill Hermes writes would land and reach every member")
			}
			readable := false
			for _, ts := range cfg.PlatformToolsets["api_server"] {
				readable = readable || ts == "skills"
			}
			if !readable {
				t.Error("the skills toolset is gone: bundled skills should stay readable")
			}
		})
	}
}
