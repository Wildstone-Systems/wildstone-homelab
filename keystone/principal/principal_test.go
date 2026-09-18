package principal

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// migrated is the schema, built once for this whole test binary rather
// than replayed into every database these tests open (#231). Each test
// still gets its own file.
var migrated = dbtemplate.New(func(path string) error {
	db, err := runtimedb.Open(path)
	if err != nil {
		return err
	}
	return db.Close()
})

func store(t *testing.T) *Store {
	t.Helper()
	db, err := runtimedb.Open(migrated.Path(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &Store{DB: db}
}

func TestMintAndAuthenticate(t *testing.T) {
	s := store(t)
	if err := s.Create("mika", runtimedb.RoleAdmin, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	secret, err := s.Mint("mika", "", 0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(secret, "wst_") || len(secret) != 4+64 {
		t.Fatalf("secret shape %q", secret)
	}
	id, err := s.Authenticate(secret)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Name != "mika" || id.Role != runtimedb.RoleAdmin || id.ActingFor != "" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestAuthenticateFailuresCollapse(t *testing.T) {
	s := store(t)
	if err := s.Create("mika", runtimedb.RoleOperator, ""); err != nil {
		t.Fatal(err)
	}
	secret, err := s.Mint("mika", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	for name, tok := range map[string]string{
		"empty":        "",
		"no prefix":    strings.TrimPrefix(secret, "wst_"),
		"wrong secret": "wst_" + strings.Repeat("0", 64),
	} {
		if _, err := s.Authenticate(tok); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: err = %v, want ErrUnauthenticated", name, err)
		}
	}

	// Revoked.
	if err := s.Revoke(secret); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := s.Authenticate(secret); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked token authenticated: %v", err)
	}

	// Expired: TTL in the past by construction.
	expired, err := s.Mint("mika", "", -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(expired); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired token authenticated: %v", err)
	}

	// Disabled principal invalidates its live tokens.
	live, err := s.Mint("mika", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.SetPrincipalDisabled("mika", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(live); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("disabled principal's token authenticated: %v", err)
	}
	// And minting for a disabled principal is refused.
	if _, err := s.Mint("mika", "", 0); err == nil {
		t.Fatal("Mint for disabled principal should fail")
	}
}

func TestActingForPropagation(t *testing.T) {
	s := store(t)
	// Principal-level acting identity.
	if err := s.Create("concierge", runtimedb.RoleOperator, "grandma"); err != nil {
		t.Fatal(err)
	}
	tok, err := s.Mint("concierge", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Authenticate(tok)
	if err != nil || id.ActingFor != "grandma" {
		t.Fatalf("identity = %+v, err %v; want actingFor grandma", id, err)
	}
	// Token-level override wins.
	tok2, err := s.Mint("concierge", "aunt", 0)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.Authenticate(tok2)
	if err != nil || id2.ActingFor != "aunt" {
		t.Fatalf("identity = %+v, err %v; want actingFor aunt", id2, err)
	}
}

func TestCreateValidation(t *testing.T) {
	s := store(t)
	for _, bad := range []string{"", "UPPER", "-lead", "trail-", "a b", strings.Repeat("x", 64)} {
		if err := s.Create(bad, runtimedb.RoleAdmin, ""); err == nil {
			t.Errorf("Create(%q) succeeded, want name validation error", bad)
		}
	}
}

func TestBootstrap(t *testing.T) {
	s := store(t)
	secret, err := s.Bootstrap()
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if secret == "" {
		t.Fatal("first Bootstrap returned empty secret")
	}
	id, err := s.Authenticate(secret)
	if err != nil || id.Role != runtimedb.RoleAdmin {
		t.Fatalf("bootstrap identity = %+v, err %v", id, err)
	}
	// Second call is a no-op: principals exist.
	again, err := s.Bootstrap()
	if err != nil || again != "" {
		t.Fatalf("second Bootstrap = %q, %v; want empty no-op", again, err)
	}
}

func TestRBACTable(t *testing.T) {
	cases := []struct {
		role runtimedb.Role
		verb Verb
		want bool
	}{
		{runtimedb.RoleAdmin, VerbManage, true},
		{runtimedb.RoleAdmin, VerbApprove, true},
		{runtimedb.RoleOperator, VerbApprove, true},
		{runtimedb.RoleOperator, VerbManage, false},
		{runtimedb.RoleButler, VerbRead, true},
		{runtimedb.RoleButler, VerbPropose, true},
		// The load-bearing row: the Butler can NEVER approve (ADR-W007 A2).
		{runtimedb.RoleButler, VerbApprove, false},
		{runtimedb.RoleButler, VerbManage, false},
		{runtimedb.RoleButler, VerbEmit, false},
		// A publisher emits and nothing else (ADR-W024 A3).
		{runtimedb.RolePublisher, VerbEmit, true},
		{runtimedb.RolePublisher, VerbRead, false},
		{runtimedb.RolePublisher, VerbPropose, false},
		{runtimedb.RolePublisher, VerbApprove, false},
		{runtimedb.RolePublisher, VerbManage, false},
		{runtimedb.RoleAdmin, VerbEmit, true},
		{runtimedb.RoleOperator, VerbEmit, false},
		{runtimedb.Role("root"), VerbRead, false},
	}
	for _, c := range cases {
		if got := Allowed(c.role, c.verb); got != c.want {
			t.Errorf("Allowed(%s, %s) = %v, want %v", c.role, c.verb, got, c.want)
		}
	}
}
