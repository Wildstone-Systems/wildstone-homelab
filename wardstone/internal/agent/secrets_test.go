package agent

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
	"github.com/chickenrat17/wildstone-homelab/proto/recoveryseal"
)

func (f *fakeDocker) envOf(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[name]; ok {
		return append([]string(nil), c.env...)
	}
	return nil
}

// storeOnlyAgent builds an Agent good for exercising the secret store
// directly (the store touches only cfg.DataDir).
func storeOnlyAgent(t *testing.T) *Agent {
	t.Helper()
	return &Agent{cfg: Config{DataDir: t.TempDir()}}
}

// TestSecretStoreSealing pins the at-rest properties: the value is not
// recoverable from the files, the key is 0600, and the name is bound
// into the AEAD so a renamed ciphertext refuses to open.
func TestSecretStoreSealing(t *testing.T) {
	a := storeOnlyAgent(t)
	const value = "super-secret-tunnel-token-value"
	if _, err := a.sealSecret("tok", value); err != nil {
		t.Fatal(err)
	}

	sealed, err := os.ReadFile(filepath.Join(a.secretsDir(), "tok"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(value)) {
		t.Fatal("plaintext value present in the sealed file")
	}
	ki, err := os.Stat(a.secretKeyPath())
	if err != nil {
		t.Fatal(err)
	}
	if ki.Mode().Perm() != 0o600 {
		t.Fatalf("secret.key mode = %v, want 0600", ki.Mode().Perm())
	}

	got, err := a.openSecret("tok")
	if err != nil || got != value {
		t.Fatalf("openSecret = %q, %v", got, err)
	}

	// Rename the ciphertext to another name: the AEAD's associated data
	// (the name) no longer matches, so it must refuse to open.
	if err := os.Rename(filepath.Join(a.secretsDir(), "tok"), filepath.Join(a.secretsDir(), "other")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.openSecret("other"); err == nil {
		t.Fatal("renamed ciphertext opened under a different name")
	}
}

func TestSecretNameValidation(t *testing.T) {
	for _, bad := range []string{"", "..", ".hidden", "-lead", "a/b", "a\\b", strings.Repeat("x", 65)} {
		if validSecretName(bad) == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	for _, good := range []string{"cloudflared-tunnel-token", "restic-photos", "A.b_c-9"} {
		if oerr := validSecretName(good); oerr != nil {
			t.Errorf("name %q rejected: %s", good, oerr.GetMessage())
		}
	}
}

// TestSecretOpsAndMaterialization drives the minor-7 surface end to end
// over the real gRPC session + fake docker: generate, idempotent ensure,
// put, list (no values), materialize into a container create, dangling
// reference refusal, and rotation.
func TestSecretOpsAndMaterialization(t *testing.T) {
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// Generate-at-install: EnsureSecret creates v1; a second Ensure is a
	// no-op reporting existed.
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureSecret{
		EnsureSecret: &wsv1.EnsureSecret{Name: "immich-oidc"},
	}})
	if res.GetError() != nil || res.GetEnsureSecret().GetExisted() || res.GetEnsureSecret().GetVersion() != 1 {
		t.Fatalf("first ensure = %+v", res)
	}
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureSecret{
		EnsureSecret: &wsv1.EnsureSecret{Name: "immich-oidc"},
	}})
	if !res.GetEnsureSecret().GetExisted() || res.GetEnsureSecret().GetVersion() != 1 {
		t.Fatalf("second ensure = %+v, want existed at v1", res)
	}

	// Operator-supplied value.
	const tokenValue = "eyJhIjoi-KNOWN-FAKE-tunnel-token-1"
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_PutSecret{
		PutSecret: &wsv1.PutSecret{Name: "cloudflared-tunnel-token", Value: tokenValue},
	}})
	if res.GetError() != nil || res.GetPutSecret().GetVersion() != 1 {
		t.Fatalf("put = %+v", res)
	}

	// List: names + versions, and the result cannot carry values by
	// construction — assert the serialized result doesn't either.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ListSecrets{
		ListSecrets: &wsv1.ListSecrets{},
	}})
	names := map[string]uint32{}
	for _, s := range res.GetListSecrets().GetSecrets() {
		names[s.GetName()] = s.GetVersion()
	}
	if names["immich-oidc"] != 1 || names["cloudflared-tunnel-token"] != 1 {
		t.Fatalf("list = %+v", names)
	}
	if strings.Contains(res.String(), tokenValue) {
		t.Fatal("ListSecrets result leaked a value")
	}

	// Materialization: the docker create body gets the plaintext; the
	// reference never reaches docker.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "cloudflared", Image: testDigestImage,
			Env: map[string]string{
				"TUNNEL_TOKEN": "secret/cloudflared-tunnel-token",
				"PLAIN":        "as-is",
			},
		},
	}})
	if res.GetError() != nil {
		t.Fatalf("ensure container: %s", res.GetError().GetMessage())
	}
	env := fake.envOf("cloudflared")
	if !containsString(env, "TUNNEL_TOKEN="+tokenValue) || !containsString(env, "PLAIN=as-is") {
		t.Fatalf("create env = %v, want materialized token and untouched PLAIN", env)
	}
	if containsString(env, "TUNNEL_TOKEN=secret/cloudflared-tunnel-token") {
		t.Fatal("reference reached docker unmaterialized")
	}

	// Dangling reference: NOT_FOUND naming the reference, no container.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "broken", Image: testDigestImage,
			Env: map[string]string{"X": "secret/ghost"},
		},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_NOT_FOUND ||
		!strings.Contains(res.GetError().GetMessage(), "secret/ghost") {
		t.Fatalf("dangling ref = %+v, want NOT_FOUND naming secret/ghost", res)
	}
	if fake.envOf("broken") != nil {
		t.Fatal("container created despite dangling secret reference")
	}

	// Rotation regenerates: version bumps and the next create sees a
	// different value.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureSecret{
		EnsureSecret: &wsv1.EnsureSecret{Name: "cloudflared-tunnel-token", Rotate: true},
	}})
	if res.GetError() != nil || res.GetEnsureSecret().GetVersion() != 2 {
		t.Fatalf("rotate = %+v, want version 2", res)
	}
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "cloudflared2", Image: testDigestImage,
			Env: map[string]string{"TUNNEL_TOKEN": "secret/cloudflared-tunnel-token"},
		},
	}})
	if res.GetError() != nil {
		t.Fatalf("ensure after rotate: %s", res.GetError().GetMessage())
	}
	if containsString(fake.envOf("cloudflared2"), "TUNNEL_TOKEN="+tokenValue) {
		t.Fatal("rotated secret still materializes the old value")
	}
}

