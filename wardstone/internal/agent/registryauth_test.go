package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

const testDigest = "@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// Which registry a reference names is Docker's rule, not a guess: the
// first component is a host only if it looks like one. Reading
// "library/redis" as a registry called "library" would send that
// registry's credential nowhere useful — or, worse, send another
// registry's credential to a host the operator never authorized.
func TestRegistryHostOf(t *testing.T) {
	for image, want := range map[string]string{
		"ghcr.io/you/chat-adapter" + testDigest:      "ghcr.io",
		"registry.example.com:5000/x/y" + testDigest: "registry.example.com:5000",
		"localhost:5000/x" + testDigest:              "localhost:5000",
		"localhost/x" + testDigest:                   "localhost",
		"library/redis" + testDigest:                 "docker.io",
		"redis" + testDigest:                         "docker.io",
		"docker.io/library/redis" + testDigest:       "docker.io",
	} {
		if got := registryHostOf(image); got != want {
			t.Fatalf("registryHostOf(%q) = %q, want %q", image, got, want)
		}
	}
}

// A host's credential lives in a secret named after it, with the port
// separator flattened — a secret name is a file name and may not carry a
// colon.
func TestRegistrySecretName(t *testing.T) {
	for host, want := range map[string]string{
		"ghcr.io":                   "registry.ghcr.io",
		"registry.example.com:5000": "registry.registry.example.com-5000",
		"docker.io":                 "registry.docker.io",
	} {
		if got := registrySecretName(host); got != want {
			t.Fatalf("registrySecretName(%q) = %q, want %q", host, got, want)
		}
		if oerr := validSecretName(want); oerr != nil {
			t.Fatalf("%q is not a storable secret name: %v", want, oerr.GetMessage())
		}
	}
}

// The credential a node holds is used for the registry it names, and for
// no other. This is the property the whole file exists to protect: a
// package naming an image on another host must not cause ghcr.io's token
// to be sent there.
func TestRegistryCredentialIsScopedToItsRegistry(t *testing.T) {
	a := storeOnlyAgent(t)
	putSecret(t, a, "registry.ghcr.io", "mika:ghp_exampletoken")

	auth, err := a.registryAuthFor("ghcr.io/you/chat-adapter" + testDigest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.URLEncoding.DecodeString(auth)
	if err != nil {
		t.Fatalf("header is not base64url: %v", err)
	}
	var got registryAuth
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Username != "mika" || got.Password != "ghp_exampletoken" || got.ServerAddress != "ghcr.io" {
		t.Fatalf("auth = %+v", got)
	}

	// Another registry, same node: no credential, so an anonymous pull.
	for _, other := range []string{
		"registry.example.com/x/y" + testDigest,
		"library/redis" + testDigest,
		"ghcr.io.evil.example.com/x" + testDigest,
	} {
		auth, err := a.registryAuthFor(other)
		if err != nil {
			t.Fatal(err)
		}
		if auth != "" {
			t.Fatalf("%s got a credential meant for ghcr.io", other)
		}
	}
}

// A credential that exists and cannot be used is an error, not a silent
// anonymous pull: against a private registry that would come back as
// "not found", and send the operator looking for the wrong problem.
func TestUnusableRegistryCredentialIsAnError(t *testing.T) {
	a := storeOnlyAgent(t)
	putSecret(t, a, "registry.ghcr.io", "just-a-token-with-no-username")

	_, err := a.registryAuthFor("ghcr.io/you/x" + testDigest)
	if err == nil {
		t.Fatal("a malformed credential was accepted")
	}
	if !strings.Contains(err.Error(), "username:password") {
		t.Fatalf("error = %v, want it to say what the value should look like", err)
	}
	// And it must not quote the value back.
	if strings.Contains(err.Error(), "just-a-token") {
		t.Fatalf("the error leaked the credential: %v", err)
	}
}

// End to end through the op: the header reaches Docker on a pull, and
// carries nothing when the node holds no credential for that registry.
func TestPullSendsTheRegistryHeader(t *testing.T) {
	fake := newFakeDocker(t)
	a := storeOnlyAgent(t)
	dc, err := newDockerClient(fake.URL())
	if err != nil {
		t.Fatal(err)
	}
	a.docker = dc
	putSecret(t, a, "registry.ghcr.io", "mika:ghp_exampletoken")

	if _, oerr := a.pullImage(context.Background(), &wsv1.PullImage{
		Image: "ghcr.io/you/chat-adapter" + testDigest,
	}); oerr != nil {
		t.Fatalf("pull: %s", oerr.GetMessage())
	}
	fake.mu.Lock()
	authed := fake.pullAuth
	fake.mu.Unlock()
	if authed == "" {
		t.Fatal("the pull carried no X-Registry-Auth")
	}

	if _, oerr := a.pullImage(context.Background(), &wsv1.PullImage{
		Image: "registry.example.com/x/y" + testDigest,
	}); oerr != nil {
		t.Fatalf("pull: %s", oerr.GetMessage())
	}
	fake.mu.Lock()
	anon := fake.pullAuth
	fake.mu.Unlock()
	if anon != "" {
		t.Fatalf("an unrelated registry got a credential: %q", anon)
	}
}
