package agent

// Secret materialization inside managed FILES (protocol minor 9).
//
// Keystone renders every config and never holds a secret value
// (ADR-W005 A2), so a rendered file carries references and the node
// substitutes them at the last possible moment — the same division
// materializeEnv already makes for container env, extended to the files
// an app reads instead of its environment.
//
// Three forms (plain, pbkdf2:, mosquitto:), because one secret genuinely
// has more than one representation in a working deployment: Authelia stores an OIDC client secret as a pbkdf2
// digest and verifies the client's presented value against it, while the
// app itself must send that same value in the clear. The store holds one
// plaintext secret and this derives the other form; the alternative —
// storing both — is two things that can disagree about one fact.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
	"golang.org/x/crypto/pbkdf2"
)

// pbkdf2Iterations matches Authelia's own default for the sha512 variant.
// It is not a knob: a file this agent writes must verify against the
// provider that reads it, and a mismatch here is a login failure with no
// message that names iterations.
const pbkdf2Iterations = 310000

// pbkdf2SaltBytes is Authelia's default salt length (16 bytes, base64'd).
const pbkdf2SaltBytes = 16

// fileSecretRefRegexp finds a secret reference inside file content. It is
// deliberately anchored on the optional pbkdf2:/mosquitto: prefix so that
// the forms cannot be confused, and bounded by the same character class the
// secret store itself accepts (secretNameRegexp) so a reference that
// cannot name a real secret is not treated as one.
var fileSecretRefRegexp = regexp.MustCompile(`(pbkdf2:|mosquitto:)?secret/([A-Za-z0-9][A-Za-z0-9._-]{0,63})`)

// materializeFileContent substitutes every secret reference in content.
//
// A dangling reference fails the whole write with NOT_FOUND naming the
// REFERENCE, never a value — the same rule materializeEnv follows, and
// the reason error strings from this package are safe to log.
//
// Failing the write rather than leaving the reference in place is the
// important half: a config file containing the literal text
// "secret/authelia-oidc" where a secret belongs is not a partial
// success, it is a provider that starts and rejects every login.
func (a *Agent) materializeFileContent(path string, content []byte) ([]byte, *wsv1.OpError) {
	var oerr *wsv1.OpError
	var consumed int // bytes of content already scanned, for column lookup
	out := fileSecretRefRegexp.ReplaceAllFunc(content, func(m []byte) []byte {
		if oerr != nil {
			return m
		}
		// ReplaceAllFunc gives no offset, so track where this match
		// starts by searching forward from the last one. References are
		// unique enough per file that this is exact, and the fallback
		// (column 0) is the no-indentation case anyway.
		idx := bytes.Index(content[consumed:], m)
		start := consumed
		if idx >= 0 {
			start = consumed + idx
			consumed = start + len(m)
		}

		sub := fileSecretRefRegexp.FindSubmatch(m)
		form, name := string(sub[1]), string(sub[2])
		value, err := a.openSecret(name)
		if err != nil {
			if os.IsNotExist(err) {
				oerr = opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND,
					"%s references secret/%s, which does not exist on this node", path, name)
			} else {
				oerr = opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "%s: opening secret/%s: %v", path, name, err)
			}
			return m
		}
		if form != "" {
			derive := pbkdf2Sha512Digest
			if form == "mosquitto:" {
				derive = mosquittoPasswordHash
			}
			digest, derr := derive(value)
			if derr != nil {
				oerr = opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "%s: hashing secret/%s: %v", path, name, derr)
				return m
			}
			value = digest
		}
		return []byte(indentContinuationLines(value, columnOf(content, start)))
	})
	if oerr != nil {
		return nil, oerr
	}
	return out, nil
}

// columnOf returns how many bytes into its line the offset sits.
func columnOf(content []byte, offset int) int {
	if offset <= 0 || offset > len(content) {
		return 0
	}
	if nl := bytes.LastIndexByte(content[:offset], '\n'); nl >= 0 {
		return offset - nl - 1
	}
	return offset
}