// TestPutSecretNeverAudited pins the audit allowlist: PutSecret's value
// must not appear in the audit attrs (and the arm is explicitly listed,
// not falling through the safe default).
func TestPutSecretNeverAudited(t *testing.T) {
	arm, fields := auditFields(&wsv1.Operation{Op: &wsv1.Operation_PutSecret{
		PutSecret: &wsv1.PutSecret{Name: "tok", Value: "the-secret-value"},
	}})
	if arm != "PutSecret" {
		t.Fatalf("arm = %s", arm)
	}
	for _, f := range fields {
		if strings.Contains(fmt.Sprint(f.Value.Any()), "the-secret-value") {
			t.Fatalf("audit attr leaked the value: %v", f)
		}
	}
}

func TestRecoverySecretBundleRoundTrip(t *testing.T) {
	source := storeOnlyAgent(t)
	if _, err := source.sealSecret("alpha", "first-secret-value"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.sealSecret("alpha", "rotated-secret-value"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.sealSecret("beta", "second-secret-value"); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := recoveryseal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	exported, oerr := source.exportSecretBundle(&wsv1.ExportSecretBundle{RecoveryPublicKey: publicKey})
	if oerr != nil {
		t.Fatalf("export: %s", oerr.GetMessage())
	}
	sealed := exported.ExportSecretBundle.GetSealedBundle()
	if exported.ExportSecretBundle.GetSecretCount() != 2 {
		t.Fatalf("exported count = %d, want 2", exported.ExportSecretBundle.GetSecretCount())
	}
	for _, plaintext := range []string{"rotated-secret-value", "second-secret-value"} {
		if bytes.Contains(sealed, []byte(plaintext)) {
			t.Fatalf("sealed recovery bundle contains %q", plaintext)
		}
	}

	target := storeOnlyAgent(t)
	imported, oerr := target.importSecretBundle(&wsv1.ImportSecretBundle{
		RecoveryPrivateKey: privateKey,
		SealedBundle:       sealed,
	})
	if oerr != nil {
		t.Fatalf("import: %s", oerr.GetMessage())
	}
	if imported.ImportSecretBundle.GetSecretCount() != 2 {
		t.Fatalf("imported count = %d, want 2", imported.ImportSecretBundle.GetSecretCount())
	}
	for name, want := range map[string]string{"alpha": "rotated-secret-value", "beta": "second-secret-value"} {
		got, err := target.openSecret(name)
		if err != nil || got != want {
			t.Fatalf("open %s = %q, %v; want %q", name, got, err, want)
		}
	}
	meta, err := target.readSecretMeta("alpha")
	if err != nil || meta.Version != 2 {
		t.Fatalf("alpha metadata = %+v, %v; want version 2", meta, err)
	}
}

func TestRecoverySecretImportRejectsWrongKeyAndNonEmptyStore(t *testing.T) {
	source := storeOnlyAgent(t)
	if _, err := source.sealSecret("alpha", "source-value"); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := recoveryseal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	exported, oerr := source.exportSecretBundle(&wsv1.ExportSecretBundle{RecoveryPublicKey: publicKey})
	if oerr != nil {
		t.Fatal(oerr.GetMessage())
	}
	_, wrongPrivateKey, err := recoveryseal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	target := storeOnlyAgent(t)
	if _, oerr := target.importSecretBundle(&wsv1.ImportSecretBundle{
		RecoveryPrivateKey: wrongPrivateKey,
		SealedBundle:       exported.ExportSecretBundle.GetSealedBundle(),
	}); oerr == nil {
		t.Fatal("wrong recovery key was accepted")
	}
	if _, err := target.sealSecret("existing", "must-survive"); err != nil {
		t.Fatal(err)
	}
	if _, oerr := target.importSecretBundle(&wsv1.ImportSecretBundle{
		RecoveryPrivateKey: privateKey,
		SealedBundle:       exported.ExportSecretBundle.GetSealedBundle(),
	}); oerr == nil || oerr.GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("non-empty import error = %+v, want FORBIDDEN", oerr)
	}
	got, err := target.openSecret("existing")
	if err != nil || got != "must-survive" {
		t.Fatalf("existing secret changed: %q, %v", got, err)
	}
}

func TestRecoveryOpsNeverAuditKeysOrBundles(t *testing.T) {
	for _, tc := range []struct {
		op   *wsv1.Operation
		arm  string
		leak string
	}{
		{&wsv1.Operation{Op: &wsv1.Operation_ExportSecretBundle{ExportSecretBundle: &wsv1.ExportSecretBundle{RecoveryPublicKey: "public-key-material"}}}, "ExportSecretBundle", "public-key-material"},
		{&wsv1.Operation{Op: &wsv1.Operation_ImportSecretBundle{ImportSecretBundle: &wsv1.ImportSecretBundle{RecoveryPrivateKey: "private-key-material", SealedBundle: []byte("sealed-material")}}}, "ImportSecretBundle", "private-key-material"},
	} {
		arm, fields := auditFields(tc.op)
		if arm != tc.arm {
			t.Fatalf("arm = %q, want %q", arm, tc.arm)
		}
		for _, field := range fields {
			if strings.Contains(fmt.Sprint(field.Value.Any()), tc.leak) {
				t.Fatalf("audit attr leaked recovery material: %v", field)
			}
		}
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
