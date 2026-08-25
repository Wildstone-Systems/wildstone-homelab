package pkgtool

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// imageRefRegexp finds digest-pinned image references inside a
// package.yaml so RefreshDigests can rewrite the digest half in place,
// preserving comments and formatting (the file is edited textually, not
// re-marshaled — packages are hand-authored documents).
var imageRefRegexp = regexp.MustCompile(`([a-zA-Z0-9][a-zA-Z0-9./_-]*(?::[a-zA-Z0-9._-]+)?)@(sha256:[a-f0-9]{64})`)

// DigestResolver returns the current manifest digest for an image
// reference like "ghcr.io/immich-app/immich-server" (registry inferred
// from the first path segment; Docker Hub when it has no dots). The
// production resolver speaks the registry HTTP API; tests inject a fake.
type DigestResolver func(imageRef string) (string, error)

// RefreshResult reports one image reference RefreshDigests examined.
type RefreshResult struct {
	Image     string
	OldDigest string
	NewDigest string // == OldDigest when already current
	Err       error
}

// RefreshDigests re-resolves every digest-pinned image in
// <dir>/package.yaml through resolve and rewrites changed pins in place —
// the packages/README gap ("no automated digest-refresh tooling yet"),
// closed (ADR-W010 C3). The updated package still goes through lint and
// the normal publish flow; this tool only edits the text.
func RefreshDigests(dir string, resolve DigestResolver) ([]RefreshResult, error) {
	path := filepath.Join(dir, "package.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var results []RefreshResult
	changed := false
	out := imageRefRegexp.ReplaceAllFunc(data, func(m []byte) []byte {
		sub := imageRefRegexp.FindSubmatch(m)
		image, oldDigest := string(sub[1]), string(sub[2])
		newDigest, err := resolve(image)
		res := RefreshResult{Image: image, OldDigest: oldDigest, NewDigest: newDigest, Err: err}
		results = append(results, res)
		if err != nil || newDigest == oldDigest {
			return m
		}
		changed = true
		return []byte(image + "@" + newDigest)
	})
	if changed {
		if err := os.WriteFile(path, out, 0o644); err != nil {
			return results, err
		}
	}
	return results, nil
}

// RegistryDigestResolver is the production DigestResolver: it speaks the
// OCI distribution API (anonymous token + manifest HEAD) against ghcr.io,
// Docker Hub, and compatible registries — the same flow the packages'
// digests were originally resolved with (packages/README).
func RegistryDigestResolver(client *http.Client) DigestResolver {
	if client == nil {
		client = fetchClient
	}
	return func(imageRef string) (string, error) {
		registry, repo := splitImageRef(imageRef)
		token, err := anonToken(client, registry, repo)
		if err != nil {
			return "", err
		}
		req, err := http.NewRequest("HEAD", fmt.Sprintf("https://%s/v2/%s/manifests/latest", registry, repo), nil)
		if err != nil {
			return "", err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("HEAD manifest %s/%s: %s", registry, repo, resp.Status)
		}
		digest := resp.Header.Get("Docker-Content-Digest")
		if digest == "" {
			return "", fmt.Errorf("HEAD manifest %s/%s: no Docker-Content-Digest header", registry, repo)
		}
		return digest, nil
	}
}

// splitImageRef separates registry host from repository path. A first
// segment with a dot is a registry host (ghcr.io/...); otherwise Docker
// Hub, with the library/ prefix for bare official images.
func splitImageRef(imageRef string) (registry, repo string) {
	parts := strings.SplitN(imageRef, "/", 2)
	if len(parts) == 2 && strings.Contains(parts[0], ".") {
		return parts[0], parts[1]
	}
	if !strings.Contains(imageRef, "/") {
		return "registry-1.docker.io", "library/" + imageRef
	}
	return "registry-1.docker.io", imageRef
}

// anonToken fetches an anonymous pull token where the registry requires
// one (Docker Hub, ghcr). Registries that serve manifests without a token
// simply skip this.
func anonToken(client *http.Client, registry, repo string) (string, error) {
	var authURL string
	switch registry {
	case "registry-1.docker.io":
		authURL = "https://auth.docker.io/token?service=registry.docker.io&scope=repository:" + repo + ":pull"
	case "ghcr.io":
		authURL = "https://ghcr.io/token?service=ghcr.io&scope=repository:" + repo + ":pull"
	default:
		return "", nil
	}
	resp, err := client.Get(authURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("token from %s: %w", registry, err)
	}
	return body.Token, nil
}