// indentContinuationLines re-indents every line of a multi-line value
// after the first, to the column the reference occupied.
//
// A single-line value is returned untouched, which is every secret except
// one shape: a PEM private key. The identity provider signs ID tokens
// with an RSA key it cannot generate on demand, and Authelia takes that
// key INLINE in its config, inside an indented YAML block:
//
//	jwks:
//	  - key: |
//	      secret/authelia-oidc-jwks
//
// Substituting a multi-line value there without re-indenting yields a
// document whose second line has dedented out of the block — invalid
// YAML, and an identity provider that will not start. Nothing about that
// is specific to PEM: any multi-line secret in any indented context has
// the same problem, so the fix belongs here rather than in the renderer
// that happened to hit it first.
func indentContinuationLines(value string, column int) string {
	if column == 0 || !strings.Contains(value, "\n") {
		return value
	}
	pad := strings.Repeat(" ", column)
	lines := strings.Split(value, "\n")
	for i := 1; i < len(lines); i++ {
		// A trailing newline leaves an empty final line; indenting it
		// would leave trailing whitespace in the document for no reason.
		if lines[i] == "" {
			continue
		}
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}

// pbkdf2Sha512Digest renders a value in the modular-crypt form Authelia
// stores and verifies against:
//
//	$pbkdf2-sha512$310000$<salt>$<hash>
//
// with both halves in the adapted base64 alphabet (standard base64
// without padding) that the passlib-derived format uses. A fresh random
// salt per call is correct and does not disturb drift detection: the
// bytes on disk are never what the caller compares against — the
// pre-materialization hash is (see ReadManagedFileResult).
// crypt64 is the base64 alphabet the crypt/passlib digest format uses,
// which is NOT Go's standard one. The only difference is index 62: '.'
// where standard base64 has '+'. Index 63 is '/' in both.
//
// This was base64.RawStdEncoding, under a comment claiming it was the
// adapted alphabet. It was not, and the failure was data-dependent in
// the worst way: a digest is only rejected when its salt or key happens
// to encode a '+', which over ~108 characters is roughly four secrets in
// five. It reads as flake, and a single hand-test can pass.
//
//	'identity_providers.oidc.clients[0].client_secret' could not decode
//	'$pbkdf2-sha512$310000$...$+uvh6...' to a *schema.PasswordDigest:
//	pbkdf2 decode error: illegal base64 data at input byte 0
//
// The alphabet is copied from what Authelia actually decodes with —
// go-crypt/crypt internal/encoding, Base64RawAdaptedEncoding — rather
// than from passlib's documentation, because that decoder is the thing
// that has to accept the output.
var crypt64 = base64.NewEncoding(
	"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789./",
).WithPadding(base64.NoPadding)

func pbkdf2Sha512Digest(value string) (string, error) {
	salt := make([]byte, pbkdf2SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := pbkdf2.Key([]byte(value), salt, pbkdf2Iterations, sha512.Size, sha512.New)
	return fmt.Sprintf("$pbkdf2-sha512$%d$%s$%s", pbkdf2Iterations,
		crypt64.EncodeToString(salt), crypt64.EncodeToString(key)), nil
}

// mosquittoIterations and mosquittoSaltBytes are what mosquitto_passwd
// itself writes for the current `$7$` scheme (PBKDF2-HMAC-SHA512, 64-byte
// key). The broker reads the iteration count from the entry, so this is
// a cost choice, not a compatibility one; it matches the tool's default.
const (
	mosquittoIterations = 101
	mosquittoSaltBytes  = 12
)

// mosquittoPasswordHash renders a value as the password field of a
// Mosquitto password-file entry:
//
//	$7$101$<base64 salt>$<base64 hash>
//
// Both halves are standard base64 WITH padding, unlike the crypt64 form
// above, and the key is derived from the raw (decoded) salt. The
// template supplies "<user>:"; only the hash is derived here, so the
// plaintext never leaves the node. A fresh salt per call is fine for the
// same reason as pbkdf2Sha512Digest: drift detection compares the
// pre-materialization source hash, never these bytes.
func mosquittoPasswordHash(value string) (string, error) {
	salt := make([]byte, mosquittoSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := pbkdf2.Key([]byte(value), salt, mosquittoIterations, sha512.Size, sha512.New)
	return fmt.Sprintf("$7$%d$%s$%s", mosquittoIterations,
		base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(key)), nil
}

// containsSecretRef reports whether content carries anything this would
// substitute. Used to refuse a materializing write that would silently
// do nothing, which is far more likely to be a renderer bug than an
// intention.
func containsSecretRef(content []byte) bool {
	return fileSecretRefRegexp.Match(bytes.TrimSpace(content))
}

// ---------------------------------------------------------------- source records

// managedSourceDir holds one small record per materialized managed file:
// the sha256 of the content as Keystone rendered it, before secrets were
// substituted.
//
// It lives in the agent's DATA directory, not the managed prefix. The
// managed prefix is bind-mounted into containers (read-only, per
// capability); the data directory is the node's own, and #48 moved it out
// of that prefix precisely so agent state is not something a container
// can read. A record here says nothing secret, but it is agent
// bookkeeping and belongs with the agent's other bookkeeping.
func (a *Agent) managedSourceDir() string {
	return filepath.Join(a.cfg.DataDir, "managed-source")
}

// managedSourcePath maps a managed path to its record. The path is
// hashed rather than embedded: managed paths contain separators and would
// otherwise need escaping, and a hash cannot escape the directory however
// odd the path is.
func (a *Agent) managedSourcePath(managedPath string) string {
	sum := sha256.Sum256([]byte(managedPath))
	return filepath.Join(a.managedSourceDir(), hex.EncodeToString(sum[:])+".sha256")
}

// recordManagedSource writes the record for a materialized file, or
// removes it when the file is written without materialization — a file
// that stops being materialized must stop claiming a source, or its
// content would be withheld from reads forever.
func (a *Agent) recordManagedSource(managedPath string, materialized bool, sourceHex string) *wsv1.OpError {
	recPath := a.managedSourcePath(managedPath)
	if !materialized {
		if err := os.Remove(recPath); err != nil && !os.IsNotExist(err) {
			return opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "clearing source record: %v", err)
		}
		return nil
	}
	if err := os.MkdirAll(a.managedSourceDir(), 0o700); err != nil {
		return opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "source record dir: %v", err)
	}
	if err := os.WriteFile(recPath, []byte(sourceHex), 0o600); err != nil {
		return opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "source record: %v", err)
	}
	return nil
}

// readManagedSource returns the recorded source hash, or "" when the file
// was not written with materialization.
func (a *Agent) readManagedSource(managedPath string) (string, *wsv1.OpError) {
	b, err := os.ReadFile(a.managedSourcePath(managedPath))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "reading source record: %v", err)
	}
	return string(bytes.TrimSpace(b)), nil
}
