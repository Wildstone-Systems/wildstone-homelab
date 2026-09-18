package principal

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// EnsureAdmin is the offline escape hatch F44 has needed since
// v0.1.6-alpha: Bootstrap returns "" once any principal exists, so a
// lost token left destroying the runtime DB as the only way back.

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := runtimedb.Open(migrated.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Store{DB: db}
}

func TestEnsureAdminCreatesTheFirstAdmin(t *testing.T) {
	s := testStore(t)
	tok, err := s.EnsureAdmin("admin")
	if err != nil {
		t.Fatal(err)
	}
	if id, err := s.Authenticate(tok); err != nil || id.Name != "admin" {
		t.Fatalf("minted token does not authenticate: %v %+v", err, id)
	}
}

// The case Bootstrap cannot serve, and the whole reason this exists: an
// admin already exists and its token is gone.
func TestEnsureAdminMintsWhenPrincipalsAlreadyExist(t *testing.T) {
	s := testStore(t)
	if _, err := s.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if again, err := s.Bootstrap(); err != nil || again != "" {
		t.Fatalf("Bootstrap should decline once principals exist, got %q %v", again, err)
	}
	tok, err := s.EnsureAdmin("admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(tok); err != nil {
		t.Fatalf("recovery token does not authenticate: %v", err)
	}
}

// Recovery must not cost the operator their existing accounts.
func TestEnsureAdminDoesNotDisturbExistingPrincipals(t *testing.T) {
	s := testStore(t)
	if err := s.Create("family", runtimedb.RoleOperator, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureAdmin("admin"); err != nil {
		t.Fatal(err)
	}
	ps, err := s.DB.ListPrincipals()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]runtimedb.Role{}
	for _, p := range ps {
		found[p.Name] = p.Role
	}
	if found["family"] != runtimedb.RoleOperator {
		t.Errorf("existing principal changed: %+v", found)
	}
	if found["admin"] != runtimedb.RoleAdmin {
		t.Errorf("admin not created as admin: %+v", found)
	}
}

// Old tokens must keep working: recovery adds a credential, it does not
// invalidate the ones an operator may still be using.
func TestEnsureAdminLeavesEarlierTokensValid(t *testing.T) {
	s := testStore(t)
	first, err := s.EnsureAdmin("admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureAdmin("admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(first); err != nil {
		t.Errorf("the earlier token stopped working: %v", err)
	}
}
