package secreterase

import "testing"

func TestEvidenceVerifiesOnlyItsOwnAbsentErase(t *testing.T) {
	ev := Evidence("ic-mail-ada-v1", "n1", 3, true)
	if !Verify("ic-mail-ada-v1", "n1", 3, true, ev) {
		t.Fatal("evidence for the erase that was asked for must verify")
	}
	for name, ok := range map[string]bool{
		"other nonce":   Verify("ic-mail-ada-v1", "n2", 3, true, ev),
		"other name":    Verify("ic-mail-ada-v2", "n1", 3, true, ev),
		"other version": Verify("ic-mail-ada-v1", "n1", 4, true, ev),
		"present":       Verify("ic-mail-ada-v1", "n1", 3, false, Evidence("ic-mail-ada-v1", "n1", 3, false)),
		"empty nonce":   Verify("ic-mail-ada-v1", "", 3, true, Evidence("ic-mail-ada-v1", "", 3, true)),
		"truncated":     Verify("ic-mail-ada-v1", "n1", 3, true, ev[:16]),
	} {
		if ok {
			t.Errorf("%s: verified, want refused", name)
		}
	}
}
