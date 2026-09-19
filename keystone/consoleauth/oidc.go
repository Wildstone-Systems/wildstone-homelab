// Package consoleauth is how the Console learns WHEN somebody signed in
// (ADR-W033 A4.1).
//
// The Console has always known WHO: the trusted ingress forwards
// Remote-User and Remote-Groups from Authelia's forward-auth. It could
// not know when, because forward-auth has no header for it — and
// "Console requires fresh or recent step-up authentication even when the
// household SSO session is still valid" (A4) is a statement about when.
//
// So the Console is also an OpenID Connect client of Authelia. The ID
// token Authelia issues carries auth_time, signed with Authelia's key,
// and Keystone verifies that signature itself against the published
// JWKS. Nothing in a request header can make a session fresh: the only
// source of freshness is a token Keystone checked.
//
// Deliberately small, and standard-library only, like the rest of this
// module. It implements exactly the relying-party half Keystone needs —
// authorization code with PKCE (S256), a public client (Keystone holds no
// secret values), RS256 ID tokens — and refuses everything else rather
// than supporting it half-way.
package consoleauth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Skew is how far ahead of Keystone's clock a token's times may be
// before they are refused. Clocks on one host agree; this absorbs the
// seconds between Authelia stamping a token and Keystone reading it, and
// nothing more. It is NOT added to a freshness window: a future-dated
// auth_time is clamped to now, never allowed to extend a session.
const Skew = time.Minute

// Provider is Authelia, as the Console's relying party sees it.
type Provider struct {
	Issuer      string
	ClientID    string
	RedirectURL string
	HTTP        *http.Client
	Now         func() time.Time

	mu        sync.Mutex
	disc      discovery
	discAt    time.Time
	keys      map[string]*rsa.PublicKey
	keysAt    time.Time
	keysTried time.Time
}

type discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Provider) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// discover reads the provider's metadata, cached for an hour.
func (p *Provider) discover(ctx context.Context) (discovery, error) {
	p.mu.Lock()
	if p.disc.TokenEndpoint != "" && p.now().Sub(p.discAt) < time.Hour {
		d := p.disc
		p.mu.Unlock()
		return d, nil
	}
	p.mu.Unlock()
	var d discovery
	if err := p.getJSON(ctx, strings.TrimRight(p.Issuer, "/")+"/.well-known/openid-configuration", &d); err != nil {
		return discovery{}, fmt.Errorf("reading the sign-in provider's configuration: %w", err)
	}
	// The issuer a provider names must be the one it was asked as. A
	// mismatch is how a token minted for somewhere else gets accepted.
	if d.Issuer != p.Issuer {
		return discovery{}, fmt.Errorf("the sign-in provider calls itself %q, not %q", d.Issuer, p.Issuer)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.JWKSURI == "" {
		return discovery{}, errors.New("the sign-in provider's configuration is incomplete")
	}
	p.mu.Lock()
	p.disc, p.discAt = d, p.now()
	p.mu.Unlock()
	return d, nil
}

func (p *Provider) getJSON(ctx context.Context, u string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
}

// AuthURL is where the browser is sent to sign in.
//
// maxAge bounds how old Authelia's own authentication may be before it
// asks again; forceLogin asks again regardless (prompt=login), which is
// the step-up. A household SSO session lasts weeks (A4.1), so entering
// the Console passes the Console's window as max_age: without it a
// month-old family sign-in would be stamped straight into the Console.
func (p *Provider) AuthURL(ctx context.Context, state, nonce, challenge string, maxAge time.Duration, forceLogin bool) (string, error) {
	d, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.ClientID},
		"redirect_uri":          {p.RedirectURL},
		"scope":                 {"openid profile"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	if forceLogin {
		q.Set("prompt", "login")
		q.Set("max_age", "0")
	} else if maxAge > 0 {
		q.Set("max_age", fmt.Sprint(int64(maxAge/time.Second)))
	}
	sep := "?"
	if strings.Contains(d.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return d.AuthorizationEndpoint + sep + q.Encode(), nil
}

// Exchange trades an authorization code for its ID token. A public
// client sends no secret; the PKCE verifier is what proves this is the
// party that started the sign-in.
func (p *Provider) Exchange(ctx context.Context, code, verifier string) (string, error) {
	d, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.RedirectURL},
		"client_id":     {p.ClientID},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("exchanging the sign-in code: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("exchanging the sign-in code: %w", err)
	}
	if resp.StatusCode != http.StatusOK || out.IDToken == "" {
		return "", fmt.Errorf("the sign-in provider refused the code: %s %s", out.Error, out.Desc)
	}
	return out.IDToken, nil
}

// Claims is what the Console takes from a verified ID token.
type Claims struct {
	// User is preferred_username: the directory account, which is what
	// the trusted ingress forwards as Remote-User and what the session is
	// bound to.
	User string
	// AuthTime is when the person last authenticated with Authelia —
	// never later than now (see Skew).
	AuthTime time.Time
}

var (
	// ErrToken is any ID token that did not verify. The reason is in the
	// wrapped message for the log; the browser is told only to sign in
	// again.
	ErrToken = errors.New("sign-in token refused")
)

// Verify checks an ID token end to end and returns its claims.
//
// Every check is required and none has a fallback: an algorithm other
// than RS256, an unknown key, a signature that does not verify, another
// issuer, another audience, an expired token, a nonce that is not the
// one this sign-in sent, a missing or future-dated auth_time, or no
// username. Accepting "alg: none", or a key named in the token itself,
// is how JWT verifiers have historically been talked into trusting what
// the caller wrote.
func (p *Provider) Verify(ctx context.Context, raw, nonce string) (Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("%w: not a signed token", ErrToken)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return Claims{}, fmt.Errorf("%w: header: %v", ErrToken, err)
	}
	if hdr.Alg != "RS256" {
		return Claims{}, fmt.Errorf("%w: algorithm %q is not RS256", ErrToken, hdr.Alg)
	}
	key, err := p.key(ctx, hdr.Kid)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrToken, err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: signature encoding", ErrToken)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return Claims{}, fmt.Errorf("%w: signature does not verify", ErrToken)
	}

	var c struct {
		Iss      string          `json:"iss"`
		Aud      json.RawMessage `json:"aud"`
		Azp      string          `json:"azp"`
		Exp      *float64        `json:"exp"`
		Iat      *float64        `json:"iat"`
		Nonce    string          `json:"nonce"`
		AuthTime *float64        `json:"auth_time"`
		Username string          `json:"preferred_username"`
	}
	if err := decodeSegment(parts[1], &c); err != nil {
		return Claims{}, fmt.Errorf("%w: claims: %v", ErrToken, err)
	}
	now := p.now()
	switch {
	case c.Iss != p.Issuer:
		return Claims{}, fmt.Errorf("%w: issuer %q", ErrToken, c.Iss)
	case !audienceIs(c.Aud, c.Azp, p.ClientID):
		return Claims{}, fmt.Errorf("%w: audience is not %s", ErrToken, p.ClientID)
	case c.Exp == nil || !now.Before(unix(*c.Exp)):
		return Claims{}, fmt.Errorf("%w: expired", ErrToken)
	case c.Iat == nil || unix(*c.Iat).After(now.Add(Skew)):
		return Claims{}, fmt.Errorf("%w: issued in the future", ErrToken)
	case nonce == "" || c.Nonce != nonce:
		return Claims{}, fmt.Errorf("%w: nonce does not match this sign-in", ErrToken)
	case c.AuthTime == nil:
		return Claims{}, fmt.Errorf("%w: no auth_time", ErrToken)
	case unix(*c.AuthTime).After(now.Add(Skew)):
		// A future-dated authentication would stay "fresh" for as long as
		// it is in the future plus the window. Refused outright rather
		// than clamped: a clock that far out is not one to trust.
		return Claims{}, fmt.Errorf("%w: auth_time is in the future", ErrToken)
	case strings.TrimSpace(c.Username) == "":
		return Claims{}, fmt.Errorf("%w: no preferred_username", ErrToken)
	}
	at := unix(*c.AuthTime)
	if at.After(now) {
		at = now // within Skew: clamped, so it can never extend a window
	}
	return Claims{User: strings.TrimSpace(c.Username), AuthTime: at}, nil
}

func unix(f float64) time.Time { return time.Unix(int64(f), 0) }

func decodeSegment(seg string, into any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// audienceIs accepts aud as a string or an array. With more than one
// audience the token must also name this client as its authorized party,
// as OIDC Core 3.1.3.7 requires.
func audienceIs(raw json.RawMessage, azp, client string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == client
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return false
	}
	found := false
	for _, a := range many {
		if a == client {
			found = true
		}
	}
	if !found {
		return false
	}
	return len(many) == 1 || azp == client
}

// key finds the signing key by id, refetching the JWKS when the id is
// unknown — Authelia's key can rotate — but at most once a minute, so a
// stream of tokens naming made-up keys cannot turn Keystone into a
// fetch loop against the provider.
func (p *Provider) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	p.mu.Lock()
	k, ok := p.keys[kid]
	stale := p.now().Sub(p.keysAt) > time.Hour
	mayFetch := p.now().Sub(p.keysTried) >= time.Minute
	p.mu.Unlock()
	if ok && !stale {
		return k, nil
	}
	if !mayFetch {
		if ok {
			return k, nil
		}
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	p.mu.Lock()
	p.keysTried = p.now()
	p.mu.Unlock()
	keys, err := p.fetchKeys(ctx)
	if err != nil {
		if ok {
			return k, nil
		}
		return nil, err
	}
	p.mu.Lock()
	p.keys, p.keysAt = keys, p.now()
	p.mu.Unlock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

func (p *Provider) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	d, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := p.getJSON(ctx, d.JWKSURI, &set); err != nil {
		return nil, fmt.Errorf("reading the sign-in provider's keys: %w", err)
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(out) == 0 {
		return nil, errors.New("the sign-in provider publishes no RSA signing key")
	}
	return out, nil
}
