package agent

// Node-side secret store and materializer (ADR-W012, protocol minor 7).
//
// A secret VALUE exists only here: sealed with AES-256-GCM (stdlib —
// wardstone adds no dependency for this, ADR-W003 §A4.5) under a 32-byte
// key at <DataDir>/secret.key, one file per secret at
// <DataDir>/secrets/<name> holding nonce‖ciphertext, plus a small
// plaintext sidecar <name>.meta.json (version + timestamps — metadata is
// not secret; the value is).
//
// The op surface over this store is WRITE-ONLY: EnsureSecret (local
// random generation — the value never crosses the wire), PutSecret
// (operator-supplied value, transits once over mTLS), ListSecrets
// (metadata only). There is no GetSecret arm at all. The single read
// path is materializeEnv, called by ensureContainer to substitute env
// values of the form secret/<name> into the docker create body.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// secretRefPrefix marks a container env VALUE as a reference into the
// store. Kept in sync with keystone/resource's secretRefRegexp (the
// small-duplicated-constant pattern; keystone cannot be imported here).
const secretRefPrefix = "secret/"

// secretNameRegexp bounds a secret name: it is used verbatim as a file
// name in the store, so the charset must exclude separators, and the
// leading alphanumeric rules out "." / ".." shapes.
var secretNameRegexp = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// defaultSecretBytes is EnsureSecret's generation size when the op
// leaves length_bytes at 0.
const defaultSecretBytes = 32

// secretMeta is the plaintext sidecar: name-level metadata only.
type secretMeta struct {
	Version     uint32 `json:"version"`
	CreatedUnix int64  `json:"createdUnix"`
	UpdatedUnix int64  `json:"updatedUnix"`
}

func (a *Agent) secretsDir() string    { return filepath.Join(a.cfg.DataDir, "secrets") }
func (a *Agent) secretKeyPath() string { return filepath.Join(a.cfg.DataDir, "secret.key") }

// secretStoreKey loads the store key, generating it (0600) on first use.
func (a *Agent) secretStoreKey() ([]byte, error) {
	key, err := os.ReadFile(a.secretKeyPath())
	if err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("secret.key is %d bytes, want 32 — refusing a corrupt key", len(key))
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(a.cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(a.secretKeyPath(), key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func (a *Agent) secretAEAD() (cipher.AEAD, error) {
	key, err := a.secretStoreKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealSecret writes name's value (nonce‖ciphertext, 0600) and its meta
// sidecar, bumping the version.
func (a *Agent) sealSecret(name, value string) (uint32, error) {
	aead, err := a.secretAEAD()
	if err != nil {
		return 0, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return 0, err
	}
	sealed := append(nonce, aead.Seal(nil, nonce, []byte(value), []byte(name))...)

	if err := os.MkdirAll(a.secretsDir(), 0o700); err != nil {
		return 0, err
	}
	meta := secretMeta{Version: 1, CreatedUnix: time.Now().Unix()}
	if prev, err := a.readSecretMeta(name); err == nil {
		meta = prev
		meta.Version++
	}
	meta.UpdatedUnix = time.Now().Unix()

	if err := os.WriteFile(filepath.Join(a.secretsDir(), name), sealed, 0o600); err != nil {
		return 0, err
	}
	mb, err := json.Marshal(meta)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(a.secretsDir(), name+".meta.json"), mb, 0o600); err != nil {
		return 0, err
	}
	return meta.Version, nil
}

// openSecret is the store's ONLY value-read path, used exclusively by
// materializeEnv. The name is bound into the AEAD as associated data, so
// a ciphertext file renamed to another secret's name fails to open
// instead of silently materializing the wrong value.
func (a *Agent) openSecret(name string) (string, error) {
	sealed, err := os.ReadFile(filepath.Join(a.secretsDir(), name))
	if err != nil {
		return "", err
	}
	aead, err := a.secretAEAD()
	if err != nil {
		return "", err
	}
	if len(sealed) < aead.NonceSize() {
		return "", fmt.Errorf("secret %q: sealed file too short", name)
	}
	value, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte(name))
	if err != nil {
		return "", fmt.Errorf("secret %q: %w", name, err)
	}
	return string(value), nil
}

func (a *Agent) readSecretMeta(name string) (secretMeta, error) {
	b, err := os.ReadFile(filepath.Join(a.secretsDir(), name+".meta.json"))
	if err != nil {
		return secretMeta{}, err
	}
	var m secretMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return secretMeta{}, err
	}
	return m, nil
}

func (a *Agent) secretExists(name string) bool {
	_, err := os.Stat(filepath.Join(a.secretsDir(), name))
	return err == nil
}

func validSecretName(name string) *wsv1.OpError {
	if !secretNameRegexp.MatchString(name) {
		return opErr(wsv1.ErrorCode_ERROR_CODE_INVALID,
			"secret name %q: must be alphanumeric-led [A-Za-z0-9._-], at most 64 chars", name)
	}
	return nil
}

// ---------------------------------------------------------------- op handlers

func (a *Agent) ensureSecret(op *wsv1.EnsureSecret) (*wsv1.OpResult_EnsureSecret, *wsv1.OpError) {
	if oerr := validSecretName(op.GetName()); oerr != nil {
		return nil, oerr
	}
	existed := a.secretExists(op.GetName())
	if existed && !op.GetRotate() {
		meta, err := a.readSecretMeta(op.GetName())
		if err != nil {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "secret meta: %v", err)
		}
		return &wsv1.OpResult_EnsureSecret{EnsureSecret: &wsv1.EnsureSecretResult{
			Existed: true, Version: meta.Version,
		}}, nil
	}

	value, oerr := generateSecretValue(op)
	if oerr != nil {
		return nil, oerr
	}
	version, err := a.sealSecret(op.GetName(), value)
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "sealing secret: %v", err)
	}
	return &wsv1.OpResult_EnsureSecret{EnsureSecret: &wsv1.EnsureSecretResult{
		Existed: existed, Version: version,
	}}, nil
}

