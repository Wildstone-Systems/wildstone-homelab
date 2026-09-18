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

// IssueChatClaim mints one claim code admitting identity to one
// Connection and returns the secret — the only moment it exists outside
// the caller's hands.
//
// The Connection, not a surface: a code admits somebody to ONE Agent's
// bot (ADR-W033 B7). It used to name a surface and, optionally, "any
// transport", which between them could not say which Agent the person
// would end up talking to.
func (s *Store) IssueChatClaim(at runtimedb.Connection, identity, issuedBy string, ttl time.Duration) (string, runtimedb.ChatClaimCode, error) {
	if at.Seq == 0 {
		return "", runtimedb.ChatClaimCode{}, fmt.Errorf("principal: a claim code names the connection it admits somebody to")
	}
	if strings.TrimSpace(identity) == "" {
		return "", runtimedb.ChatClaimCode{}, fmt.Errorf("principal: a claim code must name the identity it binds")
	}
	// A Connection admits a PERSON (ADR-W033 B9, design §8.3), on either
	// surface. This used to require a Keystone principal for an
	// admin-surface code and take a family identity as given, which is
	// the model W033 supersedes: every external identity — directory
	// login, web session, Telegram, Discord — attaches to the same
	// canonical Person (A2), and the adapter's own credential
	// authenticates the bot rather than naming who speaks through it.
	//
	// Checked at issue as well as at use because B9 says both:
	// "Configuration and runtime both enforce this." A code is
	// configuration, and the first thing an operator should learn about
	// a typo is not that their bot answers nobody.
	m, err := s.DB.GetButlerMember(identity)
	if err != nil {
		return "", runtimedb.ChatClaimCode{},
			fmt.Errorf("principal: claim for %q, who is not a household member: %w", identity, err)
	}
	if !m.Active() {
		return "", runtimedb.ChatClaimCode{},
			fmt.Errorf("principal: %q is not an admitted household member (ADR-W030 A2.1)", identity)
	}
	// An Owner-Agent Connection admits Owners only (B9), and the
	// Connection now says which Agent it belongs to, so the class comes
	// from that Agent rather than from a surface the binding named.
	if at.AdmitsOwnersOnly() && m.Role != runtimedb.RoleOwner {
		return "", runtimedb.ChatClaimCode{}, fmt.Errorf(
			"principal: %q is a member, and %s is an owner agent, whose connections admit owners only (ADR-W033 B9)",
			identity, at.Butler)
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
		Connection: at.Seq,
		Identity:   identity,
		CreatedBy:  issuedBy,
		ExpiresAt:  time.Now().Add(ttl),
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
func (s *Store) RedeemChatClaim(code string, at runtimedb.Connection, account, displayName, conversation string) (runtimedb.ChatBinding, error) {
	return s.DB.RedeemChatClaimCode(hashSecret(NormalizeClaimCode(code)), at, account, displayName, conversation)
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
