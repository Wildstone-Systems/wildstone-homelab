package agent

// Pulling from a private registry (F94).
//
// Docker's image pull takes credentials in an X-Registry-Auth header, and
// this agent sent none — so every image Wildstone runs had to be
// anonymously pullable. That is not a small limitation: a package's image
// must be digest-pinned and is re-pulled before every container start, so
// a private repository failed the step, and `docker load`ing the image
// onto the node by hand did not help either (the pull still runs).
//
// The credential is a NODE SECRET, resolved here and never anywhere else.
// That is the whole design:
//
//   - Keystone cannot read a secret back (ADR-W012), so it cannot send
//     one down with the op even if it wanted to. The value exists on the
//     node, sealed, and is opened for the length of one HTTP request.
//   - Nothing about the pull crosses the wire differently. There is no
//     protocol change, no new op, and no new field for an operator to
//     get wrong.
//
// The secret is named after the registry it is for — `registry.ghcr.io`
// for images from ghcr.io — and a credential is used ONLY for the
// registry its name matches. Sending ghcr.io's token to some other host
// because a package named an image oddly is the failure this file is
// most careful to avoid, so the host is derived from the reference by
// the same rule Docker itself uses, and matched exactly.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// registrySecretPrefix names the node secret holding one registry's
// credential. The rest of the name is the registry host with its port
// separator rewritten, because a secret name is used verbatim as a file
// name and may not contain a colon:
//
//	ghcr.io                   -> registry.ghcr.io
//	registry.example.com:5000 -> registry.registry.example.com_5000
//	(no host in the ref)      -> registry.docker.io
//
// The rewrite is ':' -> '_' rather than ':' -> '-' precisely because it
// must be REVERSIBLE. A hyphen is legal in a hostname, so flattening to
// one made `example.com:5000` and the entirely different host
// `example.com-5000` the same secret — and a package pulling from the
// second would have been handed the credential for the first. An
// underscore cannot appear in a registry host at all (validRegistryHost
// enforces it), so no two hosts can produce one name.
const registrySecretPrefix = "registry."

// defaultRegistryHost is where a reference with no host comes from, the
// same default Docker applies.
const defaultRegistryHost = "docker.io"

// registryAuth is Docker's AuthConfig, in the one shape this agent
// sends: a username and password for one server. No identity token, no
// email — fields a credential helper fills in, which this is not.
type registryAuth struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	ServerAddress string `json:"serveraddress"`
}

// validRegistryHost is an RFC 1123 host with an optional port. It is the
// invariant the secret naming rests on: no underscore, so ':' -> '_' is
// injective, and no separator that could reach outside the secrets
// directory.
var validRegistryHost = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(:[0-9]{1,5})?$`)

// registryHostOf extracts the registry host from an image reference.
//
// Docker's rule, and it is subtler than "split on the first slash": the
// first path component is a HOST only if it looks like one — it contains
// a dot or a colon, or it is exactly "localhost". Otherwise the whole
// reference is a Docker Hub repository, and `library/redis@sha256:…` must
// not be read as a registry called "library".
func registryHostOf(image string) string {
	first, rest, ok := strings.Cut(image, "/")
	if !ok {
		return defaultRegistryHost
	}
	_ = rest
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return first
	}
	return defaultRegistryHost
}

// registrySecretName is the node secret a given host's credential lives
// in, or "" for a host this agent will not look one up for.
//
// Refusing is the safe direction: no name means no credential means an
// anonymous pull, which fails visibly at the registry rather than
// sending somebody's token somewhere it was not meant to go.
func registrySecretName(host string) string {
	if !validRegistryHost.MatchString(host) {
		return ""
	}
	name := registrySecretPrefix + strings.ReplaceAll(host, ":", "_")
	// A name the store could not hold cannot name a credential either —
	// the operator was never able to set one under it.
	if validSecretName(name) != nil {
		return ""
	}
	return name
}

// registryAuthFor returns the X-Registry-Auth header value for pulling
// image, or "" when this node holds no credential for that registry —
// which is the ordinary case, and stays an anonymous pull.
//
// A credential that EXISTS but cannot be read or parsed is an error
// rather than a silent fall back to anonymous: the operator set it
// because the registry needs it, and an anonymous pull against a private
// registry fails as "not found", which sends them looking for the wrong
// problem.
func (a *Agent) registryAuthFor(image string) (string, error) {
	host := registryHostOf(image)
	name := registrySecretName(host)
	if name == "" || !a.secretExists(name) {
		return "", nil
	}
	value, err := a.openSecret(name)
	if err != nil {
		return "", fmt.Errorf("registry credential %s: %w", name, err)
	}
	user, pass, ok := strings.Cut(strings.TrimSpace(value), ":")
	if !ok || user == "" || pass == "" {
		// Deliberately says nothing about the value itself.
		return "", fmt.Errorf("registry credential %s is not username:password", name)
	}
	encoded, err := json.Marshal(registryAuth{Username: user, Password: pass, ServerAddress: host})
	if err != nil {
		return "", fmt.Errorf("registry credential %s: %w", name, err)
	}
	// Docker reads this header as base64url.
	return base64.URLEncoding.EncodeToString(encoded), nil
}
