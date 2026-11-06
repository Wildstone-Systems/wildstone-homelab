package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"gopkg.in/yaml.v3"
)

// HelmBindings references existing, separately provisioned resources. Projection
// neither creates claims/secrets nor adopts a live release.
type HelmBindings struct {
	Name           string
	Namespace      string
	ServiceAccount string
	ConfigClaim    string
	MetadataClaim  string
	MediaClaim     string
	SecretName     string
	// SecretRevision identifies the encrypted configuration revision, not a
	// plaintext secret hash. Publication must verify it before dispatch.
	SecretRevision string `json:",omitempty"`
	// SecretFile enables the separately owned encrypted Secret projection.
	// Only one fixed relative filename and deterministic restart policy are
	// supported; the ciphertext bytes are never passed to this package.
	SecretFile          string `json:",omitempty"`
	SecretGeneration    uint64 `json:",omitempty"`
	SecretRestartPolicy string `json:",omitempty"`
}

// HelmProjection is review material, not approval or a deployable operation.
type HelmProjection struct {
	Files            map[string][]byte
	Digest           string
	AcceptedRevision string
	PackageVersion   string
}

var kubeName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
var fullRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)
var secretRevision = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ProjectHelm is a pure offline projection for the first reviewed chart profile.
// It never contacts Git/Kubernetes, loads a secret or authorizes an operation.
func (e *Engine) ProjectHelm(pkg *pkgspec.Package, params map[string]string, b HelmBindings, acceptedRevision string) (*HelmProjection, error) {
	if pkg == nil || pkg.Helm == nil {
		return nil, fmt.Errorf("capability: missing Helm profile")
	}
	if err := pkg.Validate(); err != nil {
		return nil, err
	}
	if !fullRevision.MatchString(acceptedRevision) {
		return nil, fmt.Errorf("capability: accepted revision must be a full commit")
	}
	if !secretRevision.MatchString(b.SecretRevision) {
		return nil, fmt.Errorf("capability: encrypted secret revision must be a full content digest")
	}
	managedSecret := b.SecretFile != ""
	if managedSecret {
		if b.SecretFile != "secret.sops.yaml" || b.SecretGeneration == 0 || b.SecretRestartPolicy != "recreate-on-ciphertext-revision" {
			return nil, fmt.Errorf("capability: unsupported encrypted secret projection")
		}
	} else if b.SecretGeneration != 0 || b.SecretRestartPolicy != "" {
		return nil, fmt.Errorf("capability: incomplete encrypted secret projection")
	}
	for field, value := range map[string]string{"name": b.Name, "namespace": b.Namespace, "serviceAccount": b.ServiceAccount, "configClaim": b.ConfigClaim, "metadataClaim": b.MetadataClaim, "mediaClaim": b.MediaClaim, "secretName": b.SecretName} {
		if !kubeName.MatchString(value) {
			return nil, fmt.Errorf("capability: invalid %s", field)
		}
	}
	if b.Namespace == "default" || strings.HasPrefix(b.Namespace, "kube-") || strings.HasPrefix(b.Namespace, "wildstone-") || b.Namespace == "flux-system" {
		return nil, fmt.Errorf("capability: application namespace must be separate from platform namespaces")
	}
	if b.ConfigClaim == b.MetadataClaim || b.ConfigClaim == b.MediaClaim || b.MetadataClaim == b.MediaClaim {
		return nil, fmt.Errorf("capability: storage roles must have distinct claims")
	}
	// Only timezone is an input in this first profile. No user-supplied raw
	// Helm values, chart source, environment, hooks or post-renderers.
	if len(pkg.Params) != 1 || pkg.Params[0].Name != "timezone" || pkg.Params[0].Validate != "iana_timezone" {
		return nil, fmt.Errorf("capability: unsupported chart parameter declaration")
	}
	merged, err := mergeParams(pkg, params)
	if err != nil {
		return nil, err
	}
	h := pkg.Helm
	image := strings.SplitN(h.Image, ":", 2)
	values := map[string]any{
		"fullnameOverride": b.Name, "replicaCount": 1,
		"strategy":                 map[string]any{"type": "Recreate"},
		"image":                    map[string]any{"repository": image[0], "tag": image[1]},
		"permissionsInitContainer": map[string]any{"enabled": false},
		"podSecurityContext":       map[string]any{"fsGroup": 1000, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"securityContext":          map[string]any{"runAsUser": 1000, "runAsGroup": 1000, "runAsNonRoot": true, "allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}},
		"env":                      map[string]any{"TZ": merged["timezone"], "HOST": "0.0.0.0", "PORT": "13378", "CONFIG_PATH": "/config", "METADATA_PATH": "/metadata", "BACKUP_PATH": "/metadata/backups", "JWT_SECRET": map[string]any{"valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": b.SecretName, "key": "jwt-secret"}}}},
		"ports":                    []any{map[string]any{"name": "http", "containerPort": 13378, "protocol": "TCP"}},
		"service":                  map[string]any{"type": "ClusterIP", "port": 13378, "targetPort": "http"},
		"ingress":                  map[string]any{"enabled": false},
		"persistence":              map[string]any{"config": map[string]any{"enabled": true, "existingClaim": b.ConfigClaim}, "metadata": map[string]any{"enabled": true, "existingClaim": b.MetadataClaim}},
		"mediaMounts":              []any{map[string]any{"name": "audiobooks", "existingClaim": b.MediaClaim, "mountPath": "/audiobooks", "readOnly": false}},
		"resources":                map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "256Mi"}, "limits": map[string]any{"cpu": "1", "memory": "1Gi"}},
	}
	if managedSecret {
		values["podAnnotations"] = map[string]any{"wildstone.dev/encrypted-secret-revision": b.SecretRevision}
	}
	for _, name := range []string{"startupProbe", "readinessProbe", "livenessProbe"} {
		probe := map[string]any{"httpGet": map[string]any{"path": "/healthcheck", "port": "http"}, "periodSeconds": 10}
		if name == "startupProbe" {
			probe["failureThreshold"] = 30
			probe["periodSeconds"] = 5
		}
		values[name] = probe
	}
	metadata := map[string]any{"name": b.Name, "namespace": b.Namespace, "labels": map[string]any{"app.kubernetes.io/part-of": "wildstone"}}
	source := map[string]any{"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "GitRepository", "metadata": metadata, "spec": map[string]any{"interval": "10m", "url": h.SourceURL, "ref": map[string]any{"commit": h.SourceCommit}}}
	release := map[string]any{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease", "metadata": metadata, "spec": map[string]any{
		"interval": "10m", "timeout": "10m", "releaseName": b.Name, "serviceAccountName": b.ServiceAccount,
		"chart":   map[string]any{"spec": map[string]any{"chart": h.ChartPath, "version": h.ChartVersion, "reconcileStrategy": "Revision", "sourceRef": map[string]any{"kind": "GitRepository", "name": b.Name}}},
		"install": map[string]any{"remediation": map[string]any{"retries": 0}}, "upgrade": map[string]any{"remediation": map[string]any{"retries": 0}}, "values": values}}
	resources := []string{"source.yaml", "release.yaml"}
	if managedSecret {
		resources = append(resources, b.SecretFile)
	}
	kustomization := map[string]any{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": resources}
	files := map[string][]byte{}
	for name, obj := range map[string]any{"source.yaml": source, "release.yaml": release, "kustomization.yaml": kustomization} {
		data, err := yaml.Marshal(obj)
		if err != nil {
			return nil, err
		}
		files[name] = data
	}
	// JSON canonicalizes map keys and binds the complete package declaration,
	// including metadata/facets, source revision, bindings and exact output.
	review, err := json.Marshal(struct {
		Revision string
		Package  *pkgspec.Package
		Bindings HelmBindings
		Files    map[string][]byte
	}{acceptedRevision, pkg, b, files})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(review)
	return &HelmProjection{Files: files, Digest: hex.EncodeToString(digest[:]), AcceptedRevision: acceptedRevision, PackageVersion: pkg.Metadata.Version}, nil
}
