package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
)

// Connection disclosure (design §8.4, migration 0064).
//
// 0063 answered who a Connection ADMITS. This answers what may be said
// over it. The two are separate objects because they answer to
// different things: admission is about the speaker, disclosure is about
// everybody who will read the reply, and §8.3's whole reason for
// refusing group destinations in MVP is that knowing the first tells
// you nothing about the second.

// Disclosure is one Connection's privacy classification.
//
// Every field is a closed set with a named unresolved member, for the
// reason admission is: a characteristic this build cannot read must not
// be indistinguishable from the permissive value. Unresolved is what an
// absent row and an unrecognised string both produce, and every policy
// question answered from it takes the strict branch.
type Disclosure struct {
	Reach      Reach      `json:"reach"`
	Encryption Encryption `json:"encryption"`
	Retention  Retention  `json:"retention"`
	Surface    TrustLevel `json:"surface"`
	// AcceptsFallback is whether a message meant for ANOTHER
	// Connection may be delivered here (§8.3). False by default and
	// false when unresolved.
	AcceptsFallback bool `json:"acceptsFallback"`
}

// Reach is whether the bytes leave the house.
type Reach string

// Encryption is what protects them in transit.
type Encryption string

// Retention is who keeps a copy.
type Retention string

// TrustLevel is what the person is looking at.
type TrustLevel string

const (
	ReachUnresolved Reach = ""
	ReachLocal      Reach = "local"
	ReachExternal   Reach = "external"

	EncryptionUnresolved Encryption = ""
	EncryptionNone       Encryption = "none"
	EncryptionTransport  Encryption = "transport"
	EncryptionEndToEnd   Encryption = "end_to_end"

	RetentionUnresolved Retention = ""
	RetentionNone       Retention = "none"
	RetentionProvider   Retention = "provider"
	// RetentionUnknown is an operator who has NOT said, which is not
	// the same as one who said nobody keeps a copy. It reads the same
	// as Unresolved for every policy decision and is kept distinct
	// because "nobody has classified this Connection" and "somebody
	// classified it and does not know" are different things to fix.
	RetentionUnknown Retention = "unknown"

	TrustUnresolved        TrustLevel = ""
	TrustedUI              TrustLevel = "trusted_ui"
	TrustExternalMessaging TrustLevel = "external_messaging"
)

// LeavesTheHouse reports whether content sent here reaches a service
// this household does not run.
//
// UNRESOLVED ANSWERS YES. A Connection nobody has classified is treated
// as though it leaves, because the question this is asked in service of
// is always "may this content go here", and the safe answer for an
// unclassified destination is no.
func (d Disclosure) LeavesTheHouse() bool { return d.Reach != ReachLocal }

// KeepsACopy reports whether somebody outside this household may retain
// what is sent. Unresolved and unknown both answer yes.
func (d Disclosure) KeepsACopy() bool { return d.Retention != RetentionNone }

// scanDisclosure reads the four columns, mapping anything outside each
// vocabulary to that vocabulary's unresolved member.
func scanDisclosure(reach, encryption, retention, surface string, fallback int64) Disclosure {
	d := Disclosure{AcceptsFallback: fallback == 1}
	switch Reach(reach) {
	case ReachLocal:
		d.Reach = ReachLocal
	case ReachExternal:
		d.Reach = ReachExternal
	}
	switch Encryption(encryption) {
	case EncryptionNone:
		d.Encryption = EncryptionNone
	case EncryptionTransport:
		d.Encryption = EncryptionTransport
	case EncryptionEndToEnd:
		d.Encryption = EncryptionEndToEnd
	}
	switch Retention(retention) {
	case RetentionNone:
		d.Retention = RetentionNone
	case RetentionProvider:
		d.Retention = RetentionProvider
	case RetentionUnknown:
		d.Retention = RetentionUnknown
	}
	switch TrustLevel(surface) {
	case TrustedUI:
		d.Surface = TrustedUI
	case TrustExternalMessaging:
		d.Surface = TrustExternalMessaging
	}
	// An unresolved row cannot accept a fallback. Reading it any other
	// way would make "nobody has classified this Connection" the one
	// state in which cross-Connection delivery is permitted, which is
	// the exact inversion §8.3 forbids.
	if d.Reach == ReachUnresolved || d.Surface == TrustUnresolved {
		d.AcceptsFallback = false
	}
	return d
}

// ErrNotARoom is returned when a destination was expected to be a room
// and is not. It exists so a caller can tell "this is a direct chat"
// from a failure to find out.
var ErrNotARoom = errors.New("runtimedb: that conversation is not a known room")

// NoteChatRoom records that a conversation on a Connection is a room
// rather than a direct chat (migration 0064).
//
// Called from the INBOUND path, which is the only place that is told.
// The adapter reports `group: true` on the message; every later
// decision about that conversation -- above all whether a household
// rule may deliver into it -- has only a target string to go on.
//
// It is idempotent and it never forgets. A room that goes quiet is
// still a room, and expiring the row would silently re-permit delivery
// into it.
func (db *DB) NoteChatRoom(connection int64, conversation string) error {
	if connection == 0 || conversation == "" {
		return fmt.Errorf("runtimedb: noting a room needs a connection and a conversation")
	}
	_, err := db.sql.Exec(
		`INSERT INTO chat_rooms (connection, conversation, first_seen_at, last_seen_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (connection, conversation) DO UPDATE SET last_seen_at = excluded.last_seen_at`,
		connection, conversation, now(), now())
	if err != nil {
		return fmt.Errorf("runtimedb: noting room %q: %w", conversation, err)
	}
	return nil
}

// IsChatRoom reports whether a destination is a known room.
//
// A DATABASE ERROR ANSWERS YES. The caller's question is "may I send
// household content here", and a lookup that failed has not
// established that this is a direct chat. Returning false on error
// would make a broken database the most permissive state there is.
func (db *DB) IsChatRoom(connection int64, conversation string) bool {
	if conversation == "" {
		return false
	}
	var seen int
	err := db.sql.QueryRow(
		`SELECT 1 FROM chat_rooms WHERE connection = ? AND conversation = ?`,
		connection, conversation).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		return true
	}
	return true
}
