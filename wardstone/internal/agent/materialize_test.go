package agent

import (
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
	"golang.org/x/crypto/pbkdf2"
)

func writeFileOp(path string, content string, materialize bool) *wsv1.WriteManagedFile {
	return &wsv1.WriteManagedFile{Path: path, Content: []byte(content), MaterializeSecrets: materialize}
}

// materializeAgent extends storeOnlyAgent (which covers the secret store)
// with a managed prefix, so the same agent can both hold secrets and
// write the files that reference them.
func materializeAgent(t *testing.T) *Agent {
	t.Helper()
	a := storeOnlyAgent(t)
	a.cfg.ManagedPrefixes = []string{t.TempDir()}
	return a
}

func putSecret(t *testing.T, a *Agent, name, value string) {
	t.Helper()
	if _, err := a.sealSecret(name, value); err != nil {
		t.Fatalf("sealSecret %s: %v", name, err)
	}
}

// TestMaterializedFileResolvesBothForms is the point of the whole change:
// one plaintext secret in the store, two representations in the file that
// needs them. Authelia stores an OIDC client secret as a pbkdf2 digest
// and verifies the client's presented value against it, while the app
// must send that same value in the clear.
func TestMaterializedFileResolvesBothForms(t *testing.T) {
	a := materializeAgent(t)
	putSecret(t, a, "immich-oidc", "s3cret-value")
	path := filepath.Join(a.cfg.ManagedPrefixes[0], "authelia", "clients.yml")
	src := "plain: secret/immich-oidc\ndigest: pbkdf2:secret/immich-oidc\n"

	if _, oerr := a.writeManagedFile(writeFileOp(path, src, true)); oerr != nil {
		t.Fatalf("write: %v", oerr)
	}
	onDisk := readManagedForTest(t, a, path)

	if !strings.Contains(onDisk, "plain: s3cret-value") {
		t.Errorf("plaintext reference not resolved:\n%s", onDisk)
	}
	if !strings.Contains(onDisk, "digest: $pbkdf2-sha512$310000$") {
		t.Errorf("digest reference not resolved to Authelia's form:\n%s", onDisk)
	}
	// The digest must not be the value, however it is encoded.
	digestLine := onDisk[strings.Index(onDisk, "digest:"):]
	if strings.Contains(digestLine, "s3cret-value") {
		t.Errorf("digest line carries the plaintext:\n%s", digestLine)
	}
}

// A materialized file's bytes never travel back to Keystone: they hold
// live secrets, and returning them would carry those into plans, error
// strings and the Butler's tool results. The pre-materialization hash
// answers "did this drift?" without any of that.
func TestReadOfMaterializedFileWithholdsContent(t *testing.T) {
	a := materializeAgent(t)
	putSecret(t, a, "immich-oidc", "s3cret-value")
	path := filepath.Join(a.cfg.ManagedPrefixes[0], "authelia", "clients.yml")
	src := "digest: pbkdf2:secret/immich-oidc\n"
	if _, oerr := a.writeManagedFile(writeFileOp(path, src, true)); oerr != nil {
		t.Fatal(oerr)
	}

	res, oerr := a.readManagedFile(&wsv1.ReadManagedFile{Path: path})
	if oerr != nil {
		t.Fatalf("read: %v", oerr)
	}
	got := res.ReadManagedFile
	if len(got.GetContent()) != 0 {
		t.Fatalf("read returned content for a materialized file: %q", got.GetContent())
	}
	sum := sha256.Sum256([]byte(src))
	if got.GetSourceSha256Hex() != hex.EncodeToString(sum[:]) {
		t.Fatalf("source hash = %q, want the hash of the pre-materialization content", got.GetSourceSha256Hex())
	}
}

// An ordinary managed file is unaffected — content still comes back, and
// no source hash claims otherwise.
func TestPlainFileStillReturnsContent(t *testing.T) {
	a := materializeAgent(t)
	path := filepath.Join(a.cfg.ManagedPrefixes[0], "Caddyfile")
	if _, oerr := a.writeManagedFile(writeFileOp(path, "hello\n", false)); oerr != nil {
		t.Fatal(oerr)
	}
	res, oerr := a.readManagedFile(&wsv1.ReadManagedFile{Path: path})
	if oerr != nil {
		t.Fatal(oerr)
	}
	if string(res.ReadManagedFile.GetContent()) != "hello\n" {
		t.Errorf("content = %q", res.ReadManagedFile.GetContent())
	}
	if res.ReadManagedFile.GetSourceSha256Hex() != "" {
		t.Errorf("plain file claimed a source hash: %q", res.ReadManagedFile.GetSourceSha256Hex())
	}
}