func (a *Agent) putSecret(op *wsv1.PutSecret) (*wsv1.OpResult_PutSecret, *wsv1.OpError) {
	if oerr := validSecretName(op.GetName()); oerr != nil {
		return nil, oerr
	}
	if op.GetValue() == "" {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "secret %q: empty value refused", op.GetName())
	}
	version, err := a.sealSecret(op.GetName(), op.GetValue())
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "sealing secret: %v", err)
	}
	return &wsv1.OpResult_PutSecret{PutSecret: &wsv1.PutSecretResult{Version: version}}, nil
}

func (a *Agent) listSecrets() (*wsv1.OpResult_ListSecrets, *wsv1.OpError) {
	entries, err := os.ReadDir(a.secretsDir())
	if os.IsNotExist(err) {
		return &wsv1.OpResult_ListSecrets{ListSecrets: &wsv1.ListSecretsResult{}}, nil
	}
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "listing secrets: %v", err)
	}
	res := &wsv1.ListSecretsResult{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasSuffix(name, ".meta.json") {
			continue
		}
		info := &wsv1.SecretInfo{Name: name}
		if meta, err := a.readSecretMeta(name); err == nil {
			info.Version = meta.Version
			info.CreatedUnixSec = meta.CreatedUnix
			info.UpdatedUnixSec = meta.UpdatedUnix
		}
		res.Secrets = append(res.Secrets, info)
	}
	return &wsv1.OpResult_ListSecrets{ListSecrets: res}, nil
}

// generateSecretValue produces the material for one EnsureSecret. The
// kind vocabulary is closed (SecretKind): the agent has to know how to
// produce each one, so "some other algorithm" is a code change here
// rather than a parameter a caller can wander into.
func generateSecretValue(op *wsv1.EnsureSecret) (string, *wsv1.OpError) {
	switch op.GetKind() {
	case wsv1.SecretKind_SECRET_KIND_UNSPECIFIED, wsv1.SecretKind_SECRET_KIND_RANDOM:
		n := int(op.GetLengthBytes())
		if n == 0 {
			n = defaultSecretBytes
		}
		if n > 1024 {
			return "", opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "length_bytes %d exceeds the 1024 cap", n)
		}
		raw := make([]byte, n)
		if _, err := rand.Read(raw); err != nil {
			return "", opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "entropy: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw), nil

	case wsv1.SecretKind_SECRET_KIND_RSA_PRIVATE_KEY:
		// 2048 rather than a caller-chosen size: an OIDC provider signs
		// ID tokens with this, every relying party has to accept it, and
		// a size nobody asked for is a compatibility problem rather than
		// a security win. length_bytes is deliberately ignored.
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return "", opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "generating rsa key: %v", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return "", opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "encoding rsa key: %v", err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil

	default:
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "unknown secret kind %s", op.GetKind())
	}
}

// ---------------------------------------------------------------- materializer

// materializeEnv substitutes env VALUES of the form secret/<name> with
// the stored plaintext, for the docker create body only (ADR-W012 A4).
// A dangling reference fails the whole op with NOT_FOUND naming the
// REFERENCE — an error message must never carry a value.
func (a *Agent) materializeEnv(env map[string]string) (map[string]string, *wsv1.OpError) {
	out := make(map[string]string, len(env))
	for k, v := range env {
		name, isRef := strings.CutPrefix(v, secretRefPrefix)
		if !isRef || !secretNameRegexp.MatchString(name) {
			out[k] = v
			continue
		}
		value, err := a.openSecret(name)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND,
					"env %s references secret/%s, which does not exist on this node", k, name)
			}
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "env %s: opening secret/%s: %v", k, name, err)
		}
		out[k] = value
	}
	return out, nil
}
