package api

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestTrustedConversationExistenceIsHiddenFromExternalMessaging(t *testing.T) {
	current := runtimedb.Connection{Seq: 7, Disclosure: runtimedb.Disclosure{
		Reach: runtimedb.ReachExternal, Encryption: runtimedb.EncryptionTransport,
		Retention: runtimedb.RetentionProvider, Surface: runtimedb.TrustExternalMessaging,
	}}
	candidate := runtimedb.SessionCandidate{
		SessionID: "bs-42", Subject: "the private homepage conversation",
		ConnectionRef: 0, ConnectionName: butlerlive.ConsoleConnection,
	}
	if _, visible := candidateOnConnection(candidate, externalContinuationConnection(current)); visible {
		t.Fatal("an external Connection learned that a trusted-UI conversation exists")
	}
}

func TestUnresolvedCandidateDisclosureFailsClosed(t *testing.T) {
	current := runtimedb.Connection{Seq: 7, Disclosure: runtimedb.Disclosure{
		Reach: runtimedb.ReachExternal, Encryption: runtimedb.EncryptionTransport,
		Retention: runtimedb.RetentionProvider, Surface: runtimedb.TrustExternalMessaging,
	}}
	origin := runtimedb.Connection{Seq: 8} // no readable classification
	candidate := runtimedb.SessionCandidate{SessionID: "bs-42", ConnectionRef: 8, Connection: &origin}
	if _, visible := candidateOnConnection(candidate, externalContinuationConnection(current)); visible {
		t.Fatal("an unreadable classification disclosed a session candidate")
	}
}

func TestCandidateFieldsAreFilteredIndependently(t *testing.T) {
	current := runtimedb.Connection{Seq: 7, Disclosure: runtimedb.Disclosure{
		Reach: runtimedb.ReachExternal, Encryption: runtimedb.EncryptionTransport,
		Retention: runtimedb.RetentionProvider, Surface: runtimedb.TrustExternalMessaging,
	}}
	origin := runtimedb.Connection{Seq: 8, Disclosure: runtimedb.Disclosure{
		Reach: runtimedb.ReachExternal, Encryption: runtimedb.EncryptionEndToEnd,
		Retention: runtimedb.RetentionProvider, Surface: runtimedb.TrustExternalMessaging,
	}}
	shown, visible := candidateOnConnection(runtimedb.SessionCandidate{Connection: &origin}, externalContinuationConnection(current))
	if !visible || shown.ShowSubject || !shown.ShowTime {
		t.Fatalf("disclosure = visible %v subject %v time %v", visible, shown.ShowSubject, shown.ShowTime)
	}
}
