package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNoSurfaceRevealsAPersonalSecret(t *testing.T) {
	h := freshnessHarness(t)
	v, err := h.s.DB.OpenSecretVault(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	h.s.SecretVault = v
	raw := make([]byte, 24)
	rand.Read(raw)
	value := hex.EncodeToString(raw)
	s, err := v.Create("sandy", runtimedb.SecretInput{Scope: "personal", Label: "Private item", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"reveal", "review"} {
		cookie := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
		code, out := h.sessionRequest(http.MethodPost, "/api/v1/household/secrets/"+s.ID+"/"+op, `{"agent":"shared"}`, "mika", cookie)
		if code != 403 || bytes.Contains([]byte(fmt.Sprint(out)), []byte(value)) {
			t.Fatalf("Console %s: %d %s", op, code, out)
		}
	}
}
func TestPersonBCannotRevealThatSecretOnHomepage(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy", "maya")
	db := h.s.DB
	v, err := db.OpenSecretVault(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	h.s.SecretVault = v
	// This harness has no Owner; establish the creator explicitly.
	if err = db.SetButlerMemberRole("sandy", runtimedb.RoleOwner, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err = db.UpsertModel(runtimedb.Model{Name: "local", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "test", Readiness: runtimedb.ModelReady, Enabled: true}, "sandy"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateButler(runtimedb.Butler{Name: "shared", Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "local"}, "sandy"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"sandy", "maya"} {
		if err = db.GrantButlerAccess("shared", p, "sandy"); err != nil {
			t.Fatal(err)
		}
	}
	raw := make([]byte, 24)
	rand.Read(raw)
	value := hex.EncodeToString(raw)
	s, err := v.Create("sandy", runtimedb.SecretInput{Scope: "household", Label: "Shared item", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := db.GetButlerMember("sandy")
	if err = v.Grant("sandy", s.ID, runtimedb.SecretGrant{PersonID: p.PersonID, Verb: "reveal"}, true); err != nil {
		t.Fatal(err)
	}
	if err = v.AgentGrant("sandy", s.ID, "shared", "reveal", true); err != nil {
		t.Fatal(err)
	}
	for _, person := range []string{"sandy", "maya"} {
		r := h.member("POST", "/api/v1/home/secrets/"+s.ID+"/reveal", person, "homepage_users", bytes.NewBufferString(`{"agent":"shared"}`), "application/json")
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if person == "sandy" {
			var result secretRevealResponse
			json.Unmarshal(b, &result)
			if r.StatusCode != 200 || result.Value != value || r.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("positive control: %d %s", r.StatusCode, b)
			}
		} else if r.StatusCode != 403 || bytes.Contains(b, []byte(value)) {
			t.Fatalf("Agent grant authorized B: %d %s", r.StatusCode, b)
		}
	}
}
func TestSecretIntakeDoesNotPersistRawConversation(t *testing.T) {
	h := freshnessHarness(t)
	v, err := h.s.DB.OpenSecretVault(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	h.s.SecretVault = v
	value := "generated-intake-value"
	note, handled, err := h.s.prepareSecretInput("sandy", "/secret personal Library = "+value, 123)
	if err != nil || !handled || strings.Contains(note, value) || !strings.Contains(note, "already been disclosed") {
		t.Fatalf("intake: %v %v %s", handled, err, note)
	}
	objects, err := v.List("sandy", false)
	if err != nil || len(objects) != 1 {
		t.Fatalf("store: %v %v", objects, err)
	}
	records, err := h.s.DB.ExportMemoryFor("sandy")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range records {
		if strings.Contains(r.Content, value) {
			t.Fatal("raw memory")
		}
		if strings.Contains(r.Content, objects[0].ID) {
			found = true
		}
	}
	if !found {
		t.Fatal("safe reference missing")
	}
}

func TestPersonalRevealDeniedOnEveryHomepageOperation(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy", "maya")
	v, err := h.s.DB.OpenSecretVault(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	h.s.SecretVault = v
	value := "generated-personal-value"
	s, err := v.Create("sandy", runtimedb.SecretInput{Scope: "personal", Label: "Private item", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	for _, person := range []string{"sandy", "maya"} {
		for _, op := range []string{"reveal", "review"} {
			r := h.member("POST", "/api/v1/home/secrets/"+s.ID+"/"+op, person, "homepage_users", bytes.NewBufferString(`{"agent":"shared"}`), "application/json")
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			if r.StatusCode != 403 || bytes.Contains(b, []byte(value)) {
				t.Fatalf("Personal %s for %s: %d", op, person, r.StatusCode)
			}
		}
	}
}
func TestConsoleSecretWritesRequireFreshHumanSession(t *testing.T) {
	h := freshnessHarness(t)
	v, err := h.s.DB.OpenSecretVault(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	h.s.SecretVault = v
	body := `{"scope":"household","label":"Calendar","value":"generated-input"}`
	stale := sessionCookie(h.s.StartConsoleSession("mika", time.Now().Add(-30*time.Minute)))
	if code, _ := h.sessionRequest("POST", "/api/v1/household/secrets", body, "mika", stale); code != 403 {
		t.Fatalf("stale write: %d", code)
	}
	fresh := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	code, out := h.sessionRequest("POST", "/api/v1/household/secrets", body, "mika", fresh)
	if code != 200 || !strings.HasPrefix(fmt.Sprint(out["id"]), "hs_") {
		t.Fatalf("fresh write: %d %v", code, out)
	}
	if strings.Contains(fmt.Sprint(out), "generated-input") {
		t.Fatal("create echoed raw value")
	}
}

// The Secrets page offers a choice of Agent rather than a text box, and
// the choice is exactly what a grant accepts: this person's live Agents,
// with their class, and never somebody else's or a retired one.
func TestTheSecretsPageListsOnlyTheAgentsAPersonMayName(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy", "maya")
	db := h.s.DB
	v, err := db.OpenSecretVault(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	h.s.SecretVault = v
	if err = db.SetButlerMemberRole("sandy", runtimedb.RoleOwner, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err = db.UpsertModel(runtimedb.Model{Name: "local", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "test", Readiness: runtimedb.ModelReady, Enabled: true}, "sandy"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []runtimedb.Butler{
		{Name: "frank", Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "local"},
		{Name: "ops", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Model: "local"},
		{Name: "gone", Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "local"},
	} {
		if _, err = db.CreateButler(b, "sandy"); err != nil {
			t.Fatal(err)
		}
		if err = db.GrantButlerAccess(b.Name, "sandy", "sandy"); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.RetireButler("gone", "sandy"); err != nil {
		t.Fatal(err)
	}

	agentsFor := func(person string) map[string]string {
		t.Helper()
		r := h.member("GET", "/api/v1/home/secrets", person, "homepage_users", nil, "")
		defer r.Body.Close()
		var out struct {
			Agents []struct{ Name, AgentClass string } `json:"agents"`
		}
		if r.StatusCode != 200 || json.NewDecoder(r.Body).Decode(&out) != nil {
			t.Fatalf("GET secrets for %s = %d", person, r.StatusCode)
		}
		got := map[string]string{}
		for _, a := range out.Agents {
			got[a.Name] = a.AgentClass
		}
		return got
	}
	if got := agentsFor("sandy"); len(got) != 2 || got["frank"] != "member" || got["ops"] != "owner" {
		t.Fatalf("sandy's Agents = %v, want frank (member) and ops (owner), not the retired one", got)
	}
	if got := agentsFor("maya"); len(got) != 0 {
		t.Fatalf("maya was offered Agents she was never granted: %v", got)
	}
}
