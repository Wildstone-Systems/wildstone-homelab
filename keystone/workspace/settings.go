// Package workspace owns the private workspace configuration/readiness seam
// (ADR-W047). Artifact storage, metadata and tool grants are separate slices.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"gopkg.in/yaml.v3"
)

const (
	Capability             = "workspace"
	Package                = "workspace-wildstone"
	SettingsFile           = "workspace.yaml"
	DefaultRoot            = "/srv/wildstone/agent-workspaces"
	MaxQuotaBytes    int64 = 1 << 40
	MaxFileBytes     int64 = 10 << 20
	MaxRetentionDays       = 3650
)

// Errors carry no host paths or private data and may be used in tool reasons.
var (
	ErrUnavailable = errors.New("storage_unavailable: workspace storage is unavailable")
	ErrDisabled    = errors.New("storage_unavailable: workspace capability is not installed")
	ErrSettings    = errors.New("storage_unavailable: workspace settings are invalid; ask the Owner to reconfigure")
	ErrNode        = errors.New("storage_unavailable: workspace must be configured on Keystone's known host")
	ErrMount       = errors.New("storage_unavailable: required workspace mount is absent or cannot be verified")
	ErrRoot        = errors.New("storage_unavailable: workspace folder is absent, inaccessible or symlinked; restore storage before retrying")
)

type Settings struct {
	Root                 string `yaml:"root"`
	RequiredMount        string `yaml:"requiredMount"`
	QuotaBytes           int64  `yaml:"quotaBytes"`
	MaxFileBytes         int64  `yaml:"maxFileBytes"`
	ScratchRetentionDays int    `yaml:"scratchRetentionDays"`
	TrashRetentionDays   int    `yaml:"trashRetentionDays"`
	// Revision binds consumers to the complete current configuration, including
	// placement. It is not authority; every operation must reload settings.
	Revision string `yaml:"-"`
}

// Read reads one authoritative current ConfigFile. Missing, ambiguous, malformed
// or remotely placed configuration never falls back to a default root.
func Read(desired []resource.Resource, node string) (Settings, error) {
	var config *resource.Resource
	for i := range desired {
		r := &desired[i]
		if r.Kind != resource.KindConfigFile || r.ConfigFile == nil || r.Metadata.Owner != "capability/"+Capability || r.ConfigFile.Filename != SettingsFile {
			continue
		}
		if config != nil {
			return Settings{}, ErrSettings
		}
		config = r
	}
	if config == nil {
		return Settings{}, ErrDisabled
	}
	if node == "" || config.Metadata.Node != node {
		return Settings{}, ErrNode
	}
	settings, err := Parse(config.ConfigFile.Content)
	if err != nil {
		return Settings{}, err
	}
	sum := sha256.Sum256([]byte(node + "\x00" + config.ConfigFile.Content))
	settings.Revision = hex.EncodeToString(sum[:])
	return settings, nil
}

func Parse(content string) (Settings, error) {
	var s Settings
	// yaml.v3 permits converting floats to integers. Refuse coercion before
	// decoding typed settings so fractional/overflow limits cannot be rounded.
	if len(content) > 65536 {
		return Settings{}, ErrSettings
	}
	var document yaml.Node
	if yaml.Unmarshal([]byte(content), &document) != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return Settings{}, ErrSettings
	}
	fields := document.Content[0].Content
	for i := 0; i < len(fields); i += 2 {
		switch fields[i].Value {
		case "quotaBytes", "maxFileBytes", "scratchRetentionDays", "trashRetentionDays":
			value := fields[i+1]
			if value.Tag != "!!int" {
				return Settings{}, ErrSettings
			}
			if _, err := strconv.ParseInt(value.Value, 10, 64); err != nil {
				return Settings{}, ErrSettings
			}
		}
	}
	decoder := yaml.NewDecoder(strings.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&s); err != nil {
		return Settings{}, ErrSettings
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Settings{}, ErrSettings
	}
	if err := s.Validate(); err != nil {
		return Settings{}, err
	}
	return s, nil
}

func (s Settings) Validate() error {
	if !safeRoot(s.Root) || s.QuotaBytes < 1 || s.QuotaBytes > MaxQuotaBytes || s.MaxFileBytes < 1 || s.MaxFileBytes > MaxFileBytes || s.MaxFileBytes > s.QuotaBytes || s.ScratchRetentionDays < 1 || s.ScratchRetentionDays > MaxRetentionDays || s.TrashRetentionDays < 1 || s.TrashRetentionDays > MaxRetentionDays {
		return ErrSettings
	}
	if s.RequiredMount != "" {
		if !safeRoot(s.RequiredMount) {
			return ErrSettings
		}
		rel, err := filepath.Rel(s.RequiredMount, s.Root)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ErrSettings
		}
	}
	return nil
}

func safeRoot(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && len(path) <= 4096 && strings.IndexFunc(path, unicode.IsControl) < 0
}
