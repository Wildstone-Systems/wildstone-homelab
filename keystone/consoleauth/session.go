package consoleauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"sync"
	"time"
)

// The default windows (ADR-W033 A4.1). Both are configurable on
// keystone's command line; these are what a household gets by default.
const (
	// DefaultEntryWindow is how old an authentication may be for its
	// holder to stay in the Console.
	DefaultEntryWindow = 12 * time.Hour
	// DefaultStepUpWindow is how old it may be for a consequential
	// operation.
	DefaultStepUpWindow = 15 * time.Minute
)

// pendingTTL bounds a sign-in that was started and never finished.
const pendingTTL = 10 * time.Minute

// maxEntries caps each table. A household has a handful of Owners; the
// cap exists so that a flood of started-and-abandoned sign-ins cannot
// grow memory without bound, not because a real household approaches it.
const maxEntries = 4096

// Session is one Console sign-in.
//
// Held in memory, deliberately. Restarting Keystone signs everybody out
// of the Console, which costs an Owner one redirect through a family
// session Authelia still holds — and in exchange a Console session is
// never written anywhere a backup or a copied data directory could carry
// it.
type Session struct {
	User     string
	AuthTime time.Time
}

// Pending is a sign-in in progress: what the callback must see again.
type Pending struct {
	Nonce    string
	Verifier string
	// User is who the trusted ingress said was signing in when it began.
	// The token that comes back must name the same account.
	User string
	// Return is the Console path to land on afterwards.
	Return string
	// Browser is the value of the cookie set when the sign-in began,
	// binding the callback to the browser that started it.
	Browser string
	created time.Time
}

// Store holds pending sign-ins and live sessions.
type Store struct {
	Now func() time.Time

	mu       sync.Mutex
	pending  map[string]Pending
	sessions map[string]Session
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Token is 32 random bytes, URL-safe.
func Token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on a supported platform; if it ever
		// did, an empty or predictable token must not be the result.
		panic("consoleauth: no randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Challenge is the PKCE S256 challenge for a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Begin records a sign-in and returns its state.
func (s *Store) Begin(p Pending) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		s.pending = map[string]Pending{}
	}
	now := s.now()
	for k, v := range s.pending {
		if now.Sub(v.created) > pendingTTL {
			delete(s.pending, k)
		}
	}
	if len(s.pending) >= maxEntries {
		// Full of live sign-ins nobody finished: drop them all rather
		// than refuse the Owner standing at the door.
		s.pending = map[string]Pending{}
	}
	state := Token()
	p.created = now
	s.pending[state] = p
	return state
}

// Take returns a pending sign-in and forgets it: a state is good once.
func (s *Store) Take(state string) (Pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[state]
	delete(s.pending, state)
	if !ok || s.now().Sub(p.created) > pendingTTL {
		return Pending{}, false
	}
	return p, true
}

// Create starts a session and returns its id.
func (s *Store) Create(sess Session, window time.Duration) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]Session{}
	}
	now := s.now()
	for k, v := range s.sessions {
		if !Fresh(v.AuthTime, now, window) {
			delete(s.sessions, k)
		}
	}
	if len(s.sessions) >= maxEntries {
		s.sessions = map[string]Session{}
	}
	id := Token()
	s.sessions[id] = sess
	return id
}

// Lookup finds a session by id.
func (s *Store) Lookup(id string) (Session, bool) {
	if id == "" {
		return Session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

// End forgets one session.
func (s *Store) End(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// EndUser forgets every session a user holds (A6: demotion invalidates
// active Console sessions). Matched case-insensitively, like every
// Console identity check.
func (s *Store) EndUser(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.sessions {
		if strings.EqualFold(v.User, user) {
			delete(s.sessions, k)
		}
	}
}

// Fresh reports whether an authentication at authTime is inside window
// at now. An authentication in the future is not fresh: a clock that
// disagrees is a reason to refuse, never a reason to extend.
func Fresh(authTime, now time.Time, window time.Duration) bool {
	if authTime.IsZero() || authTime.After(now) {
		return false
	}
	return now.Sub(authTime) <= window
}
