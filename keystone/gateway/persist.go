package gateway

// Keystone-side persistence (ADR-W003 W2): the embedded CA, the op-signing
// key, and join tokens survive a Keystone restart when Config.DataDir is
// set. Layout in DataDir: ca.crt, ca.key, op-signing.key, op-signing.pub,
// tokens.json. Directory 0700, files 0600, writes are tmp+rename (same
// pattern as wardstone/internal/agent/ops.go's writeManagedFile) so a crash
// mid-write never leaves a torn file behind.
//
// Empty DataDir keeps the pre-W2 in-memory-only behavior unchanged: a fresh
// CA and signing key every process start, and MintJoinToken/tokens.json
// simply not touching disk.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// writeFileAtomic writes data to path via a temp file + rename so readers
// (including this same process on a future restart) never observe a
// partially-written file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".wildstone-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil { // WriteFile's mode is subject to umask; be explicit
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ---------------------------------------------------------------- CA

// loadOrCreateCA loads ca.crt/ca.key from dataDir if both exist, otherwise
// generates a fresh CA and persists it. The bool return reports whether an
// existing CA was loaded (true) or a new one was created (false), for the
// startup log line.
func loadOrCreateCA(dataDir string) (*CA, bool, error) {
	certPath := filepath.Join(dataDir, "ca.crt")
	keyPath := filepath.Join(dataDir, "ca.key")

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		ca, err := loadCA(certPEM, keyPEM)
		if err != nil {
			return nil, false, fmt.Errorf("load CA from %s: %w", dataDir, err)
		}
		return ca, true, nil
	case os.IsNotExist(certErr) && os.IsNotExist(keyErr):
		// Neither exists: create fresh, the expected first-run path.
	case certErr != nil && !os.IsNotExist(certErr):
		return nil, false, fmt.Errorf("read ca.crt: %w", certErr)
	case keyErr != nil && !os.IsNotExist(keyErr):
		return nil, false, fmt.Errorf("read ca.key: %w", keyErr)
	default:
		// One exists without the other: a torn or partially-migrated data
		// dir. Refuse rather than silently minting a new CA that orphans
		// agents pinned to the half-present one.
		return nil, false, fmt.Errorf("data dir %s has ca.crt/ca.key inconsistently present", dataDir)
	}

	ca, err := NewCA()
	if err != nil {
		return nil, false, err
	}
	if err := writeFileAtomic(certPath, ca.PEM(), 0o600); err != nil {
		return nil, false, fmt.Errorf("write ca.crt: %w", err)
	}
	if err := writeFileAtomic(keyPath, ca.KeyPEM(), 0o600); err != nil {
		return nil, false, fmt.Errorf("write ca.key: %w", err)
	}
	return ca, false, nil
}

// ---------------------------------------------------------------- op-signing key

// loadOrCreateSigningKey loads op-signing.key/op-signing.pub (raw bytes, not
// PEM — matching the agent's pinned op-signing.pub format) from dataDir if
// both exist, otherwise generates and persists a fresh ed25519 keypair.
func loadOrCreateSigningKey(dataDir string) (ed25519.PrivateKey, bool, error) {
	pubPath := filepath.Join(dataDir, "op-signing.pub")
	keyPath := filepath.Join(dataDir, "op-signing.key")

	pubB, pubErr := os.ReadFile(pubPath)
	keyB, keyErr := os.ReadFile(keyPath)
	switch {
	case pubErr == nil && keyErr == nil:
		if len(keyB) != ed25519.PrivateKeySize {
			return nil, false, fmt.Errorf("op-signing.key: bad length %d", len(keyB))
		}
		priv := ed25519.PrivateKey(keyB)
		if !bytes.Equal(priv.Public().(ed25519.PublicKey), pubB) {
			return nil, false, fmt.Errorf("op-signing.pub does not match op-signing.key")
		}
		return priv, true, nil
	case os.IsNotExist(pubErr) && os.IsNotExist(keyErr):
		// fall through to create
	case pubErr != nil && !os.IsNotExist(pubErr):
		return nil, false, fmt.Errorf("read op-signing.pub: %w", pubErr)
	case keyErr != nil && !os.IsNotExist(keyErr):
		return nil, false, fmt.Errorf("read op-signing.key: %w", keyErr)
	default:
		return nil, false, fmt.Errorf("data dir %s has op-signing.pub/.key inconsistently present", dataDir)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, err
	}
	if err := writeFileAtomic(keyPath, priv, 0o600); err != nil {
		return nil, false, fmt.Errorf("write op-signing.key: %w", err)
	}
	if err := writeFileAtomic(pubPath, pub, 0o600); err != nil {
		return nil, false, fmt.Errorf("write op-signing.pub: %w", err)
	}
	return priv, false, nil
}

