package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func prepareVerifiedRestoreRelease(t *testing.T, h *harness) ed25519.PublicKey {
	t.Helper()
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err := h.s.DB.BackupTo(snapshot); err != nil {
		t.Fatal(err)
	}
	pub, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	snapshotAt := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	fencedAt := snapshotAt.Add(time.Hour)
	body, err := h.s.DB.BuildRecoveryJournalEvidence(snapshot, "keystone-original", "independent test custody",
		snapshotAt, fencedAt, fencedAt.Add(time.Minute), privateKey)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if err := h.s.DB.EnterRestoreQuarantine("API release fixture", snapshotAt); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.ExecForTest(`UPDATE restore_state SET snapshot_sha256=? WHERE id=1`, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.ReconcileRestoreEvidence(body, pub, "mika", fencedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	h.s.RecoveryJournalPublicKey = pub
	return pub
}

func TestRestoreReleaseRequiresExplicitCurrentOwnerAndIsAudited(t *testing.T) {
	h := freshnessHarness(t)
	prepareVerifiedRestoreRelease(t, h)
	fresh := sessionCookie(h.s.StartConsoleSession("mika", h.s.now()))

	if code, out := h.sessionRequest(http.MethodPost, "/api/v1/restore/release", `{}`, "mika", fresh); code != http.StatusBadRequest {
		t.Fatalf("implicit release = %d: %+v", code, out)
	}
	if state, _ := h.s.DB.GetRestoreState(); !state.Quarantined {
		t.Fatal("implicit request released quarantine")
	}

	code, out := h.sessionRequest(http.MethodPost, "/api/v1/restore/release", `{"release":true}`, "mika", fresh)
	if code != http.StatusOK {
		t.Fatalf("Owner release = %d: %+v", code, out)
	}
	release, ok := out["release"].(map[string]any)
	if !ok || release["releasedBy"] != "mika" || release["alreadyReleased"] != false {
		t.Fatalf("release response = %+v", out)
	}
	state, err := h.s.DB.GetRestoreState()
	if err != nil || state.Quarantined || state.ReleasedBy != "mika" || state.ReleasedAt.IsZero() {
		t.Fatalf("release state = %+v, %v", state, err)
	}
	if err := h.s.DB.CheckHouseholdExecution(); !errors.Is(err, runtimedb.ErrCoordinatorFenced) {
		t.Fatalf("release granted coordinator execution = %v", err)
	}
	audit, err := h.s.DB.ListAudit(20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range audit {
		if row.Path == "/api/v1/restore/release" && row.Status == http.StatusOK && row.ActingFor == "mika" {
			found = true
		}
	}
	if !found {
		t.Fatalf("successful release request missing from audit: %+v", audit)
	}
	code, out = h.sessionRequest(http.MethodPost, "/api/v1/restore/release", `{"release":true}`, "mika", fresh)
	if code != http.StatusOK || out["release"].(map[string]any)["alreadyReleased"] != true {
		t.Fatalf("repeated release = %d: %+v", code, out)
	}
}

func TestRestoreReleaseRefusesBearerOperatorAgentAndRevokedOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		role runtimedb.Role
	}{
		{"administrator bearer", runtimedb.RoleAdmin},
		{"operator bearer", runtimedb.RoleOperator},
		{"Agent bearer", runtimedb.RoleButler},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := freshnessHarness(t)
			prepareVerifiedRestoreRelease(t, h)
			if res := h.do(tc.role, http.MethodPost, "/api/v1/restore/release", map[string]bool{"release": true}, nil); res.StatusCode != http.StatusForbidden {
				t.Fatalf("release = %d, want 403", res.StatusCode)
			}
			if state, _ := h.s.DB.GetRestoreState(); !state.Quarantined || !state.ReleasedAt.IsZero() {
				t.Fatalf("refused caller changed restore state: %+v", state)
			}
		})
	}

	t.Run("revoked Owner", func(t *testing.T) {
		h := freshnessHarness(t)
		prepareVerifiedRestoreRelease(t, h)
		h.owner("olive")
		session := sessionCookie(h.s.StartConsoleSession("mika", h.s.now()))
		if err := h.s.DB.DisableButlerMember("mika", "olive"); err != nil {
			t.Fatal(err)
		}
		code, _ := h.sessionRequest(http.MethodPost, "/api/v1/restore/release", `{"release":true}`, "mika", session)
		if code != http.StatusForbidden && code != http.StatusUnauthorized {
			t.Fatalf("revoked Owner release = %d", code)
		}
		if state, _ := h.s.DB.GetRestoreState(); !state.Quarantined || !state.ReleasedAt.IsZero() {
			t.Fatalf("revoked Owner changed restore state: %+v", state)
		}
	})
}
