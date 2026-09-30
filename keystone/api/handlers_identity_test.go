package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// identityFileAgent answers ReadManagedFile with a fixed notification.
type identityFileAgent struct{ content string }

func (a identityFileAgent) Apply(_ context.Context, _ string, _ *wsv1.Operation) (*wsv1.OpResult, error) {
	return &wsv1.OpResult{Result: &wsv1.OpResult_ReadManagedFile{
		ReadManagedFile: &wsv1.ReadManagedFileResult{Content: []byte(a.content)},
	}}, nil
}

// testResetToken is secret-shaped (so redaction would eat it) and built
// at runtime, per CONTRIBUTING's rule for test secrets.
func testResetToken() string { return strings.Repeat("aaaabbbbccccdddd", 3) }

func getIdentityCode(t *testing.T, h *harness, role runtimedb.Role) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/nodes/n1/identity/code", nil)
	req.Header.Set("Authorization", "Bearer "+h.tokens[role])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw bytes.Buffer
	raw.ReadFrom(resp.Body)
	return resp, raw.String()
}

// TestIdentityResetLinkSurvivesRedaction is #463: the reset link reaches
// the admin whole, uncached, while the free-text body and anything else
// secret-shaped in it stay redacted.
func TestIdentityResetLinkSurvivesRedaction(t *testing.T) {
	h := newHarness(t)
	link := "https://auth.example.test/reset-password/step2?token=" + testResetToken()
	unrelated := "deadbeef" + strings.Repeat("0123456789abcdef", 4)
	h.s.Agents = identityFileAgent{content: "Date: now\nRecipient: someone\n\n" +
		"Click to reset: " + link + "\n\napi_key: " + unrelated + "\n"}

	resp, body := getIdentityCode(t, h, runtimedb.RoleAdmin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var out identityNotification
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if out.ResetLink != link {
		t.Errorf("resetLink = %q, want the link intact", out.ResetLink)
	}
	if strings.Contains(out.Body, testResetToken()) {
		t.Error("the notification body carried the reset token unredacted")
	}
	if strings.Contains(body, unrelated) {
		t.Error("an unrelated secret-shaped value survived redaction")
	}
	if strings.Count(body, testResetToken()) != 1 {
		t.Errorf("the token appears %d times, want only in resetLink", strings.Count(body, testResetToken()))
	}
}

func TestIdentityCodeIsAdminOnly(t *testing.T) {
	h := newHarness(t)
	h.s.Agents = identityFileAgent{content: "https://auth.example.test/reset-password/step2?token=" + testResetToken()}
	resp, body := getIdentityCode(t, h, runtimedb.RoleOperator)
	if resp.StatusCode != http.StatusForbidden || strings.Contains(body, testResetToken()) {
		t.Fatalf("operator got %d: %s", resp.StatusCode, body)
	}
}

func TestResetLinkFrom(t *testing.T) {
	tok := testResetToken()
	good := "https://auth.example.test/reset-password/step2?token=" + tok
	for _, tc := range []struct {
		name, body, want string
	}{
		{"valid", "reset: " + good + "\n", good},
		{"repeated same link", good + "\n" + good, good},
		{"none", "Your one-time code:\nABCD1234\n", ""},
		{"http", "http://auth.example.test/reset-password/step2?token=" + tok, ""},
		{"userinfo", "https://u:p@auth.example.test/reset-password/step2?token=" + tok, ""},
		{"other page", "https://auth.example.test/revoke/reset-password?token=" + tok, ""},
		{"empty token", "https://auth.example.test/reset-password/step2?token=", ""},
		{"two tokens", good + "&token=other", ""},
		{"extra param", good + "&next=elsewhere", ""},
		{"fragment", good + "#x", ""},
		{"two different links", good + "\nhttps://other.example.test/reset-password/step2?token=" + tok, ""},
	} {
		if got := resetLinkFrom(tc.body); got != tc.want {
			t.Errorf("%s: resetLinkFrom = %q, want %q", tc.name, got, tc.want)
		}
	}
}
