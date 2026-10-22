// Package principal is the public-plane identity model (ADR-W007 A2/A3):
// principals with closed roles, opaque hashed bearer tokens, the
// acting-identity field (ADR-W001 foundation), and the RBAC verb table.
// Storage is keystone/runtimedb; this package owns the semantics.
package principal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// tokenPrefix marks Wildstone bearer secrets so a leaked one is
// recognizable to secret scanners (gitleaks has a custom-prefix rule in
// CI) and unmistakable in support transcripts.
const tokenPrefix = "wst_"

// nameRegexp mirrors the resource-name discipline (ADR-W004): lowercase
// DNS-label-ish, 2–63 chars.
var nameRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,61}[a-z0-9])?$`)

// ErrUnauthenticated is returned for any token that does not resolve to a
// live principal. Callers get one error for absent, malformed, revoked,
// and expired alike: authn failures are deliberately indistinguishable to
// the caller (the audit row records the same).
var ErrUnauthenticated = errors.New("principal: unauthenticated")

// Identity is the authenticated caller a request handler sees.
type Identity struct {
	Name      string         `json:"name"`
	Role      runtimedb.Role `json:"role"`
	ActingFor string         `json:"actingFor,omitempty"`
	// AuthTime is when a person signed in to the Console, for an identity
	// that arrived through the trusted ingress with a Console session
	// (ADR-W033 A4.1). Nil for a bearer token, which has no sign-in to
	// be fresh or stale.
	AuthTime *time.Time `json:"authTime,omitempty"`
}

// Store wraps the runtime DB with the identity semantics.
type Store struct {
	DB *runtimedb.DB
}

// Create registers a new principal after validating the name against the
// resource-name discipline.
func (s *Store) Create(name string, role runtimedb.Role, actingFor string) error {
	if !nameRegexp.MatchString(name) {
		return fmt.Errorf("principal: invalid name %q (want lowercase [a-z0-9._-], 2–63 chars)", name)
	}
	return s.DB.CreatePrincipal(name, role, actingFor)
}

// Mint issues a new bearer token for an existing, enabled principal and
// returns the secret — the only moment it ever exists outside the caller's
// hands (ADR-W007 A3: the DB stores sha256 only). ttl 0 means no expiry.
// actingFor "" inherits the principal's own acting identity.
func (s *Store) Mint(name, actingFor string, ttl time.Duration) (string, error) {
	p, err := s.DB.GetPrincipal(name)
	if err != nil {
		return "", fmt.Errorf("principal: minting for %q: %w", name, err)
	}
	if p.Disabled {
		return "", fmt.Errorf("principal: %q is disabled", name)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("principal: generating token: %w", err)
	}
	secret := tokenPrefix + hex.EncodeToString(raw[:])
	var expires time.Time
	if ttl != 0 {
		expires = time.Now().Add(ttl)
	}
	if err := s.DB.InsertToken(hashSecret(secret), name, actingFor, expires); err != nil {
		return "", err
	}
	return secret, nil
}

// Authenticate resolves a presented bearer secret to an Identity. Failure
// reasons collapse into ErrUnauthenticated by design; the successful path
// stamps last_used.
func (s *Store) Authenticate(secret string) (Identity, error) {
	if !strings.HasPrefix(secret, tokenPrefix) {
		return Identity{}, ErrUnauthenticated
	}
	tok, err := s.DB.GetToken(hashSecret(secret))
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	if !tok.RevokedAt.IsZero() {
		return Identity{}, ErrUnauthenticated
	}
	if !tok.ExpiresAt.IsZero() && time.Now().After(tok.ExpiresAt) {
		return Identity{}, ErrUnauthenticated
	}
	p, err := s.DB.GetPrincipal(tok.Principal)
	if err != nil || p.Disabled {
		return Identity{}, ErrUnauthenticated
	}
	if err := s.DB.TouchUsableStaticToken(tok.Hash, tok.Principal); errors.Is(err, runtimedb.ErrNotFound) {
		return Identity{}, ErrUnauthenticated
	} else if err != nil {
		return Identity{}, fmt.Errorf("principal: touching token: %w", err)
	}
	acting := tok.ActingFor
	if acting == "" {
		acting = p.ActingFor
	}
	return Identity{Name: p.Name, Role: p.Role, ActingFor: acting}, nil
}

// TokenHash returns the database fingerprint for a syntactically valid
// Wildstone bearer. It exists for store-level authorization fences that must
// prove the exact request token is still live in the same transaction as a
// consequential read or write. Callers must authenticate first; this function
// does not establish identity and never persists or logs the bearer.
func TokenHash(secret string) (string, error) {
	if !strings.HasPrefix(secret, tokenPrefix) {
		return "", ErrUnauthenticated
	}
	return hashSecret(secret), nil
}

// Revoke revokes a token by its secret. Admin flows that only hold the
// hash can call runtimedb.RevokeToken directly.
func (s *Store) Revoke(secret string) error {
	return s.DB.RevokeToken(hashSecret(secret))
}

// RevokeOthers revokes every live token held by name except secret: a
// rotation that only lands once the new credential is known to have
// arrived (ADR-W044 A6 pairing), so a failed hand-over leaves the old one
// working.
func (s *Store) RevokeOthers(name, secret string) (int64, error) {
	return s.DB.RevokeTokensForExcept(name, hashSecret(secret))
}

// Bootstrap mints the first admin principal + token when the principals
// table is empty (the ADR-W003 join-token pattern: print once at first
// start, everything after goes through the API). Returns ("", nil) when
// principals already exist.
func (s *Store) Bootstrap() (string, error) {
	existing, err := s.DB.ListPrincipals()
	if err != nil {
		return "", err
	}
	if len(existing) > 0 {
		return "", nil
	}
	if err := s.Create("admin", runtimedb.RoleAdmin, ""); err != nil {
		return "", err
	}
	return s.Mint("admin", "", 0)
}

// EnsureAdmin mints a token for an admin principal, creating that
// principal if the table is empty (ADR-W020 A2).
//
// Bootstrap deliberately returns "" once any principal exists, which is
// correct for a start-up path: a running control plane must not mint a
// second admin every time it restarts. It is also what made admin access
// unrecoverable — the token existed only in the journal, and scrubbing
// the journal a beat early left destroying the runtime DB as the only
// way back (F44). That blocked automated setup three times during
// v0.1.6-alpha testing and again during v0.1.9-alpha testing, where a
// journal surviving a wipe returned tokens from three different installs
// and the first one silently failed.
//
// This is the offline escape hatch. It is deliberately NOT reachable
// from the API: the caller must be able to open the data directory,
// which means root on the host, which is the same proof of physical
// access join tokens already rest on (ADR-W003). Nothing new is trusted;
// an existing assumption is simply made recoverable.
func (s *Store) EnsureAdmin(name string) (string, error) {
	return s.EnsureAdminFor(name, 0)
}

// EnsureAdminFor is EnsureAdmin with a token lifetime; 0 means no expiry.
func (s *Store) EnsureAdminFor(name string, ttl time.Duration) (string, error) {
	if name == "" {
		name = "admin"
	}
	if _, err := s.DB.GetPrincipal(name); err != nil {
		if !errors.Is(err, runtimedb.ErrNotFound) {
			return "", err
		}
		if err := s.Create(name, runtimedb.RoleAdmin, ""); err != nil {
			return "", err
		}
	}
	return s.Mint(name, "", ttl)
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
