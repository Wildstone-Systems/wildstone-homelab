package pkgtool

import (
	"strings"
	"testing"
)

const containerEnvRefPackage = `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: envpkg, version: 1.0.0, capability: envpkg, profile: default }
params:
  - { name: node, required: true }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: envpkg-app, node: "{{ .Params.node }}" }
    spec:
      image: "docker.io/library/nginx@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
      env:
        DB_HOSTNAME: "container/envpkg-postgres"
        CACHE_URL: "redis://container/envpkg-redis:6379"
        PLAIN_HOST: envpkg-postgres
        A_SECRET: "secret/envpkg-db-password"
`

// TestLintRejectsContainerRefInEnv pins the defect two shipped packages
// carried in six places, neither of which could have started.
//
// "container/<name>:<port>" is real syntax in Route.upstream, where the
// reconciler resolves it. Nothing resolves it in env — the agent
// substitutes secret/ and passes everything else through byte for byte —
// so immich-server received DB_HOSTNAME=container/immich-postgres and
// node-postgres read the "/" as a UNIX socket path. Lint never pulled an
// image, and the only reader of the value was the process that died.
func TestLintRejectsContainerRefInEnv(t *testing.T) {
	dir := writePackage(t, containerEnvRefPackage)
	issues, err := Lint(dir, LintOptions{})
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	var flagged []string
	for _, i := range issues {
		if i.Rule == "container-env-ref" {
			flagged = append(flagged, i.Msg)
		}
	}
	if len(flagged) != 2 {
		t.Fatalf("want 2 container-env-ref issues (bare and embedded-in-URL), got %d: %v", len(flagged), flagged)
	}
	// The embedded case is the one a naive prefix check misses, and three
	// of the six real occurrences were of that shape.
	joined := strings.Join(flagged, "\n")
	for _, want := range []string{"DB_HOSTNAME", "CACHE_URL"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no issue for %s: %v", want, flagged)
		}
	}
	// A plain container name is the fix, and a secret/ reference is the
	// one prefix env legitimately carries. Neither may be flagged.
	for _, mustNot := range []string{"PLAIN_HOST", "A_SECRET"} {
		if strings.Contains(joined, mustNot) {
			t.Errorf("%s was flagged, but it is correct: %v", mustNot, flagged)
		}
	}
	// The message has to say what to write instead: this fires at package
	// authoring time, far from the container log that would otherwise be
	// the only evidence.
	if !strings.Contains(joined, "plain container name") {
		t.Errorf("message does not name the fix: %v", flagged)
	}
}
