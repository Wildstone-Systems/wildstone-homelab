package runtimedb

import "testing"

// 0089: a Connection is classified the moment it exists. Before it, one
// added after 0064 had no row and read as wholly unresolved.
func TestANewConnectionIsClassified(t *testing.T) {
	db, b := runtimeFixture(t)
	for _, tc := range []struct {
		transport string
		retention Retention
	}{
		{"telegram", RetentionProvider},
		{"matrix", RetentionUnknown},
	} {
		if _, err := db.AddButlerAddress(b.Name, tc.transport, "bot-"+tc.transport, "", "mika"); err != nil {
			t.Fatalf("AddButlerAddress(%s): %v", tc.transport, err)
		}
		c, err := db.ChatConnectionAt(tc.transport, "bot-"+tc.transport)
		if err != nil {
			t.Fatal(err)
		}
		want := Disclosure{Reach: ReachExternal, Encryption: EncryptionTransport, Retention: tc.retention, Surface: TrustExternalMessaging}
		if c.Disclosure != want {
			t.Errorf("%s disclosure = %+v, want %+v", tc.transport, c.Disclosure, want)
		}
	}
}
