package agent

// Secret materialization inside managed FILES (protocol minor 9).
//
// Keystone renders every config and never holds a secret value
// (ADR-W005 A2), so a rendered file carries references and the node
// substitutes them at the last possible moment — the same division
// materializeEnv already makes for container env, extended to the files
// an app reads instead of its environment.
//
// Two forms, because one secret genuinely has two representations in a
// working deployment: Authelia stores an OIDC client secret as a pbkdf2
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
// deliberately anchored on the optional pbkdf2: prefix so that the two
// forms cannot be confused, and bounded by the same character class the
// secret store itself accepts (secretNameRegexp) so a reference that
// cannot name a real secret is not treated as one.
var fileSecretRefRegexp = regexp.MustCompile(`(pbkdf2:)?secret/([A-Za-z0-9][A-Za-z0-9._-]{0,63})`)

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
	out := fileSecretRefRegexp.ReplaceAllFunc(content, func(m []byte) []byte {
		if oerr != nil {
			return m
		}
		sub := fileSecretRefRegexp.FindSubmatch(m)
		hashed, name := len(sub[1]) > 0, string(sub[2])
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
		if !hashed {
			return []byte(value)
		}
		digest, derr := pbkdf2Sha512Digest(value)
		if derr != nil {
			oerr = opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "%s: hashing secret/%s: %v", path, name, derr)
			return m
		}
		return []byte(digest)
	})
	if oerr != nil {
		return nil, oerr
	}
	return out, nil
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
func pbkdf2Sha512Digest(value string) (string, error) {
	salt := make([]byte, pbkdf2SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := pbkdf2.Key([]byte(value), salt, pbkdf2Iterations, sha512.Size, sha512.New)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$pbkdf2-sha512$%d$%s$%s", pbkdf2Iterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
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
