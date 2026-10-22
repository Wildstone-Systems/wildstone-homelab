package api

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/domestic"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// knowledgeSettingsFile is the file the knowledge capability renders
// (packages/knowledge-wildstone, ADR-W046), and the only way the
// knowledge folder's path reaches this process. Read from desired state
// for the reason filedropSettingsFile is: the capability installs no
// container, and an install after startup must reach the agent tools.
const knowledgeSettingsFile = "knowledge.yaml"

type knowledgeSettings struct {
	// Root is the Owner-chosen host folder holding the notes.
	Root string `yaml:"root"`
}

// KnowledgeRoot reports where the household's knowledge folder is, from
// current desired state. Empty with no error means the capability is not
// installed or names no folder, which the tools report as such.
//
// A malformed settings file is an error rather than an empty answer:
// falling back to a default folder would be inventing a place to keep
// the household's notes.
func (s *Server) KnowledgeRoot() (string, error) {
	if s == nil || s.State == nil {
		return "", fmt.Errorf("this control plane cannot read desired state")
	}
	desired, err := s.State.Load()
	if err != nil {
		return "", err
	}
	return knowledgeRootIn(desired)
}

func knowledgeRootIn(desired []resource.Resource) (string, error) {
	for _, r := range desired {
		if r.Kind != resource.KindConfigFile || r.ConfigFile == nil {
			continue
		}
		if strings.TrimPrefix(r.Metadata.Owner, "capability/") != domestic.KnowledgeCapability {
			continue
		}
		if r.ConfigFile.Filename != knowledgeSettingsFile {
			continue
		}
		var settings knowledgeSettings
		if err := yaml.Unmarshal([]byte(r.ConfigFile.Content), &settings); err != nil {
			return "", fmt.Errorf("the knowledge capability's settings are unreadable: %w", err)
		}
		return strings.TrimSpace(settings.Root), nil
	}
	return "", nil
}
