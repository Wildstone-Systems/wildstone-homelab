package extract

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/research/publicfetch"
)

const (
	PageBytes = 2500
	// Reserve 500 bytes for the gateway envelope and later budget metadata.
	// Activation must still measure the final envelope after redaction.
	BodyBytes  = 3500
	cacheTTL   = 15 * time.Minute
	maxSources = 128
)

var (
	ErrInput    = errors.New("invalid_input: invalid source request")
	ErrDenied   = errors.New("permission_denied: source access refused")
	ErrStale    = errors.New("stale_cursor: source expired or unavailable")
	ErrCapacity = errors.New("quota_exceeded: extracted source cache full")
)

// Scope is trusted context, never model arguments. PolicyRevision binds the
// provenance/configuration snapshot used for extraction.
type Scope struct{ Person, Agent, Task, Resource, PolicyRevision string }

func (s Scope) valid() bool {
	for _, v := range []string{s.Person, s.Agent, s.Task, s.Resource, s.PolicyRevision} {
		if len(v) == 0 || len(v) > 128 || !utf8.ValidString(v) {
			return false
		}
	}
	return true
}

// Authorize must recheck current grants and provenance, including revocation.
// It runs before every insertion/lookup; cache hits confer no authority.
type Authorize func(context.Context, Scope) error

type cached struct {
	scope   Scope
	doc     Document
	expires time.Time
}

// Store is a bounded process-local immutable text cache, not durable artifact
// storage or a search cache. Restart invalidates refs/cursors. It retains no raw
// bodies/files. Later gateway wiring owns authorization and provenance storage.
type Store struct {
	mu        sync.Mutex
	sources   map[string]cached
	key       [32]byte
	authorize Authorize
	now       func() time.Time
}

func NewStore(authorize Authorize) (*Store, error) {
	if authorize == nil {
		return nil, ErrDenied
	}
	s := &Store{sources: make(map[string]cached), authorize: authorize, now: time.Now}
	if _, err := rand.Read(s.key[:]); err != nil {
		return nil, err
	}
	return s, nil
}

type Page struct {
	Text       string     `json:"text"`
	NextCursor string     `json:"nextCursor,omitempty"`
	Truncated  bool       `json:"truncated"`
	Source     SourceInfo `json:"source"`
}

// Encode produces the contract body; it still requires the canonical untrusted
// gateway envelope. This is not a complete InvokeResult.
func (p Page) Encode() ([]byte, error) {
	return json.Marshal(struct {
		Version int  `json:"version"`
		Data    Page `json:"data"`
	}{1, p})
}
func (s *Store) allowed(ctx context.Context, scope Scope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !scope.valid() {
		return ErrInput
	}
	if s.authorize == nil || s.authorize(ctx, scope) != nil {
		return ErrDenied
	}
	return ctx.Err()
}

