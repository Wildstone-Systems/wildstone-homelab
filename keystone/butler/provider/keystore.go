package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// Provider keys entered in the Console (ADR-W017 A10).
//
// A hosted provider's key used to come only from keystone's environment,
// which meant an Owner had to reach the machine's shell to connect
// OpenAI. A10 keeps what that rule was protecting — a key is never read
// back, never stored in the runtime DB, the Statefile or a plan, never
// logged, never returned — and lets the Console WRITE one:
//
//   - it lives in one root-only file (0600) in keystone's data directory,
//     outside the database, the Statefile and every backup;
//   - the API accepts it and never returns it: status is "set", and from
//     where;
//   - an environment variable still wins, so an operator who manages keys
//     in keystone.env is never overruled from a browser.
//
// The store is process-wide because the key is read where a provider
// client is built, and that happens in more than one place (the Butler
// factory, the probe, the model listing). KeyGeneration moves on every
// change so a cached client built with the old key is not reused.

// ErrNoKeyStore is a write with no key file configured.
var ErrNoKeyStore = errors.New("provider keys: no key file is configured on this keystone")

type keyStore struct {
	path string
	mu   sync.RWMutex
	keys map[string]string
}

var (
	store      atomic.Pointer[keyStore]
	generation atomic.Uint64
)

// UseKeyFile points the process at its key file and loads it. A missing
// file is an empty store, not an error.
func UseKeyFile(path string) error {
	ks := &keyStore{path: path, keys: map[string]string{}}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("provider keys: reading %s: %w", path, err)
	default:
		if err := json.Unmarshal(raw, &ks.keys); err != nil {
			return fmt.Errorf("provider keys: %s is not a key file: %w", path, err)
		}
	}
	store.Store(ks)
	generation.Add(1)
	return nil
}

// KeyGeneration changes whenever a stored key does.
func KeyGeneration() uint64 { return generation.Load() }

func storedKey(name string) string {
	ks := store.Load()
	if ks == nil {
		return ""
	}
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return ks.keys[name]
}

// KeySource says where a kind's key comes from: "environment",
// "console", or "" for none. The environment wins.
func (k Kind) KeySource() string {
	switch {
	case k.KeyEnv != "" && strings.TrimSpace(os.Getenv(k.KeyEnv)) != "":
		return "environment"
	case storedKey(k.Name) != "":
		return "console"
	default:
		return ""
	}
}

// SetKey stores a kind's key. The value is checked for shape only — a
// key has no whitespace and a sane length — and never echoed in an error.
func SetKey(k Kind, value string) error {
	if k.KeyEnv == "" {
		return fmt.Errorf("%s takes no key", k.Label)
	}
	value = strings.TrimSpace(value)
	if len(value) < 8 || len(value) > 4096 || strings.ContainsAny(value, " \t\r\n") {
		return fmt.Errorf("that does not look like a %s key (it should be one unbroken string)", k.Label)
	}
	return writeKeys(func(keys map[string]string) { keys[k.Name] = value })
}

// RemoveKey forgets a kind's stored key. The environment is untouched.
func RemoveKey(k Kind) error {
	return writeKeys(func(keys map[string]string) { delete(keys, k.Name) })
}

func writeKeys(change func(map[string]string)) error {
	ks := store.Load()
	if ks == nil {
		return ErrNoKeyStore
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	next := make(map[string]string, len(ks.keys)+1)
	for n, v := range ks.keys {
		next[n] = v
	}
	change(next)
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	// Written beside the target at 0600 and renamed over it, so a crash
	// leaves the old file or the new one — never half of either, and
	// never a moment when the file exists with a wider mode.
	tmp, err := os.CreateTemp(filepath.Dir(ks.path), ".provider-keys-*")
	if err != nil {
		return fmt.Errorf("provider keys: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("provider keys: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("provider keys: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("provider keys: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("provider keys: %w", err)
	}
	if err := os.Rename(tmp.Name(), ks.path); err != nil {
		return fmt.Errorf("provider keys: %w", err)
	}
	ks.keys = next
	generation.Add(1)
	return nil
}
