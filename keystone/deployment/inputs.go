// Package deployment connects existing Owner Plans to local Git publication.
// Flux/Kubernetes, not this package, reconcile workloads. It has no cluster
// mutation credentials, arbitrary Helm values, shell execution or external Git
// push. Optional read-only observation uses a separately scoped identity.
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
	"regexp"
	"sort"
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
	SecretDelivery      *SecretDeliveryConfig   `json:"secretDelivery,omitempty"`
	Observation         *ObservationConfig      `json:"observation,omitempty"`
}

// SecretDeliveryConfig is fixed privileged bootstrap inventory. Product/API
// input cannot select these fields. The first implementation deliberately has
// one filename, one restart behavior and one outer Flux decryption binding.
type SecretDeliveryConfig struct {
	Generation             uint64 `json:"generation"`
	OutputPath             string `json:"outputPath"`
	KustomizationNamespace string `json:"kustomizationNamespace"`
	KustomizationName      string `json:"kustomizationName"`
	DecryptionSecretName   string `json:"decryptionSecretName"`
	RestartPolicy          string `json:"restartPolicy"`
}

type encryptedSecretInfo struct {
	secretType            string
	recipientFingerprints []string
	sopsVersion           string
	encryptedRegex        string
	managedShape          bool
}

type inputs struct {
	config                                Config
	pkg                                   *pkgspec.Package
	inventory, profile, secret, placement string
	secretBytes                           []byte
	secretInfo                            encryptedSecretInfo
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
	if c.Instance == "" || c.Instance != c.Bindings.Name || c.Bindings.SecretRevision != "" || c.Bindings.SecretFile != "" || c.Bindings.SecretGeneration != 0 || c.Bindings.SecretRestartPolicy != "" || c.Repository == c.MirrorRepository {
		return out, fmt.Errorf("deployment: explicit isolated instance inventory required")
	}
	if c.SecretDelivery != nil {
		s := c.SecretDelivery
		name := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
		if s.Generation == 0 || s.OutputPath != "secret.sops.yaml" || s.RestartPolicy != "recreate-on-ciphertext-revision" || !name.MatchString(s.KustomizationNamespace) || s.KustomizationNamespace == "default" || strings.HasPrefix(s.KustomizationNamespace, "kube-") || s.KustomizationNamespace == c.Bindings.Namespace || !name.MatchString(s.KustomizationName) || !name.MatchString(s.DecryptionSecretName) {
			return out, fmt.Errorf("deployment: exact encrypted Secret delivery inventory required")
		}
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
	info, err := inspectEncryptedSecret(secret, c.Bindings)
	if err != nil {
		return out, err
	}
	if c.SecretDelivery != nil && (len(info.recipientFingerprints) == 0 || info.sopsVersion != "3.13.3" || info.encryptedRegex != "^(data|stringData)$" || !info.managedShape) {
		return out, fmt.Errorf("deployment: exact SOPS/age Secret document required")
	}
	out.inventory = digest(raw)
	out.profile = digest(profile)
	out.secret = digest(secret)
	out.secretBytes = append([]byte(nil), secret...)
	out.secretInfo = info
	b := c.Bindings
	b.SecretRevision = ""
	delivery := c.SecretDelivery
	if delivery != nil {
		copy := *delivery
		copy.Generation = 0
		delivery = &copy
	}
	out.placement = digest(encoded(struct {
		Instance, Repository, Mirror string
		Bindings                     capability.HelmBindings
		SecretDelivery               *SecretDeliveryConfig
	}{c.Instance, c.Repository, c.MirrorRepository, b, delivery}))
	// Validate every namespace/name/claim and the supported mapping now.
	b.SecretRevision = out.secret
	if c.SecretDelivery != nil {
		b.SecretFile = c.SecretDelivery.OutputPath
		b.SecretGeneration = c.SecretDelivery.Generation
		b.SecretRestartPolicy = c.SecretDelivery.RestartPolicy
	}
	if _, err := (&capability.Engine{}).ProjectHelm(out.pkg, map[string]string{"timezone": "UTC"}, b, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		return out, fmt.Errorf("deployment: invalid fixed instance/profile bindings")
	}
	return out, nil
}

func encryptedSecret(raw []byte, b capability.HelmBindings) error {
	_, err := inspectEncryptedSecret(raw, b)
	return err
}

func inspectEncryptedSecret(raw []byte, b capability.HelmBindings) (encryptedSecretInfo, error) {
	var info encryptedSecretInfo
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return info, fmt.Errorf("deployment: encrypted Secret configuration required")
	}
	for k := range doc {
		switch k {
		case "apiVersion", "kind", "metadata", "type", "data", "stringData", "sops":
		default:
			return info, fmt.Errorf("deployment: unsupported encrypted Secret document")
		}
	}
	metadata, _ := doc["metadata"].(map[string]any)
	sops, _ := doc["sops"].(map[string]any)
	encrypted := func(v any) bool {
		s, ok := v.(string)
		return ok && len(s) > 5 && bytes.HasPrefix([]byte(s), []byte("ENC[")) && s[len(s)-1] == ']'
	}
	if doc["apiVersion"] != "v1" || doc["kind"] != "Secret" || metadata["name"] != b.SecretName || metadata["namespace"] != b.Namespace || !encrypted(sops["mac"]) {
		return info, fmt.Errorf("deployment: exact encrypted Secret reference required")
	}
	for key := range metadata {
		if key != "name" && key != "namespace" {
			return info, fmt.Errorf("deployment: unsupported encrypted Secret metadata")
		}
	}
	info.secretType = "Opaque"
	if doc["type"] != nil {
		if doc["type"] != "Opaque" {
			return info, fmt.Errorf("deployment: unsupported encrypted Secret type")
		}
		info.secretType = doc["type"].(string)
	}
	count := 0
	for _, field := range []string{"data", "stringData"} {
		if doc[field] == nil {
			continue
		}
		values, ok := doc[field].(map[string]any)
		if !ok {
			return info, fmt.Errorf("deployment: encrypted Secret payload required")
		}
		for k, v := range values {
			if k != "jwt-secret" || !encrypted(v) {
				return info, fmt.Errorf("deployment: plaintext or unsupported Secret payload refused")
			}
			count++
		}
	}
	if count != 1 {
		return info, fmt.Errorf("deployment: one encrypted jwt-secret reference required")
	}

	allowedSOPS := map[string]bool{"age": true, "lastmodified": true, "mac": true, "encrypted_regex": true, "version": true}
	info.managedShape = true
	for key := range sops {
		if !allowedSOPS[key] {
			info.managedShape = false
		}
	}
	info.sopsVersion, _ = sops["version"].(string)
	info.encryptedRegex, _ = sops["encrypted_regex"].(string)
	if modified, ok := sops["lastmodified"].(string); !ok || modified == "" {
		info.managedShape = false
	}
	age, ok := sops["age"].([]any)
	if !ok || len(age) == 0 {
		info.managedShape = false
	} else {
		seen := map[string]bool{}
		ageRecipient := regexp.MustCompile(`^age1[0-9a-z]{20,100}$`)
		for _, rawEntry := range age {
			entry, ok := rawEntry.(map[string]any)
			if !ok || len(entry) != 2 {
				info.managedShape = false
				continue
			}
			recipient, rok := entry["recipient"].(string)
			wrapped, wok := entry["enc"].(string)
			if !rok || !wok || recipient != strings.TrimSpace(recipient) || recipient != strings.ToLower(recipient) || !ageRecipient.MatchString(recipient) || !strings.Contains(wrapped, "-----BEGIN AGE ENCRYPTED FILE-----") || !strings.Contains(wrapped, "-----END AGE ENCRYPTED FILE-----") || seen[recipient] {
				info.managedShape = false
				continue
			}
			seen[recipient] = true
			sum := sha256.Sum256([]byte(recipient))
			info.recipientFingerprints = append(info.recipientFingerprints, "sha256:"+hex.EncodeToString(sum[:]))
		}
	}
	sort.Strings(info.recipientFingerprints)
	return info, nil
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
	if s.config.SecretDelivery != nil {
		b.SecretFile = s.config.SecretDelivery.OutputPath
		b.SecretGeneration = s.config.SecretDelivery.Generation
		b.SecretRestartPolicy = s.config.SecretDelivery.RestartPolicy
	}
	return (&capability.Engine{}).ProjectHelm(s.pkg, map[string]string{"timezone": spec.Timezone}, b, revision)
}

func (s inputs) privateFiles() map[string][]byte {
	if s.config.SecretDelivery == nil {
		return nil
	}
	return map[string][]byte{s.config.SecretDelivery.OutputPath: append([]byte(nil), s.secretBytes...)}
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
