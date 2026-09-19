// Package fakeidp is a stand-in for Authelia's OpenID Connect provider,
// for tests of the Console sign-in (ADR-W033 A4.1).
//
// It is strict where the real one is: the token endpoint refuses a code
// it did not issue, a second use of a code, a PKCE verifier that does
// not match the challenge, and a redirect_uri or client_id that differs
// from the authorization request. A fake that accepted anything would
// let the relying party's own mistakes through.
package fakeidp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// IdP is one fake provider.
type IdP struct {
	Issuer   string
	ClientID string
	Kid      string
	Key      *rsa.PrivateKey
	Server   *httptest.Server

	// User and AuthTime are what the next token says; Now is its clock.
	User     string
	AuthTime time.Time
	Now      func() time.Time
	// Mutate, when set, edits the claims of the next token before it is
	// signed — how a test forges an issuer, an audience or a time.
	Mutate func(map[string]any)
	// SignWith, when set, signs the next token with this key instead.
	SignWith *rsa.PrivateKey

	mu            sync.Mutex
	codes         map[string]grant
	LastAuthorize url.Values
}

type grant struct {
	nonce, challenge, redirect, client string
}

// New starts a provider that says it is issuer.
func New(t *testing.T, issuer, clientID string) *IdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &IdP{Issuer: issuer, ClientID: clientID, Kid: "k1", Key: key, codes: map[string]grant{},
		Now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 i.Issuer,
			"authorization_endpoint": i.Issuer + "/api/oidc/authorization",
			"token_endpoint":         i.Issuer + "/api/oidc/token",
			"jwks_uri":               i.Issuer + "/jwks.json",
		})
	})
	mux.HandleFunc("GET /jwks.json", func(w http.ResponseWriter, r *http.Request) {
		pub := i.Key.PublicKey
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": i.Kid,
			"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /api/oidc/token", i.token)
	i.Server = httptest.NewServer(mux)
	t.Cleanup(i.Server.Close)
	return i
}

// Client reaches this provider whatever host a URL names, so the relying
// party can be given the real-looking issuer it would use in production.
func (i *IdP) Client() *http.Client {
	target, _ := url.Parse(i.Server.URL)
	return &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		r.Host = target.Host
		return http.DefaultTransport.RoundTrip(r)
	})}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Authorize is the browser at the authorization endpoint, already signed
// in: it records the request and returns the code the provider would
// redirect back with, bound to that request's nonce and challenge.
func (i *IdP) Authorize(t *testing.T, authURL string) (code, state string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("authorization request without S256 PKCE: %s", authURL)
	}
	code = fmt.Sprintf("code-%d", time.Now().UnixNano())
	i.mu.Lock()
	i.LastAuthorize = q
	i.codes[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
		redirect: q.Get("redirect_uri"), client: q.Get("client_id")}
	i.mu.Unlock()
	return code, q.Get("state")
}

func (i *IdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	i.mu.Lock()
	g, ok := i.codes[r.PostForm.Get("code")]
	delete(i.codes, r.PostForm.Get("code"))
	i.mu.Unlock()
	fail := func(why string) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": why})
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case !ok:
		fail("unknown or reused code")
	case r.PostForm.Get("grant_type") != "authorization_code":
		fail("grant_type")
	case b64(sum[:]) != g.challenge:
		fail("PKCE verifier does not match")
	case r.PostForm.Get("redirect_uri") != g.redirect || r.PostForm.Get("client_id") != g.client || g.client != i.ClientID:
		fail("client or redirect mismatch")
	default:
		json.NewEncoder(w).Encode(map[string]string{"id_token": i.Mint(g.nonce), "token_type": "Bearer"})
	}
}

// Mint signs an ID token as the provider would for this nonce.
func (i *IdP) Mint(nonce string) string {
	now := i.Now()
	claims := map[string]any{
		"iss": i.Issuer, "aud": []string{i.ClientID}, "sub": "0f5a-uuid",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
		"auth_time": i.AuthTime.Unix(), "nonce": nonce, "preferred_username": i.User,
	}
	if i.Mutate != nil {
		i.Mutate(claims)
	}
	key := i.Key
	if i.SignWith != nil {
		key = i.SignWith
	}
	return Sign(key, map[string]any{"alg": "RS256", "kid": i.Kid, "typ": "JWT"}, claims)
}

// Sign builds a compact JWS with RS256 whatever the header claims — a
// header naming another algorithm is how a test checks it is refused.
func Sign(key *rsa.PrivateKey, header, claims map[string]any) string {
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	signing := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signing + "." + b64(sig)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
