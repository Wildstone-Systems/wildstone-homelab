package principal

// Chat claim codes (ADR-W026 A4): the one-time secret that turns a chat
// account into a bound identity.
//
// A claim code is deliberately shaped like nothing else in Keystone. It
// is short enough to read aloud and type into a chat window on a phone,
// it lives for minutes rather than forever, it works once, and it can
// only ever produce the ONE binding it names — a code issued for
// "family, member sam" cannot bind anybody to the admin surface however
// it is presented. Like every other secret here, only its hash is
// stored (ADR-W007 A3), so a database read cannot replay one.

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

const (
	// DefaultClaimTTL is how long an issued code stays redeemable. Long
	// enough to walk to somebody and read it out, short enough that a
	// code left in a chat log is worthless by the time anyone finds it.
	DefaultClaimTTL = 15 * time.Minute
	// MaxClaimTTL bounds what a caller may ask for.
	MaxClaimTTL = 24 * time.Hour
	// claimCodeGroups/claimCodeGroupLen shape the code as three groups of
	// four, e.g. K7QP-3XMR-9TWD.
	claimCodeGroups   = 3
	claimCodeGroupLen = 4
)

// claimAlphabet excludes the characters people confuse when reading a
// code off a screen: 0/O, 1/I/L, and the vowels that let a random code
// spell something unfortunate.
const claimAlphabet = "23456789BCDFGHJKMNPQRSTVWXYZ"

// IssueChatClaim mints one claim code for identity on surface and
// returns the secret — the only moment it exists outside the caller's
// hands. transport "" lets the code be redeemed on any transport;
// naming one narrows it further.
func (s *Store) IssueChatClaim(surface runtimedb.ButlerSurface, identity, transport, issuedBy string, ttl time.Duration) (string, runtimedb.ChatClaimCode, error) {
	if !runtimedb.ValidButlerSurface(surface) {
		return "", runtimedb.ChatClaimCode{}, fmt.Errorf("principal: unknown surface %q (admin|family)", surface)
	}
	if strings.TrimSpace(identity) == "" {
		return "", runtimedb.ChatClaimCode{}, fmt.Errorf("principal: a claim code must name the identity it binds")
	}
	// An admin-surface code binds a chat account to a KEYSTONE PRINCIPAL,
	// so the principal has to exist and be enabled — otherwise the first
	// thing the operator learns about a typo is that their bot answers
	// nothing. A family code names a household member, which is an LDAP
	// account this control plane does not own, so it is taken as given.
	if surface == runtimedb.ButlerSurfaceAdmin {
		p, err := s.DB.GetPrincipal(identity)
		if err != nil {
			return "", runtimedb.ChatClaimCode{}, fmt.Errorf("principal: admin claim for unknown principal %q: %w", identity, err)
		}
		if p.Disabled {
			return "", runtimedb.ChatClaimCode{}, fmt.Errorf("principal: %q is disabled", identity)
		}
	}
	if ttl <= 0 {
		ttl = DefaultClaimTTL
	}
	if ttl > MaxClaimTTL {
		return "", runtimedb.ChatClaimCode{}, fmt.Errorf("principal: claim ttl %s exceeds the maximum %s", ttl, MaxClaimTTL)
	}
	code, err := newClaimCode()
	if err != nil {
		return "", runtimedb.ChatClaimCode{}, err
	}
	rec := runtimedb.ChatClaimCode{
		Surface:   surface,
		Identity:  identity,
		Transport: transport,
		CreatedBy: issuedBy,
		ExpiresAt: time.Now().Add(ttl),
	}
	if err := s.DB.InsertChatClaimCode(hashSecret(code), rec); err != nil {
		return "", runtimedb.ChatClaimCode{}, err
	}
	return code, rec, nil
}

// RedeemChatClaim exchanges a presented code for a binding. Every
// failure — unknown, expired, spent, wrong transport — comes back as
// runtimedb.ErrClaimCode, so a caller trying codes learns nothing from
// which one it was.
func (s *Store) RedeemChatClaim(code, transport, account, displayName, conversation string) (runtimedb.ChatBinding, error) {
	return s.DB.RedeemChatClaimCode(hashSecret(NormalizeClaimCode(code)), transport, account, displayName, conversation)
}

// NormalizeClaimCode makes a typed code match an issued one: people add
// spaces, drop the dashes, and use lower case. None of that should be
// the difference between a working code and a mysterious refusal.
func NormalizeClaimCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		if strings.ContainsRune(claimAlphabet, r) {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) != claimCodeGroups*claimCodeGroupLen {
		// Return the cleaned form anyway: a wrong-length code simply
		// will not match a stored hash, which is the same refusal as any
		// other bad code.
		return out
	}
	groups := make([]string, 0, claimCodeGroups)
	for i := 0; i < claimCodeGroups; i++ {
		groups = append(groups, out[i*claimCodeGroupLen:(i+1)*claimCodeGroupLen])
	}
	return strings.Join(groups, "-")
}

// newClaimCode draws a code from crypto/rand with rejection sampling, so
// every character is uniform over the alphabet rather than biased by a
// modulo.
func newClaimCode() (string, error) {
	n := claimCodeGroups * claimCodeGroupLen
	chars := make([]byte, 0, n)
	buf := make([]byte, 1)
	limit := byte(256 - (256 % len(claimAlphabet)))
	for len(chars) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("principal: generating claim code: %w", err)
		}
		if buf[0] >= limit {
			continue
		}
		chars = append(chars, claimAlphabet[int(buf[0])%len(claimAlphabet)])
	}
	groups := make([]string, 0, claimCodeGroups)
	for i := 0; i < claimCodeGroups; i++ {
		groups = append(groups, string(chars[i*claimCodeGroupLen:(i+1)*claimCodeGroupLen]))
	}
	return strings.Join(groups, "-"), nil
}