// ---------------------------------------------------------------- join tokens

// tokenRecord is the on-disk representation of one minted join token.
type tokenRecord struct {
	Expires time.Time `json:"expires"`
	Used    bool      `json:"used"`
}

// tokenStore holds minted join tokens (as opposed to Config.JoinTokens'
// static, non-expiring, never-persisted dev tokens). With dataDir == "" it
// behaves purely in-memory. With dataDir set, every mint and every
// mark-used is persisted to tokens.json before the call returns success —
// the crash-safety requirement is that a used token can never come back
// reusable after a crash, so the disk write happens before Join is allowed
// to proceed with issuing a certificate.
type tokenStore struct {
	mu   sync.Mutex
	path string // "" => in-memory only, no disk I/O
	data map[string]*tokenRecord
}

func loadTokenStore(dataDir string) (*tokenStore, error) {
	ts := &tokenStore{data: map[string]*tokenRecord{}}
	if dataDir == "" {
		return ts, nil
	}
	ts.path = filepath.Join(dataDir, "tokens.json")
	b, err := os.ReadFile(ts.path)
	if err != nil {
		if os.IsNotExist(err) {
			return ts, nil
		}
		return nil, fmt.Errorf("read tokens.json: %w", err)
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &ts.data); err != nil {
			return nil, fmt.Errorf("parse tokens.json: %w", err)
		}
	}
	return ts, nil
}

// persist must be called with mu held.
func (ts *tokenStore) persist() error {
	if ts.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(ts.data, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(ts.path, b, 0o600)
}

// mint creates a new single-use join token with the given TTL: 32 random
// bytes, hex-encoded. Persisted (if dataDir is set) before returning.
func (ts *tokenStore) mint(ttl time.Duration) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)

	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.data[token] = &tokenRecord{Expires: time.Now().Add(ttl), Used: false}
	if err := ts.persist(); err != nil {
		delete(ts.data, token)
		return "", fmt.Errorf("persist minted token: %w", err)
	}
	return token, nil
}

// consume validates and marks a token used, atomically from the caller's
// point of view. It returns false for an unknown, already-used, or expired
// token. On success the "used" mark is fsync-durable on disk (via rename)
// before consume returns — Join must not proceed to issue a certificate
// until this returns true, so a crash between consume and cert issuance
// still leaves the token correctly spent.
//
// Expired-but-unused tokens are left in tokens.json as a permanent record
// (not deleted) rather than resurrected or pruned — see W2's report for the
// documented tradeoff: this is a homelab-scale join-token store, not a
// high-churn credential system, so unbounded (but tiny, hex-string) growth
// of stale entries is an acceptable simplification over adding a reaper.
func (ts *tokenStore) consume(token string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	rec, ok := ts.data[token]
	if !ok || rec.Used || time.Now().After(rec.Expires) {
		return false
	}
	rec.Used = true
	if err := ts.persist(); err != nil {
		// Could not durably record the use — refuse rather than risk a
		// token that's usable in memory but not on disk.
		rec.Used = false
		return false
	}
	return true
}
