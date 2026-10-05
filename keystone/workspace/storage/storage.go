// Package storage holds inert immutable workspace bytes (ADR-W047 S04).
// It is not an authorization or metadata service: callers must resolve current
// Person/Agent authority, reserve capacity and journal effects before Publish.
// A successful Publish means bytes exist, never that an artifact is committed.
package storage

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/internal/confinedpath"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
)

// ErrUncertain retains the object for journal reconciliation after a link whose
// directory durability could not be confirmed. Never retry using a new ID.
var ErrUncertain = errors.New("storage_unavailable: byte publication requires recovery; durability is uncertain")

var (
	ErrPath        = errors.New("invalid_input: unsafe workspace path or identity")
	ErrFormat      = errors.New("unsupported: workspace requires inert UTF-8 md, txt, json or csv bytes")
	ErrSize        = errors.New("invalid_input: workspace file exceeds configured limit")
	ErrConflict    = errors.New("conflict: workspace byte destination exists")
	ErrIntegrity   = errors.New("storage_unavailable: workspace byte integrity check failed")
	ErrUnsupported = errors.New("unsupported: filesystem lacks required confinement or durable no-overwrite operations")
	ErrFence       = errors.New("context_missing: publication requires an execution fence")
)

// Object identifies bytes, not an artifact revision or bearer authority. Keep
// this tuple in the trusted publication journal; never trust a model's hash.
type Object struct {
	ID     string
	SHA256 string
	Size   int64
}

// Store is scoped by stable trusted owner IDs, never display names or model
// input. Sharing is metadata authority over these same bytes, not another root.
// Settings is a per-operation snapshot; callers reload authoritative settings.
type Store struct {
	settings  workspace.Settings
	namespace string
	// Fault seams are private so production cannot disable confinement.
	hooks hooks
}

func New(settings workspace.Settings, personID, agentID string) (*Store, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	for _, id := range []string{personID, agentID} {
		if id == "" || len(id) > 128 || !utf8.ValidString(id) {
			return nil, ErrPath
		}
	}
	// Length-delimited independent hashes prevent ambiguous identities and keep
	// raw IDs out of directory names. IDs remain selectors, never permissions.
	p := sha256.Sum256([]byte(personID))
	a := sha256.Sum256([]byte(agentID))
	return &Store{settings: settings, namespace: hex.EncodeToString(p[:]) + "/" + hex.EncodeToString(a[:])}, nil
}

// ValidatePath shares the exact W037 lexical rules. Empty paths are only valid
// for directory roots; bytes additionally require an allowlisted extension.
func ValidatePath(raw string, directory bool) error {
	p, err := confinedpath.ParseRelativePath(raw)
	if err != nil || (!directory && p == "") {
		return ErrPath
	}
	if directory {
		return nil
	}
	switch path.Ext(raw) {
	case ".md", ".txt", ".json", ".csv":
		return nil
	}
	return ErrFormat
}

func validObject(o Object, max int64) bool {
	if !validID(o.ID) || len(o.SHA256) != 64 || o.Size < 0 || o.Size > max {
		return false
	}
	b, err := hex.DecodeString(o.SHA256)
	return err == nil && hex.EncodeToString(b) == o.SHA256
}
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	b, err := hex.DecodeString(id)
	return err == nil && hex.EncodeToString(b) == id
}
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", workspace.ErrUnavailable
	}
	return hex.EncodeToString(b[:]), nil
}
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func readText(r io.Reader, max int64) ([]byte, error) {
	if r == nil {
		return nil, ErrFormat
	}
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, workspace.ErrUnavailable
	}
	if int64(len(data)) > max {
		return nil, ErrSize
	}
	if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return nil, ErrFormat
	}
	return data, nil
}
