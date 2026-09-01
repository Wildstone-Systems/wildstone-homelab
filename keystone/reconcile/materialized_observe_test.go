package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// TestMaterializedFileComparesBySourceHash is the trap this mechanism
// exists to avoid, and it is silent in cause: the agent resolves secret
// references when it writes the Authelia clients file, so the bytes on
// disk are not the bytes Keystone rendered. Comparing rendered content
// against them never matches, which would make the reconciler rewrite
// the file on every tick forever and report drift it had just repaired.
//
// Keystone cannot compute the expected content — it holds no secret
// values, by design — so the pre-materialization hash is the only thing
// it can compare, and the agent sends that instead of the content.
func TestMaterializedFileComparesBySourceHash(t *testing.T) {
	want := []byte("client_secret: 'pbkdf2:secret/immich-oidc'\n")
	sum := sha256.Sum256(want)

	materialized := &wsv1.ReadManagedFileResult{SourceSha256Hex: hex.EncodeToString(sum[:])}
	if !managedFileMatches(materialized, want) {
		t.Error("a converged materialized file was reported as drifted")
	}

	// A source hash that does not match is genuine drift.
	other := sha256.Sum256([]byte("something else"))
	stale := &wsv1.ReadManagedFileResult{SourceSha256Hex: hex.EncodeToString(other[:])}
	if managedFileMatches(stale, want) {
		t.Error("a drifted materialized file was reported as converged")
	}
}

// The content field of a materialized read is empty on purpose — the
// bytes hold live secrets. A comparison that fell back to bytes when the
// hash matched, or that treated empty content as "gone", would either
// leak or thrash.
func TestEmptyContentWithASourceHashIsNotTreatedAsAbsent(t *testing.T) {
	want := []byte("x\n")
	sum := sha256.Sum256(want)
	res := &wsv1.ReadManagedFileResult{Content: nil, SourceSha256Hex: hex.EncodeToString(sum[:])}
	if !managedFileMatches(res, want) {
		t.Fatal("matching source hash with withheld content was not accepted")
	}
}

// A plain managed file is unaffected: no source hash, byte comparison.
func TestPlainFileStillComparesByBytes(t *testing.T) {
	want := []byte("import routes/*.caddy\n")
	if !managedFileMatches(&wsv1.ReadManagedFileResult{Content: want}, want) {
		t.Error("identical plain content compared unequal")
	}
	if managedFileMatches(&wsv1.ReadManagedFileResult{Content: []byte("different")}, want) {
		t.Error("different plain content compared equal")
	}
}
