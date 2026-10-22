package pkgtool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const imageSetPackage = `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: pairpkg, version: 1.0.0, capability: pairpkg, profile: default }
params:
  - { name: node, required: true }
imageSets:
  - name: server-and-db
    images: [example.com/app/server, example.com/app/db]
    reason: the server accepts only the extensions the db image supplies
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: pairpkg-server, node: "{{ .Params.node }}" }
    spec:
      image: "example.com/app/server@sha256:1111111111111111111111111111111111111111111111111111111111111111"
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: pairpkg-db, node: "{{ .Params.node }}" }
    spec:
      image: "example.com/app/db@sha256:2222222222222222222222222222222222222222222222222222222222222222"
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: pairpkg-cache, node: "{{ .Params.node }}" }
    spec:
      image: "example.com/app/cache@sha256:3333333333333333333333333333333333333333333333333333333333333333"
`

func readPackage(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "package.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPartialSetRefreshIsHeldBack is the photos-immich failure, made
// impossible to repeat silently. immich-server supports a fixed set of
// postgres vector extensions and its postgres image supplies one, so the
// two pins are not independent — but RefreshDigests resolved each by
// regex, and lint never pulls an image, so the tool most able to create
// the mismatch was the one least able to see it. The shipped pair had
// drifted apart and the package could not start at all.
func TestPartialSetRefreshIsHeldBack(t *testing.T) {
	dir := writePackage(t, imageSetPackage)

	// Only the server has moved. The db is already current.
	results, err := RefreshDigests(dir, func(image string) (string, error) {
		switch image {
		case "example.com/app/server":
			return "sha256:" + strings.Repeat("a", 64), nil
		case "example.com/app/db":
			return "sha256:" + strings.Repeat("2", 64), nil // unchanged
		default:
			return "sha256:" + strings.Repeat("c", 64), nil
		}
	})
	if err != nil {
		t.Fatalf("RefreshDigests: %v", err)
	}

	byImage := map[string]RefreshResult{}
	for _, r := range results {
		byImage[r.Image] = r
	}
	server := byImage["example.com/app/server"]
	if !server.HeldBack {
		t.Fatalf("server pin was not held back: %+v", server)
	}
	if !strings.Contains(server.Reason, "server-and-db") || !strings.Contains(server.Reason, "extensions") {
		t.Errorf("reason must name the set and carry the package's own explanation: %q", server.Reason)
	}

	// The point is the file, not the report: a held-back pin must not be
	// written, or the mismatch ships anyway.
	after := readPackage(t, dir)
	if strings.Contains(after, strings.Repeat("a", 64)) {
		t.Fatalf("held-back digest was written to the package")
	}

	// An image outside any set is unaffected — the rule constrains the
	// set, not the refresh.
	if !strings.Contains(after, strings.Repeat("c", 64)) {
		t.Errorf("unrelated image was not refreshed:\n%s", after)
	}
	if cache := byImage["example.com/app/cache"]; cache.HeldBack {
		t.Errorf("unrelated image was held back: %+v", cache)
	}

}

// When every member moves, the set is not a brake.
func TestWholeSetRefreshesTogether(t *testing.T) {
	dir := writePackage(t, imageSetPackage)
	results, err := RefreshDigests(dir, func(image string) (string, error) {
		switch image {
		case "example.com/app/server":
			return "sha256:" + strings.Repeat("a", 64), nil
		case "example.com/app/db":
			return "sha256:" + strings.Repeat("b", 64), nil
		default:
			return "sha256:" + strings.Repeat("c", 64), nil
		}
	})
	if err != nil {
		t.Fatalf("RefreshDigests: %v", err)
	}
	for _, r := range results {
		if r.HeldBack {
			t.Fatalf("%s held back though the whole set moved: %+v", r.Image, r)
		}
	}
	after := readPackage(t, dir)
	for _, want := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		if !strings.Contains(after, want) {
			t.Errorf("digest %s… not written:\n%s", want[:8], after)
		}
	}
}

// A member that fails to resolve holds its siblings back too: refreshing
// the half that answered is precisely the mismatch to avoid.
func TestResolveFailureHoldsTheSetBack(t *testing.T) {
	dir := writePackage(t, imageSetPackage)
	results, err := RefreshDigests(dir, func(image string) (string, error) {
		switch image {
		case "example.com/app/server":
			return "sha256:" + strings.Repeat("a", 64), nil
		case "example.com/app/db":
			return "", os.ErrDeadlineExceeded
		default:
			return "sha256:" + strings.Repeat("c", 64), nil
		}
	})
	if err != nil {
		t.Fatalf("RefreshDigests: %v", err)
	}
	for _, r := range results {
		if r.Image == "example.com/app/server" && !r.HeldBack {
			t.Fatalf("server refreshed while its sibling failed to resolve: %+v", r)
		}
	}
	if strings.Contains(readPackage(t, dir), strings.Repeat("a", 64)) {
		t.Fatal("digest written despite a sibling resolve failure")
	}
}
