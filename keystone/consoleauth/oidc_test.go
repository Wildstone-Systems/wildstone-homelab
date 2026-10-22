package consoleauth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/consoleauth"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/consoleauth/fakeidp"
)

const issuer = "https://auth.home.test"

func provider(t *testing.T) (*consoleauth.Provider, *fakeidp.IdP) {
	t.Helper()
	idp := fakeidp.New(t, issuer, "wildstone-console")
	idp.User = "mika"
	idp.AuthTime = time.Now().Add(-time.Minute)
	return &consoleauth.Provider{Issuer: issuer, ClientID: "wildstone-console",
		RedirectURL: "https://console.home.test/api/v1/console/callback", HTTP: idp.Client()}, idp
}

func TestAGoodTokenVerifies(t *testing.T) {
	p, idp := provider(t)
	c, err := p.Verify(context.Background(), idp.Mint("n1"), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if c.User != "mika" || c.AuthTime.Unix() != idp.AuthTime.Unix() {
		t.Errorf("claims = %+v", c)
	}
}

// Every way a token can be wrong is refused, one at a time. A verifier
// that is safe only when every check happens to agree is one refactor
// from not being safe.
func TestEveryForgedTokenIsRefused(t *testing.T) {
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what  string
		token func(*fakeidp.IdP) string
		nonce string
	}{
		{"alg none", func(i *fakeidp.IdP) string {
			tok := fakeidp.Sign(i.Key, map[string]any{"alg": "none", "kid": i.Kid}, map[string]any{"iss": issuer})
			return tok[:strings.LastIndex(tok, ".")+1]
		}, "n1"},
		{"HS256 header", func(i *fakeidp.IdP) string {
			return fakeidp.Sign(i.Key, map[string]any{"alg": "HS256", "kid": i.Kid}, map[string]any{"iss": issuer})
		}, "n1"},
		{"signed by another key under the provider's key id", func(i *fakeidp.IdP) string {
			i.SignWith = other
			defer func() { i.SignWith = nil }()
			return i.Mint("n1")
		}, "n1"},
		{"an unknown key id", func(i *fakeidp.IdP) string {
			return fakeidp.Sign(i.Key, map[string]any{"alg": "RS256", "kid": "nope"}, map[string]any{"iss": issuer})
		}, "n1"},
		{"another issuer", mutated(func(m map[string]any) { m["iss"] = "https://evil.test" }), "n1"},
		{"another audience", mutated(func(m map[string]any) { m["aud"] = "some-other-app" }), "n1"},
		{"several audiences with no azp", mutated(func(m map[string]any) { m["aud"] = []string{"wildstone-console", "x"} }), "n1"},
		{"expired", mutated(func(m map[string]any) { m["exp"] = time.Now().Add(-time.Second).Unix() }), "n1"},
		{"no exp", mutated(func(m map[string]any) { delete(m, "exp") }), "n1"},
		{"issued in the future", mutated(func(m map[string]any) { m["iat"] = time.Now().Add(time.Hour).Unix() }), "n1"},
		{"another sign-in's nonce", func(i *fakeidp.IdP) string { return i.Mint("n1") }, "n2"},
		{"no nonce expected", func(i *fakeidp.IdP) string { return i.Mint("") }, ""},
		{"no auth_time", mutated(func(m map[string]any) { delete(m, "auth_time") }), "n1"},
		{"no username", mutated(func(m map[string]any) { m["preferred_username"] = " " }), "n1"},
		{"tampered claims", func(i *fakeidp.IdP) string {
			parts := strings.Split(i.Mint("n1"), ".")
			other := strings.Split(fakeidp.Sign(i.Key, map[string]any{"alg": "RS256", "kid": i.Kid},
				map[string]any{"iss": issuer, "preferred_username": "admin"}), ".")
			return parts[0] + "." + other[1] + "." + parts[2]
		}, "n1"},
	} {
		t.Run(c.what, func(t *testing.T) {
			p, idp := provider(t)
			if _, err := p.Verify(context.Background(), c.token(idp), c.nonce); !errors.Is(err, consoleauth.ErrToken) {
				t.Errorf("accepted, or refused for the wrong reason: %v", err)
			}
		})
	}
}