// A file that stops being materialized must stop claiming a source, or
// its content would be withheld from reads forever.
func TestRewriteWithoutMaterializationClearsTheRecord(t *testing.T) {
	a := materializeAgent(t)
	putSecret(t, a, "immich-oidc", "s3cret-value")
	path := filepath.Join(a.cfg.ManagedPrefixes[0], "authelia", "clients.yml")
	if _, oerr := a.writeManagedFile(writeFileOp(path, "d: pbkdf2:secret/immich-oidc\n", true)); oerr != nil {
		t.Fatal(oerr)
	}
	if _, oerr := a.writeManagedFile(writeFileOp(path, "plain again\n", false)); oerr != nil {
		t.Fatal(oerr)
	}
	res, oerr := a.readManagedFile(&wsv1.ReadManagedFile{Path: path})
	if oerr != nil {
		t.Fatal(oerr)
	}
	if res.ReadManagedFile.GetSourceSha256Hex() != "" {
		t.Fatalf("stale source record survived: %q", res.ReadManagedFile.GetSourceSha256Hex())
	}
	if string(res.ReadManagedFile.GetContent()) != "plain again\n" {
		t.Fatalf("content = %q", res.ReadManagedFile.GetContent())
	}
}

// A dangling reference fails the write. Leaving it in place would ship a
// config with a secret-shaped hole and no error anywhere — a provider
// that starts and rejects every login.
func TestDanglingReferenceFailsTheWriteAndNamesNoValue(t *testing.T) {
	a := materializeAgent(t)
	putSecret(t, a, "present", "the-value")
	path := filepath.Join(a.cfg.ManagedPrefixes[0], "authelia", "clients.yml")
	_, oerr := a.writeManagedFile(writeFileOp(path, "a: secret/present\nb: secret/missing\n", true))
	if oerr == nil {
		t.Fatal("write succeeded with a dangling reference")
	}
	if oerr.GetCode() != wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Errorf("code = %s, want NOT_FOUND", oerr.GetCode())
	}
	if !strings.Contains(oerr.GetMessage(), "secret/missing") {
		t.Errorf("message does not name the reference: %q", oerr.GetMessage())
	}
	// An error message must never carry a value, including one that
	// resolved fine before the failure.
	if strings.Contains(oerr.GetMessage(), "the-value") {
		t.Errorf("error message carries a secret value: %q", oerr.GetMessage())
	}
	// And nothing may be left on disk.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a file was written despite the failure")
	}
}

// Asking for materialization on content with nothing to materialize is a
// renderer that stopped emitting references — refused, rather than
// shipping a config with a hole where a secret belongs.
func TestMaterializeWithNoReferenceIsRefused(t *testing.T) {
	a := materializeAgent(t)
	_, oerr := a.writeManagedFile(writeFileOp(filepath.Join(a.cfg.ManagedPrefixes[0], "x.yml"), "nothing here\n", true))
	if oerr == nil || oerr.GetCode() != wsv1.ErrorCode_ERROR_CODE_INVALID {
		t.Fatalf("want INVALID, got %v", oerr)
	}
}

// A materialized file holds live secrets, so it must not be world- or
// group-readable however permissive the caller's mode was.
func TestMaterializedFileIsNotWorldReadable(t *testing.T) {
	a := materializeAgent(t)
	putSecret(t, a, "immich-oidc", "s3cret-value")
	path := filepath.Join(a.cfg.ManagedPrefixes[0], "authelia", "clients.yml")
	op := writeFileOp(path, "d: pbkdf2:secret/immich-oidc\n", true)
	op.Mode = 0o644
	if _, oerr := a.writeManagedFile(op); oerr != nil {
		t.Fatal(oerr)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("mode = %04o, want no group or other access", perm)
	}
}

// The digest must verify against the value the app will present, or the
// whole exercise produces a provider that rejects every correct login.
func TestDigestVerifiesAgainstThePlaintext(t *testing.T) {
	digest, err := pbkdf2Sha512Digest("correct-value")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPbkdf2Sha512(t, digest, "correct-value") {
		t.Error("digest does not verify against the value it was derived from")
	}
	if verifyPbkdf2Sha512(t, digest, "wrong-value") {
		t.Error("digest verifies against the wrong value")
	}
	// A fresh salt per call: two digests of one value must differ, or the
	// file leaks whether two clients share a secret.
	other, err := pbkdf2Sha512Digest("correct-value")
	if err != nil {
		t.Fatal(err)
	}
	if digest == other {
		t.Error("two digests of the same value are identical — the salt is not random")
	}
}

