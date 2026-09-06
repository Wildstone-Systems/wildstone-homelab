package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// The capability names its surface, and the request must agree
// (ADR-W026 A3). Without this check, `-surface admin` on the family
// package would mint an admin credential into the family package's
// secret, and the household's bot would come up talking to the
// operator's Butler — working perfectly, for as long as nobody looked.
func TestChatAdapterRefusesASurfaceThatDisagreesWithTheCapability(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ capability, surface, want string }{
		{"chat-telegram-family", "admin", "is the family surface"},
		{"chat-matrix-admin", "family", "is the admin surface"},
		{"chat-telegram", "family", "does not name a surface"},
	} {
		var body map[string]any
		resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/chat/adapters/"+tc.capability,
			map[string]any{"node": "n1", "surface": tc.surface}, &body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s -surface %s: status = %d, want 400", tc.capability, tc.surface, resp.StatusCode)
		}
		if msg, _ := body["error"].(string); !strings.Contains(msg, tc.want) {
			t.Fatalf("%s -surface %s: error = %q, want it to mention %q",
				tc.capability, tc.surface, msg, tc.want)
		}
	}
}

// The surface decides the role, and it is never the butler role every
// external agent holds.
func TestRoleForSurface(t *testing.T) {
	family, err := roleForSurface(runtimedb.ButlerSurfaceFamily)
	if err != nil || family != runtimedb.RoleConcierge {
		t.Fatalf("family role = %q (err %v), want concierge", family, err)
	}
	admin, err := roleForSurface(runtimedb.ButlerSurfaceAdmin)
	if err != nil || admin != runtimedb.RoleValet {
		t.Fatalf("admin role = %q (err %v), want valet", admin, err)
	}
	if admin == runtimedb.RoleButler {
		t.Fatalf("the admin adapter runs as the butler role — external agents would share its verbs")
	}
	if _, err := roleForSurface("household"); err == nil {
		t.Fatalf("an unknown surface produced a role")
	}
}