// Put publishes a fresh immutable extraction. Re-fetching a URL never changes
// existing sources or renews their retrieval date/expiry.
func (s *Store) Put(ctx context.Context, scope Scope, input publicfetch.Page) (Page, error) {
	if err := s.allowed(ctx, scope); err != nil {
		return Page{}, err
	}
	doc, err := Extract(input)
	if err != nil {
		return Page{}, err
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return Page{}, err
	}
	doc.Source.SourceID = hex.EncodeToString(id[:])
	provenance, _ := json.Marshal(struct {
		Scope    Scope
		Document Document
	}{scope, doc})
	digest := sha256.Sum256(provenance)
	doc.Source.Revision = hex.EncodeToString(digest[:])
	c := cached{scope: scope, doc: doc, expires: s.now().Add(cacheTTL)}
	first, err := s.page(c, 0)
	if err != nil {
		return Page{}, err
	}
	if err = s.allowed(ctx, scope); err != nil {
		return Page{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, c := range s.sources {
		if !s.now().Before(c.expires) {
			delete(s.sources, key)
		}
	}
	if len(s.sources) >= maxSources {
		return Page{}, ErrCapacity
	}
	s.sources[doc.Source.SourceID] = c
	return first, nil
}

// Read never refetches. A later cursor-only tool adapter must resolve the
// immutable source, not turn a cursor into a URL fetch. Selectors grant nothing.
func (s *Store) Read(ctx context.Context, scope Scope, ref SourceRef, cursor string) (Page, error) {
	if err := s.allowed(ctx, scope); err != nil {
		return Page{}, err
	}
	if len(ref.SourceID) != 32 || len(ref.Revision) != 64 {
		return Page{}, ErrInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.sources[ref.SourceID]
	if !ok || c.scope != scope {
		return Page{}, ErrStale
	}
	if !s.now().Before(c.expires) {
		delete(s.sources, ref.SourceID)
		return Page{}, ErrStale
	}
	if ref != c.doc.Source.SourceRef {
		return Page{}, ErrInput
	}
	offset := 0
	if cursor != "" {
		if len(cursor) > 512 {
			return Page{}, ErrInput
		}
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || len(b) != 52 || base64.RawURLEncoding.EncodeToString(b) != cursor {
			return Page{}, ErrInput
		}
		mac := hmac.New(sha256.New, s.key[:])
		mac.Write(b[:20])
		mac.Write([]byte(ref.Revision))
		if !hmac.Equal(mac.Sum(nil), b[20:]) || hex.EncodeToString(b[:16]) != ref.SourceID {
			return Page{}, ErrInput
		}
		offset = int(binary.BigEndian.Uint32(b[16:20]))
		if offset <= 0 || offset >= len(c.doc.Text) || !utf8.RuneStart(c.doc.Text[offset]) {
			return Page{}, ErrInput
		}
	}
	return s.page(c, offset)
}
func (s *Store) cursor(c cached, offset int) string {
	b := make([]byte, 20, 52)
	id, _ := hex.DecodeString(c.doc.Source.SourceID)
	copy(b, id)
	binary.BigEndian.PutUint32(b[16:], uint32(offset))
	mac := hmac.New(sha256.New, s.key[:])
	mac.Write(b)
	mac.Write([]byte(c.doc.Source.Revision))
	return base64.RawURLEncoding.EncodeToString(append(b, mac.Sum(nil)...))
}
func (s *Store) page(c cached, offset int) (Page, error) {
	text := c.doc.Text
	end := offset + PageBytes
	if end > len(text) {
		end = len(text)
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	for {
		p := Page{Text: text[offset:end], Truncated: c.doc.Source.Truncated, Source: c.doc.Source}
		if end < len(text) {
			p.NextCursor = s.cursor(c, end)
		}
		encoded, err := p.Encode()
		if err != nil {
			return Page{}, err
		}
		if len(encoded) <= BodyBytes {
			return p, nil
		}
		if end == offset {
			return Page{}, publicfetch.ErrSize
		}
		_, size := utf8.DecodeLastRuneInString(text[offset:end])
		end -= size
		if end == offset && offset < len(text) {
			return Page{}, publicfetch.ErrSize
		}
	}
}

// Continue implements the cursor-only selector without accepting a replacement
// URL. Read authenticates the cursor and immutable revision before disclosure.
func (s *Store) Continue(ctx context.Context, scope Scope, cursor string) (Page, error) {
	if err := s.allowed(ctx, scope); err != nil {
		return Page{}, err
	}
	if len(cursor) > 512 {
		return Page{}, ErrInput
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(b) != 52 {
		return Page{}, ErrInput
	}
	s.mu.Lock()
	c, ok := s.sources[hex.EncodeToString(b[:16])]
	s.mu.Unlock()
	if !ok || c.scope != scope {
		return Page{}, ErrStale
	}
	return s.Read(ctx, scope, c.doc.Source.SourceRef, cursor)
}