func readManagedForTest(t *testing.T, a *Agent, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// verifyPbkdf2Sha512 re-derives the digest the way a PROVIDER would, from
// the encoded string alone. Written independently of the producing code
// on purpose: a test that called pbkdf2Sha512Digest twice would prove
// only that the function is deterministic given a salt, not that what it
// emits is a digest anyone else can check. Authelia rejecting every
// correct login is the failure this guards against, and it is silent.
func verifyPbkdf2Sha512(t *testing.T, encoded, value string) bool {
	t.Helper()
	parts := strings.Split(encoded, "$")
	// "", "pbkdf2-sha512", iterations, salt, hash
	if len(parts) != 5 || parts[1] != "pbkdf2-sha512" {
		t.Fatalf("digest is not in modular-crypt form: %q", encoded)
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil {
		t.Fatalf("iteration count %q: %v", parts[2], err)
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[3])
	if err != nil {
		t.Fatalf("salt is not raw-std base64: %v", err)
	}
	want, err := enc.DecodeString(parts[4])
	if err != nil {
		t.Fatalf("hash is not raw-std base64: %v", err)
	}
	got := pbkdf2.Key([]byte(value), salt, iter, sha512.Size, sha512.New)
	return hmac.Equal(got, want)
}

// TestMultiLineSecretKeepsItsBlockIndentation is the PEM case, and it is
// the reason indentation is handled at all. Authelia takes its OIDC
// signing key INLINE, inside an indented YAML block:
//
//	jwks:
//	  - key: |
//	      secret/authelia-oidc-jwks
//
// Substituting a multi-line value there without re-indenting dedents
// every line after the first out of the block. The result is invalid
// YAML and an identity provider that will not start — which is a total
// outage of every login, from a whitespace bug.
func TestMultiLineSecretKeepsItsBlockIndentation(t *testing.T) {
	a := materializeAgent(t)
	// A plain multi-line value, not a key-shaped literal. The real case
	// is a PEM private key, but nothing about the indentation rule cares
	// what the lines say — and a fake key in the source is exactly the
	// shape the repo's secret scanner exists to catch, which it cannot
	// tell from a real one.
	multiline := "FIRST-LINE\nsecond-line\nthird-line\nLAST-LINE\n"
	putSecret(t, a, "authelia-oidc-jwks", multiline)

	path := filepath.Join(a.cfg.ManagedPrefixes[0], "authelia", "configuration.yml")
	src := "jwks:\n  - key: |\n      secret/authelia-oidc-jwks\n"
	if _, oerr := a.writeManagedFile(writeFileOp(path, src, true)); oerr != nil {
		t.Fatalf("write: %v", oerr)
	}
	got := readManagedForTest(t, a, path)

	for _, line := range []string{
		"      FIRST-LINE",
		"      second-line",
		"      third-line",
		"      LAST-LINE",
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("line %q is not indented into the block:\n%s", strings.TrimSpace(line), got)
		}
	}
	// A trailing newline must not leave a line of pure whitespace.
	for _, line := range strings.Split(got, "\n") {
		if line != "" && strings.TrimSpace(line) == "" {
			t.Errorf("whitespace-only line left in the document:\n%q", got)
		}
	}
}

// A single-line secret at a column is untouched: re-indenting a value
// with no newlines would be a no-op at best and corruption at worst.
func TestSingleLineSecretIsNotReindented(t *testing.T) {
	a := materializeAgent(t)
	putSecret(t, a, "db-password", "p4ssw0rd")
	path := filepath.Join(a.cfg.ManagedPrefixes[0], "x.yml")
	if _, oerr := a.writeManagedFile(writeFileOp(path, "auth:\n    pw: secret/db-password\n", true)); oerr != nil {
		t.Fatal(oerr)
	}
	if got := readManagedForTest(t, a, path); got != "auth:\n    pw: p4ssw0rd\n" {
		t.Errorf("got %q", got)
	}
}

// The RSA kind exists because an OIDC provider cannot generate its own
// signing key on demand, and a random blob is not a key.
func TestEnsureSecretRsaKindProducesAUsableKey(t *testing.T) {
	a := materializeAgent(t)
	if _, oerr := a.ensureSecret(&wsv1.EnsureSecret{
		Name: "authelia-oidc-jwks", Kind: wsv1.SecretKind_SECRET_KIND_RSA_PRIVATE_KEY,
	}); oerr != nil {
		t.Fatalf("ensureSecret: %v", oerr)
	}
	value, err := a.openSecret("authelia-oidc-jwks")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(value))
	if block == nil {
		t.Fatalf("stored value is not PEM:\n%s", value)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("stored key does not parse: %v", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("stored key is %T, want *rsa.PrivateKey", key)
	}
	if bits := rsaKey.N.BitLen(); bits < 2048 {
		t.Errorf("key is %d bits, want at least 2048", bits)
	}
	// It is multi-line, which is exactly why the indentation rule above
	// has to hold — the two facts are one requirement.
	if !strings.Contains(value, "\n") {
		t.Error("a PEM key with no newlines would not exercise the indentation path")
	}
}

// The default kind stays random, so an older Keystone that never sets the
// field still gets what it has always asked for.
func TestUnspecifiedKindIsStillRandom(t *testing.T) {
	a := materializeAgent(t)
	if _, oerr := a.ensureSecret(&wsv1.EnsureSecret{Name: "session"}); oerr != nil {
		t.Fatal(oerr)
	}
	value, err := a.openSecret("session")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(value, "BEGIN") || strings.Contains(value, "\n") {
		t.Errorf("unspecified kind produced something other than a random token: %q", value)
	}
}
