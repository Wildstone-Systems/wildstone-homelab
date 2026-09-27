package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// fakePantryOS is PantryOS's side of pairing: it accepts one code, keeps
// what Keystone sent, and answers with an agent key its contract accepts.
type fakePantryOS struct {
	mu          sync.Mutex
	code        string
	key         string
	gotToken    string
	gotKeystone string
	capsStatus  int
	// delay holds the exchange open, so two pairings can overlap.
	delay time.Duration
	srv   *httptest.Server
}

func newFakePantryOS(t *testing.T, code, key string) *fakePantryOS {
	t.Helper()
	f := &fakePantryOS{code: code, key: key, capsStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+pantryExchangePath, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(f.delay)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if body["code"] != f.code {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"invalid or expired pairing code"}`))
			return
		}
		f.code = "" // single use
		f.gotToken, f.gotKeystone = body["publisher_token"], body["keystone_url"]
		_ = json.NewEncoder(w).Encode(map[string]string{"agent_key": f.key, "contract": "pantry-1"})
	})
	mux.HandleFunc("GET /api/integrations/wildstone/v1/capabilities", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.capsStatus != http.StatusOK || r.Header.Get("X-Agent-Key") != f.key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"contract":"pantry-1","capabilities":[{"kind":"query","name":"pantry.list","version":"pantry-1","available":true}]}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func pairingHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	v, err := h.s.DB.OpenSecretVault(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	h.s.SecretVault = v
	return h
}

func pair(h *harness, address, code string) (*http.Response, map[string]any) {
	var out map[string]any
	resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/pantry/pairing", map[string]string{
		"address": address, "code": code, "keystoneUrl": "https://keystone.example.internal",
	}, &out)
	return resp, out
}

func (h *harness) publisherTokenWorks(token string) bool {
	id, err := h.s.Principals.Authenticate(token)
	return err == nil && id.Name == publisherPrincipalPrefix+PantryCapability
}

func TestPairingHandsBothSecretsOverAndShowsNeither(t *testing.T) {
	h := pairingHarness(t)
	pantryOS := newFakePantryOS(t, "ABCD-EFGH", "REPLACED-agent-key-1")

	resp, out := pair(h, pantryOS.srv.URL+"/", "ABCD-EFGH")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pair = %d %v", resp.StatusCode, out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "REPLACED-agent-key-1") || strings.Contains(string(raw), pantryOS.gotToken) {
		t.Fatalf("a secret appeared in the response: %s", raw)
	}
	// PantryOS received a working publisher token and Keystone's address.
	if !h.publisherTokenWorks(pantryOS.gotToken) || pantryOS.gotKeystone != "https://keystone.example.internal" {
		t.Fatalf("PantryOS got token-works=%v keystone=%q", h.publisherTokenWorks(pantryOS.gotToken), pantryOS.gotKeystone)
	}
	// Keystone keeps the agent key, sealed, for the pantry tools.
	endpoint, key, err := h.s.SecretVault.PairingCredential(PantryCapability)
	if err != nil || endpoint != pantryOS.srv.URL || key != "REPLACED-agent-key-1" {
		t.Fatalf("stored = %q %q %v", endpoint, key, err)
	}
	var status map[string]any
	h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/pantry/pairing", nil, &status)
	if status["paired"] != true {
		t.Fatalf("status = %v", status)
	}
}

func TestPairingAgainRotatesThePublisherToken(t *testing.T) {
	h := pairingHarness(t)
	first := newFakePantryOS(t, "AAAA-AAAA", "REPLACED-agent-key-1")
	if resp, out := pair(h, first.srv.URL, "AAAA-AAAA"); resp.StatusCode != http.StatusOK {
		t.Fatalf("first = %d %v", resp.StatusCode, out)
	}
	second := newFakePantryOS(t, "BBBB-BBBB", "REPLACED-agent-key-2")
	if resp, out := pair(h, second.srv.URL, "BBBB-BBBB"); resp.StatusCode != http.StatusOK {
		t.Fatalf("second = %d %v", resp.StatusCode, out)
	}
	if h.publisherTokenWorks(first.gotToken) || !h.publisherTokenWorks(second.gotToken) {
		t.Fatal("re-pairing did not rotate the publisher token")
	}
	if _, key, _ := h.s.SecretVault.PairingCredential(PantryCapability); key != "REPLACED-agent-key-2" {
		t.Fatalf("stored key = %q", key)
	}
}

// Two pairings at once (two Owners, or a double submit) are taken one after
// the other: whichever lands last is THE pairing, its token works, and the
// other's does not. Without that, each rotation could revoke the other's new
// token and leave PantryOS holding a dead one behind a 200.
func TestTwoPairingsAtOnceLeaveOneWorkingPairing(t *testing.T) {
	h := pairingHarness(t)
	first := newFakePantryOS(t, "AAAA-AAAA", "REPLACED-agent-key-1")
	second := newFakePantryOS(t, "BBBB-BBBB", "REPLACED-agent-key-2")
	first.delay, second.delay = 200*time.Millisecond, 200*time.Millisecond

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, p := range []*fakePantryOS{first, second} {
		wg.Add(1)
		go func(i int, p *fakePantryOS, code string) {
			defer wg.Done()
			resp, _ := pair(h, p.srv.URL, code)
			codes[i] = resp.StatusCode
		}(i, p, []string{"AAAA-AAAA", "BBBB-BBBB"}[i])
	}
	wg.Wait()
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("pairings answered %v", codes)
	}
	endpoint, key, err := h.s.SecretVault.PairingCredential(PantryCapability)
	if err != nil {
		t.Fatal(err)
	}
	winner, loser := first, second
	if key == "REPLACED-agent-key-2" {
		winner, loser = second, first
	}
	if endpoint != winner.srv.URL {
		t.Fatalf("stored endpoint %q is not the pairing whose key is stored", endpoint)
	}
	if !h.publisherTokenWorks(winner.gotToken) {
		t.Fatal("the pairing that stands holds a revoked publisher token")
	}
	if h.publisherTokenWorks(loser.gotToken) {
		t.Fatal("the replaced pairing's publisher token still works")
	}
}

// A failed pairing undoes only itself: the token it minted is revoked,
// and an existing pairing keeps working.
func TestAFailedPairingLeavesTheCurrentOneAlone(t *testing.T) {
	h := pairingHarness(t)
	good := newFakePantryOS(t, "AAAA-AAAA", "REPLACED-agent-key-1")
	if resp, _ := pair(h, good.srv.URL, "AAAA-AAAA"); resp.StatusCode != http.StatusOK {
		t.Fatal("setup pairing failed")
	}

	// Wrong code.
	other := newFakePantryOS(t, "RIGHT-CODE", "REPLACED-agent-key-2")
	resp, out := pair(h, other.srv.URL, "WRONG-CODE")
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(out["error"].(string), "wrong, expired, or already used") {
		t.Fatalf("wrong code = %d %v", resp.StatusCode, out)
	}
	// The key PantryOS handed back does not work.
	broken := newFakePantryOS(t, "CCCC-CCCC", "REPLACED-agent-key-3")
	broken.capsStatus = http.StatusUnauthorized
	resp, out = pair(h, broken.srv.URL, "CCCC-CCCC")
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(out["error"].(string), "does not work") {
		t.Fatalf("broken key = %d %v", resp.StatusCode, out)
	}
	if h.publisherTokenWorks(broken.gotToken) {
		t.Fatal("the token minted for a failed pairing still works")
	}
	if !h.publisherTokenWorks(good.gotToken) {
		t.Fatal("a failed pairing revoked the working one's token")
	}
	if endpoint, _, _ := h.s.SecretVault.PairingCredential(PantryCapability); endpoint != good.srv.URL {
		t.Fatalf("a failed pairing replaced the working one: %q", endpoint)
	}
}

func TestPairingNeverFollowsARedirect(t *testing.T) {
	h := pairingHarness(t)
	var leaked string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		leaked = string(b)
	}))
	defer elsewhere.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	if resp, _ := pair(h, redirecting.URL, "ABCD-EFGH"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("redirect = %d", resp.StatusCode)
	}
	if leaked != "" {
		t.Fatalf("the publisher token followed a redirect: %s", leaked)
	}
}

func TestPairingRefusesBadInputAndNonAdmins(t *testing.T) {
	h := pairingHarness(t)
	for _, body := range []map[string]string{
		{"address": "pantry", "code": "X", "keystoneUrl": "https://k.example.internal"},
		{"address": "https://user:pw@pantry.example.internal", "code": "X", "keystoneUrl": "https://k.example.internal"},
		{"address": "https://pantry.example.internal", "code": "", "keystoneUrl": "https://k.example.internal"},
		{"address": "https://pantry.example.internal", "code": "X", "keystoneUrl": "ftp://k"},
	} {
		if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/pantry/pairing", body, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d", body, resp.StatusCode)
		}
	}
	if resp := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/pantry/pairing", map[string]string{}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("butler pairing = %d", resp.StatusCode)
	}
}

func TestUnpairingForgetsTheKeyAndRevokesTheToken(t *testing.T) {
	h := pairingHarness(t)
	pantryOS := newFakePantryOS(t, "ABCD-EFGH", "REPLACED-agent-key-1")
	if resp, _ := pair(h, pantryOS.srv.URL, "ABCD-EFGH"); resp.StatusCode != http.StatusOK {
		t.Fatal("pairing failed")
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodDelete, "/api/v1/pantry/pairing", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("unpair = %d", resp.StatusCode)
	}
	if _, _, err := h.s.SecretVault.PairingCredential(PantryCapability); err == nil {
		t.Fatal("the key survived unpairing")
	}
	if h.publisherTokenWorks(pantryOS.gotToken) {
		t.Fatal("the publisher token survived unpairing")
	}
}

// While PantryOS is paired, its publisher token belongs to the pairing: the
// generic publisher route would revoke it behind PantryOS's back, so it is
// refused and the token keeps working. Pairing again is how it rotates.
func TestAPairedPantryTokenIsNotRotatedByTheGenericPublisherRoute(t *testing.T) {
	h := pairingHarness(t)
	pantryOS := newFakePantryOS(t, "ABCD-EFGH", "REPLACED-agent-key-1")
	if resp, out := pair(h, pantryOS.srv.URL, "ABCD-EFGH"); resp.StatusCode != http.StatusOK {
		t.Fatalf("pair = %d %v", resp.StatusCode, out)
	}
	resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/publishers/pantry", map[string]string{"node": "n1"}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("generic rotation while paired = %d, want 409", resp.StatusCode)
	}
	if !h.publisherTokenWorks(pantryOS.gotToken) {
		t.Fatal("the paired PantryOS lost its publisher token")
	}
}
