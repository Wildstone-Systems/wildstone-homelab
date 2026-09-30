package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
	"github.com/chickenrat17/wildstone-homelab/proto/recoveryseal"
	"github.com/chickenrat17/wildstone-homelab/proto/secreterase"
)

// TestEraseSecretProvesAbsence pins the erase contract (protocol minor
// 22): the value and its sidecar are gone, the answer says so, and the
// evidence verifies only for the request that asked.
func TestEraseSecretProvesAbsence(t *testing.T) {
	a := storeOnlyAgent(t)
	const name = "pc.mail-ada.v1"
	if _, err := a.sealSecret(name, "REPLACED-app-password"); err != nil {
		t.Fatal(err)
	}
	res := a.dispatchOp(t.Context(), "op1", &wsv1.Operation{Op: &wsv1.Operation_EraseSecret{
		EraseSecret: &wsv1.EraseSecret{Name: name, MaxVersion: 1, Nonce: "n-1"},
	}})
	got := res.GetEraseSecret()
	if got == nil {
		t.Fatalf("erase failed: %v", res.GetError())
	}
	if !got.GetExisted() || !got.GetAbsent() || got.GetErasedVersion() != 1 {
		t.Fatalf("erase result = %+v, want existed, absent, version 1", got)
	}
	if !secreterase.Verify(name, "n-1", 1, true, got.GetEvidence()) {
		t.Fatal("evidence does not verify for its own request")
	}
	if secreterase.Verify(name, "n-other", 1, true, got.GetEvidence()) {
		t.Fatal("evidence verified for a different request")
	}
	for _, p := range []string{name, name + ".meta.json"} {
		if _, err := os.Lstat(filepath.Join(a.secretsDir(), p)); !os.IsNotExist(err) {
			t.Fatalf("%s still exists after erase: %v", p, err)
		}
	}
	if _, err := a.openSecret(name); err == nil {
		t.Fatal("an erased secret still opens")
	}

	// Idempotent: the retry after a lost answer is a proven absence.
	again, oerr := a.eraseSecret(&wsv1.EraseSecret{Name: name, Nonce: "n-2"})
	if oerr != nil || again.EraseSecret.GetExisted() || !again.EraseSecret.GetAbsent() {
		t.Fatalf("second erase = %+v, %v; want not-existed and absent", again, oerr)
	}
	if !secreterase.Verify(name, "n-2", 0, true, again.EraseSecret.GetEvidence()) {
		t.Fatal("idempotent erase evidence does not verify")
	}
}

// TestEraseSecretIsVersionFenced: a stale erase never destroys a newer
// credential written under the same name.
func TestEraseSecretIsVersionFenced(t *testing.T) {
	a := storeOnlyAgent(t)
	for _, v := range []string{"REPLACED-old", "REPLACED-new"} {
		if _, err := a.sealSecret("tok", v); err != nil {
			t.Fatal(err)
		}
	}
	_, oerr := a.eraseSecret(&wsv1.EraseSecret{Name: "tok", MaxVersion: 1, Nonce: "n"})
	if oerr == nil || oerr.GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("fenced erase error = %v, want FORBIDDEN", oerr)
	}
	if got, err := a.openSecret("tok"); err != nil || got != "REPLACED-new" {
		t.Fatalf("newer secret was touched: %q, %v", got, err)
	}
	for _, bad := range []*wsv1.EraseSecret{{Name: "../x", Nonce: "n"}, {Name: "tok"}} {
		if _, oerr := a.eraseSecret(bad); oerr == nil || oerr.GetCode() != wsv1.ErrorCode_ERROR_CODE_INVALID {
			t.Fatalf("erase %+v = %v, want INVALID", bad, oerr)
		}
	}
}

// TestPersonalCredentialsStayOutOfRecoveryExport: a Person's mailbox
// credential is not household recovery material (ADR-W033 D1).
func TestPersonalCredentialsStayOutOfRecoveryExport(t *testing.T) {
	a := storeOnlyAgent(t)
	if _, err := a.sealSecret("household-key", "REPLACED-household"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.sealSecret(secreterase.PersonalPrefix+"mail-ada.v1", "REPLACED-personal"); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := recoveryseal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	out, oerr := a.exportSecretBundle(&wsv1.ExportSecretBundle{RecoveryPublicKey: pub})
	if oerr != nil {
		t.Fatal(oerr.GetMessage())
	}
	if out.ExportSecretBundle.GetSecretCount() != 1 {
		t.Fatalf("exported %d secrets, want only the household one", out.ExportSecretBundle.GetSecretCount())
	}
	plain, err := recoveryseal.Open(priv, out.ExportSecretBundle.GetSealedBundle())
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) == 0 || bytes.Contains(plain, []byte("REPLACED-personal")) || bytes.Contains(plain, []byte("mail-ada")) {
		t.Fatal("the personal credential reached the recovery bundle")
	}
}

func TestEraseSecretAuditsNameOnly(t *testing.T) {
	arm, fields := auditFields(&wsv1.Operation{Op: &wsv1.Operation_EraseSecret{
		EraseSecret: &wsv1.EraseSecret{Name: "pc.mail-ada.v1", MaxVersion: 2, Nonce: "nonce-value"},
	}})
	if arm != "EraseSecret" {
		t.Fatalf("arm = %s", arm)
	}
	for _, f := range fields {
		if f.Value.String() == "nonce-value" {
			t.Fatal("the nonce was audited")
		}
	}
}
