// Package deployment connects existing Owner Plans to local Git publication.
// Flux/Kubernetes, not this package, reconcile workloads. It has no cluster
// credentials, arbitrary Helm values, shell execution or external Git push.
package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/localgit"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"gopkg.in/yaml.v3"
)

// Config is local privileged bootstrap inventory, not editable application
// desired state. The first integration has one pre-provisioned isolated slot.
// Paths/claims cannot be supplied by a proposal or an Agent.
type Config struct {
	Instance            string                  `json:"instance"`
	ProfileFile         string                  `json:"profileFile"`
	EncryptedSecretFile string                  `json:"encryptedSecretFile"`
	Repository          string                  `json:"repository"`
	MirrorRepository    string                  `json:"mirrorRepository"`
	Bindings            capability.HelmBindings `json:"bindings"`
}

type inputs struct {
	config                                Config
	pkg                                   *pkgspec.Package
	inventory, profile, secret, placement string
}

func digest(data []byte) string { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }
func encoded(v any) []byte      { b, _ := json.Marshal(v); return b }

func localFile(path string, private bool) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("deployment: absolute bootstrap paths required")
	}
	if private {
		if err := localgit.CheckLocalPath(filepath.Dir(path)); err != nil {
			return nil, fmt.Errorf("deployment: configuration must be outside source checkouts")
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 || (private && info.Mode().Perm()&0077 != 0) {
		return nil, fmt.Errorf("deployment: private bootstrap material unavailable")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("deployment: bootstrap material unavailable")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return nil, fmt.Errorf("deployment: bootstrap material unavailable")
	}
	return b, nil
}

func load(path string) (inputs, error) {
	var out inputs
	raw, err := localFile(path, true)
	if err != nil {
		return out, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&out.config); err != nil {
		return out, fmt.Errorf("deployment: invalid bootstrap inventory")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return out, fmt.Errorf("deployment: invalid bootstrap inventory")
	}
	c := out.config
	if c.Instance == "" || c.Instance != c.Bindings.Name || c.Bindings.SecretRevision != "" || c.Repository == c.MirrorRepository {
		return out, fmt.Errorf("deployment: explicit isolated instance inventory required")
	}
	for _, p := range []string{c.Repository, c.MirrorRepository} {
		if err := localgit.CheckLocalPath(p); err != nil {
			return out, fmt.Errorf("deployment: invalid local repository path")
		}
	}
	for _, pair := range [][2]string{{c.Repository, c.MirrorRepository}, {c.MirrorRepository, c.Repository}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err != nil || relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return out, fmt.Errorf("deployment: separate non-nested repositories required")
		}
	}
	profile, err := localFile(c.ProfileFile, false)
	if err != nil {
		return out, err
	}
	// Load validates the existing closed Package schema, including no templates,
	// hooks, floating artifacts or arbitrary values in this reviewed profile.
	out.pkg, err = pkgspec.Load(c.ProfileFile)
	if err != nil || out.pkg.Helm == nil {
		return out, fmt.Errorf("deployment: reviewed Helm profile unavailable")
	}
	// Refuse a changed file between the bounded read and Package parser.
	again, err := localFile(c.ProfileFile, false)
	if err != nil || !bytes.Equal(profile, again) {
		return out, fmt.Errorf("deployment: profile changed while reading")
	}
	secret, err := localFile(c.EncryptedSecretFile, true)
	if err != nil {
		return out, err
	}
	if err := encryptedSecret(secret, c.Bindings); err != nil {
		return out, err
	}
	out.inventory = digest(raw)
	out.profile = digest(profile)
	out.secret = digest(secret)
	b := c.Bindings
	b.SecretRevision = ""
	out.placement = digest(encoded(struct {
		Instance, Repository, Mirror string
		Bindings                     capability.HelmBindings
	}{c.Instance, c.Repository, c.MirrorRepository, b}))
	// Validate every namespace/name/claim and the supported mapping now.
	b.SecretRevision = out.secret
	if _, err := (&capability.Engine{}).ProjectHelm(out.pkg, map[string]string{"timezone": "UTC"}, b, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		return out, fmt.Errorf("deployment: invalid fixed instance/profile bindings")
	}
	return out, nil
}

func encryptedSecret(raw []byte, b capability.HelmBindings) error {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("deployment: encrypted Secret configuration required")
	}
	for k := range doc {
		switch k {
		case "apiVersion", "kind", "metadata", "type", "data", "stringData", "sops":
		default:
			return fmt.Errorf("deployment: unsupported encrypted Secret document")
		}
	}
	metadata, _ := doc["metadata"].(map[string]any)
	sops, _ := doc["sops"].(map[string]any)
	encrypted := func(v any) bool {
		s, ok := v.(string)
		return ok && len(s) > 5 && bytes.HasPrefix([]byte(s), []byte("ENC[")) && s[len(s)-1] == ']'
	}
	if doc["apiVersion"] != "v1" || doc["kind"] != "Secret" || metadata["name"] != b.SecretName || metadata["namespace"] != b.Namespace || !encrypted(sops["mac"]) {
		return fmt.Errorf("deployment: exact encrypted Secret reference required")
	}
	count := 0
	for _, field := range []string{"data", "stringData"} {
		if doc[field] == nil {
			continue
		}
		values, ok := doc[field].(map[string]any)
		if !ok {
			return fmt.Errorf("deployment: encrypted Secret payload required")
		}
		for k, v := range values {
			if k != "jwt-secret" || !encrypted(v) {
				return fmt.Errorf("deployment: plaintext or unsupported Secret payload refused")
			}
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("deployment: one encrypted jwt-secret reference required")
	}
	return nil
}

func (s inputs) project(want resource.Resource, revision string) (*capability.HelmProjection, error) {
	if err := want.Validate(); err != nil {
		return nil, err
	}
	spec := want.HelmDeployment
	if want.Metadata.Name != s.config.Instance || spec == nil || spec.Placement != s.placement || spec.Package != s.pkg.Metadata.Name || spec.Version != s.pkg.Metadata.Version {
		return nil, fmt.Errorf("deployment: instance/profile/placement changed; fresh review required")
	}
	b := s.config.Bindings
	b.SecretRevision = s.secret
	return (&capability.Engine{}).ProjectHelm(s.pkg, map[string]string{"timezone": spec.Timezone}, b, revision)
}

func (c Config) repoIdentity() string { return digest([]byte(filepath.Clean(c.Repository))) }

// checkFiles performs bounded reads/hash comparison only. It is safe inside
// the final SQLite authority fence: no parse/render, Git, DB or network calls.
func (s inputs) checkFiles(path string) error {
	for _, item := range []struct {
		path, expected string
		private        bool
	}{{path, s.inventory, true}, {s.config.ProfileFile, s.profile, false}, {s.config.EncryptedSecretFile, s.secret, true}} {
		raw, err := localFile(item.path, item.private)
		if err != nil || digest(raw) != item.expected {
			return fmt.Errorf("deployment: local input version changed")
		}
	}
	return nil
}
