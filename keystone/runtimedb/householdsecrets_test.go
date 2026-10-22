package runtimedb

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func secretFixture(t *testing.T) (*DB, *SecretVault, string) {
	t.Helper()
	db := butlerFixture(t)
	newButler(t, db, "shared", "kitchen-box")
	for _, p := range []string{"sam", "mika"} {
		if err := db.GrantButlerAccess("shared", p, "mika"); err != nil {
			t.Fatal(err)
		}
	}
	v, err := db.OpenSecretVault(filepath.Join(t.TempDir(), "household.key"))
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 24)
	rand.Read(raw)
	value := hex.EncodeToString(raw)
	return db, v, value
}
func TestPersonBCannotUseAHouseholdSecretGrantedOnlyToPersonA(t *testing.T) {
	db, v, value := secretFixture(t)
	s, err := v.Create("mika", SecretInput{Scope: "household", Label: "Calendar", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := db.GetButlerMember("sam")
	if err = v.Grant("mika", s.ID, SecretGrant{PersonID: p.PersonID, Verb: "use"}, true); err != nil {
		t.Fatal(err)
	}
	if err = v.AgentGrant("mika", s.ID, "shared", "calendar.get_agenda", true); err != nil {
		t.Fatal(err)
	}
	called := false
	use := func(raw []byte) error {
		called = true
		if string(raw) != value {
			t.Fatal("wrong value")
		}
		return nil
	}
	if err = v.Use(s.ID, db.BindMemoryRead("sam", "shared", MemoryDestination{BuiltIn: true}), "calendar.get_agenda", use); err != nil || !called {
		t.Fatalf("authorized use: %v", err)
	}
	called = false
	if err = v.Use(s.ID, db.BindMemoryRead("mika", "shared", MemoryDestination{BuiltIn: true}), "calendar.get_agenda", use); err == nil || called {
		t.Fatal("Agent grant authorized Person B")
	}
	if err = v.Grant("mika", s.ID, SecretGrant{PersonID: p.PersonID, Verb: "use"}, false); err != nil {
		t.Fatal(err)
	}
	if err = v.Use(s.ID, db.BindMemoryRead("sam", "shared", MemoryDestination{BuiltIn: true}), "calendar.get_agenda", use); err == nil {
		t.Fatal("revoked grant authorized use")
	}
}
func TestNoSurfaceRevealsAPersonalSecretStore(t *testing.T) {
	db, v, value := secretFixture(t)
	s, err := v.Create("sam", SecretInput{Scope: "personal", Label: "Library", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"sam", "mika"} {
		for _, surface := range []string{"homepage", "console", "external"} {
			if _, err = v.Reveal(s.ID, db.BindMemoryRead(p, "shared", MemoryDestination{BuiltIn: true}), surface); err == nil {
				t.Fatalf("Personal revealed to %s on %s", p, surface)
			}
		}
	}
	if err = v.Rotate("mika", s.ID, value); err == nil {
		t.Fatal("Owner managed another Person's secret")
	}
	for _, action := range []string{"quarantine", "disable", "delete", "offboard"} {
		if err = v.Lifecycle("mika", s.ID, action); err != nil {
			t.Fatal(err)
		}
	}
}
func TestUseDoesNotImplyReveal(t *testing.T) {
	db, v, value := secretFixture(t)
	s, err := v.Create("mika", SecretInput{Scope: "household", Label: "Calendar", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := db.GetButlerMember("sam")
	if err = v.Grant("mika", s.ID, SecretGrant{PersonID: p.PersonID, Verb: "use"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err = v.Reveal(s.ID, db.BindMemoryRead("sam", "shared", MemoryDestination{BuiltIn: true}), "homepage"); err == nil {
		t.Fatal("use implied reveal")
	}
}
func TestManageOnHouseholdSecretIsOwnerOnly(t *testing.T) {
	_, v, value := secretFixture(t)
	if _, err := v.Create("sam", SecretInput{Scope: "household", Label: "Calendar", Value: value}); err == nil {
		t.Fatal("Member created active Household secret")
	}
}
func TestSecretGrantsUseTheSameGroupNamespace(t *testing.T) {
	db, v, value := secretFixture(t)
	p, _ := db.GetButlerMember("sam")
	gid := groupID(t, db, SeedAdults)
	if err := db.AddPersonToGroup(gid, p.PersonID, "mika"); err != nil {
		t.Fatal(err)
	}
	s, err := v.Create("mika", SecretInput{Scope: "household", Label: "Calendar", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	if err = v.Grant("mika", s.ID, SecretGrant{GroupID: gid, Verb: "use"}, true); err != nil {
		t.Fatal(err)
	}
	if err = v.AgentGrant("mika", s.ID, "shared", "calendar.get_agenda", true); err != nil {
		t.Fatal(err)
	}
	read := db.BindMemoryRead("sam", "shared", MemoryDestination{BuiltIn: true})
	if err = v.Use(s.ID, read, "calendar.get_agenda", func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err = db.RemovePersonFromGroup(gid, p.PersonID); err != nil {
		t.Fatal(err)
	}
	if err = v.Use(s.ID, read, "calendar.get_agenda", func([]byte) error { t.Fatal("revoked group reached consumer"); return nil }); err == nil {
		t.Fatal("group revocation ignored")
	}
}
func TestOwnerLifecycleOnAPersonalSecretDoesNotRevealOrGrantManage(t *testing.T) {
	for _, action := range []string{"quarantine", "disable", "delete", "offboard"} {
		t.Run(action, func(t *testing.T) {
			db, v, value := secretFixture(t)
			s, err := v.Create("sam", SecretInput{Scope: "personal", Label: "Private item", Value: value})
			if err != nil {
				t.Fatal(err)
			}
			if err = v.AgentGrant("sam", s.ID, "shared", "calendar.get_agenda", true); err != nil {
				t.Fatal(err)
			}
			read := db.BindMemoryRead("sam", "shared", MemoryDestination{BuiltIn: true})
			if err = v.Use(s.ID, read, "calendar.get_agenda", func([]byte) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err = v.Lifecycle("mika", s.ID, action); err != nil {
				t.Fatal(err)
			}
			if err = v.Use(s.ID, read, "calendar.get_agenda", func([]byte) error { t.Fatal("lifecycle retained use"); return nil }); err == nil {
				t.Fatal("lifecycle did not fence")
			}
			if err = v.AgentGrant("mika", s.ID, "shared", "calendar.get_agenda", true); err == nil {
				t.Fatal("lifecycle conferred manage")
			}
			if _, err = v.Review("mika", s.ID); err == nil {
				t.Fatal("lifecycle conferred review")
			}
		})
	}
}
func TestSecretCiphertextAndKeyFailures(t *testing.T) {
	db, v, value := secretFixture(t)
	s, err := v.Create("sam", SecretInput{Scope: "personal", Label: "Private item", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext []byte
	if err = db.sql.QueryRow(`SELECT ciphertext FROM household_secrets WHERE id=?`, s.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte(value)) {
		t.Fatal("plaintext stored")
	}
	if _, err = db.OpenSecretVault(filepath.Join(t.TempDir(), "missing.key")); err == nil {
		t.Fatal("missing key replaced on populated store")
	}
	if err = v.AgentGrant("sam", s.ID, "shared", "calendar.get_agenda", true); err != nil {
		t.Fatal(err)
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err = db.sql.Exec(`UPDATE household_secrets SET ciphertext=? WHERE id=?`, ciphertext, s.ID); err != nil {
		t.Fatal(err)
	}
	if err = v.Use(s.ID, db.BindMemoryRead("sam", "shared", MemoryDestination{BuiltIn: true}), "calendar.get_agenda", func([]byte) error { t.Fatal("tampered value reached tool"); return nil }); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}
func TestHouseholdSubmissionIsSeparateFromPersonal(t *testing.T) {
	_, v, value := secretFixture(t)
	original, err := v.Create("sam", SecretInput{Scope: "personal", Label: "Private item", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := v.Create("sam", SecretInput{Scope: "household", Label: "Shared item", Value: value, Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := v.Review("mika", submitted.ID)
	if err != nil || string(raw) != value {
		t.Fatal("Owner could not validate explicitly submitted copy", err)
	}
	clear(raw)
	if err = v.Lifecycle("mika", submitted.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err = v.Review("mika", original.ID); err == nil {
		t.Fatal("submission exposed Personal original")
	}
	if err = v.Rotate("mika", original.ID, value); err == nil {
		t.Fatal("submission transferred Personal management")
	}
}
func TestSecretRestoreDoesNotResurrectDeletedValues(t *testing.T) {
	db, v, value := secretFixture(t)
	s, err := v.Create("sam", SecretInput{Scope: "personal", Label: "Private item", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	var old []byte
	if err = db.sql.QueryRow(`SELECT ciphertext FROM household_secrets WHERE id=?`, s.ID).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if err = v.Lifecycle("mika", s.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.sql.Exec(`UPDATE household_secrets SET state='active',ciphertext=? WHERE id=?`, old, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ReconcileRestore(); err != nil {
		t.Fatal(err)
	}
	var state string
	var n int
	if err = db.sql.QueryRow(`SELECT state,length(ciphertext) FROM household_secrets WHERE id=?`, s.ID).Scan(&state, &n); err != nil {
		t.Fatal(err)
	}
	if state != "deleted" || n != 0 {
		t.Fatal("restore resurrected secret")
	}
}
func TestWrongSecretKeyRefusesOpen(t *testing.T) {
	db, v, value := secretFixture(t)
	if _, err := v.Create("sam", SecretInput{Scope: "personal", Label: "Private item", Value: value}); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	rand.Read(key)
	path := filepath.Join(t.TempDir(), "wrong.key")
	if err := os.WriteFile(path, key, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.OpenSecretVault(path); err == nil {
		t.Fatal("wrong key accepted; subsequent writes could create mixed-key store")
	}
}
func TestOnlyOwningPersonCanSubmitPersonalCopy(t *testing.T) {
	_, v, value := secretFixture(t)
	original, err := v.Create("sam", SecretInput{Scope: "personal", Label: "WiFi", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.SubmitPersonalCopy("mika", original.ID); err == nil {
		t.Fatal("Owner copied another Person's value")
	}
	copy, err := v.SubmitPersonalCopy("sam", original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if copy.ID == original.ID || copy.State != "pending" || copy.Scope != "household" {
		t.Fatal("not a separate proposal")
	}
	raw, err := v.Review("mika", copy.ID)
	if err != nil || string(raw) != value {
		t.Fatal("submitted copy cannot be reviewed", err)
	}
	clear(raw)
	if _, err = v.Review("mika", original.ID); err == nil {
		t.Fatal("original became reviewable")
	}
	if err = v.Lifecycle("mika", copy.ID, "reject"); err != nil {
		t.Fatal(err)
	}
	objects, err := v.List("sam", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objects {
		if o.ID == original.ID && o.State != "active" {
			t.Fatal("rejection changed original")
		}
	}
}
func TestSecretValueCannotBecomeItsMemoryLabel(t *testing.T) {
	_, v, _ := secretFixture(t)
	if _, err := v.Create("sam", SecretInput{Scope: "personal", Label: "Example code 1234", Value: "1234"}); err == nil {
		t.Fatal("raw value admitted into plaintext label")
	}
}

func TestSecretUseDoesNotBlockConcurrentRevocation(t *testing.T) {
	db, v, value := secretFixture(t)
	s, err := v.Create("sam", SecretInput{Scope: "personal", Label: "Calendar", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	if err = v.AgentGrant("sam", s.ID, "shared", "calendar.get_agenda", true); err != nil {
		t.Fatal(err)
	}
	caller := db.BindMemoryRead("sam", "shared", MemoryDestination{BuiltIn: true})
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- v.Use(s.ID, caller, "calendar.get_agenda", func(raw []byte) error { close(started); <-release; return nil })
	}()
	<-started
	revoked := make(chan error, 1)
	go func() { revoked <- v.AgentGrant("sam", s.ID, "shared", "calendar.get_agenda", false) }()
	timedOut := false
	select {
	case err = <-revoked:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(2 * time.Second):
		timedOut = true
		t.Error("external secret use blocked database revocation")
	}
	close(release)
	if timedOut {
		if err = <-revoked; err != nil {
			t.Error(err)
		}
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	var uses int
	if err = db.sql.QueryRow("SELECT count(*) FROM household_secret_audit WHERE secret_id=? AND action='use'", s.ID).Scan(&uses); err != nil || uses != 1 {
		t.Fatalf("completion audit: count=%d err=%v", uses, err)
	}
	// A use already authorized may finish; revocation fences subsequent uses.
	if err = v.Use(s.ID, caller, "calendar.get_agenda", func([]byte) error { t.Error("revoked credential consumed"); return nil }); err == nil {
		t.Error("revoked grant authorized use")
	}
}