func mutated(f func(map[string]any)) func(*fakeidp.IdP) string {
	return func(i *fakeidp.IdP) string {
		i.Mutate = f
		defer func() { i.Mutate = nil }()
		return i.Mint("n1")
	}
}

// TestClockSkewDoesNotGrantFreshness (#205): an authentication stamped
// in the future is refused outright, and one a few seconds ahead is
// clamped to now — so no amount of skew makes a session fresh for
// longer than the window.
func TestClockSkewDoesNotGrantFreshness(t *testing.T) {
	p, idp := provider(t)
	idp.AuthTime = time.Now().Add(24 * time.Hour)
	if _, err := p.Verify(context.Background(), idp.Mint("n1"), "n1"); !errors.Is(err, consoleauth.ErrToken) {
		t.Fatalf("a future-dated authentication verified: %v", err)
	}

	idp.AuthTime = time.Now().Add(30 * time.Second)
	c, err := p.Verify(context.Background(), idp.Mint("n1"), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if c.AuthTime.After(time.Now()) {
		t.Errorf("auth_time %s is later than now; skew would extend the window", c.AuthTime)
	}

	now := time.Now()
	if consoleauth.Fresh(now.Add(time.Hour), now, consoleauth.DefaultEntryWindow) {
		t.Error("Fresh called a future authentication fresh")
	}
	if consoleauth.Fresh(time.Time{}, now, consoleauth.DefaultEntryWindow) {
		t.Error("Fresh called a missing authentication fresh")
	}
	if consoleauth.Fresh(now.Add(-consoleauth.DefaultEntryWindow-time.Second), now, consoleauth.DefaultEntryWindow) {
		t.Error("Fresh called an authentication outside the window fresh")
	}
}

// Entering the Console sends the entry window as max_age, so a
// month-old family session is not stamped straight into the Console;
// step-up forces a new authentication.
func TestTheAuthorizationRequestCarriesTheWindow(t *testing.T) {
	p, _ := provider(t)
	ctx := context.Background()
	u, err := p.AuthURL(ctx, "s", "n", "c", 12*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "max_age=43200") || strings.Contains(u, "prompt=") {
		t.Errorf("entry request = %s", u)
	}
	u, _ = p.AuthURL(ctx, "s", "n", "c", 12*time.Hour, true)
	if !strings.Contains(u, "prompt=login") {
		t.Errorf("step-up request = %s", u)
	}
	// Never max_age alongside it: Authelia reads max_age=0 as a sign-in
	// that can never be recent enough, so the step-up could not finish.
	if strings.Contains(u, "max_age") {
		t.Errorf("step-up request carries max_age, which Authelia can never satisfy at 0: %s", u)
	}
}

func TestAProviderNamingAnotherIssuerIsRefused(t *testing.T) {
	idp := fakeidp.New(t, "https://somewhere.else", "wildstone-console")
	p := &consoleauth.Provider{Issuer: issuer, ClientID: "wildstone-console", HTTP: idp.Client()}
	if _, err := p.AuthURL(context.Background(), "s", "n", "c", time.Hour, false); err == nil {
		t.Fatal("discovery accepted a provider that calls itself something else")
	}
}

func TestAStateIsGoodOnce(t *testing.T) {
	var s consoleauth.Store
	state := s.Begin(consoleauth.Pending{User: "mika"})
	if _, ok := s.Take(state); !ok {
		t.Fatal("a fresh state was refused")
	}
	if _, ok := s.Take(state); ok {
		t.Fatal("a state was accepted twice")
	}
	now := time.Now()
	s.Now = func() time.Time { return now }
	state = s.Begin(consoleauth.Pending{User: "mika"})
	s.Now = func() time.Time { return now.Add(11 * time.Minute) }
	if _, ok := s.Take(state); ok {
		t.Fatal("an abandoned sign-in was accepted after its lifetime")
	}
}
