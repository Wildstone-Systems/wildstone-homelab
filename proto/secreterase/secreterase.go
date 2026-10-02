// Package secreterase is the evidence format of the Wardstone EraseSecret
// operation (protocol minor 22, ADR-W030 A5.1, epic #326 S02).
//
// The agent computes it over what it did; Keystone recomputes it over
// what it asked and what the agent says happened. It depends only on
// the standard library so both modules share the exact bytes without a
// dependency in either direction, the same arrangement as recoveryseal.
package secreterase

import (
	"crypto/sha256"
	"crypto/subtle"
	"strconv"
)

// PersonalPrefix begins the node secret name of every Personal
// integration credential (a Person's mailbox app password). Keystone
// names them "pc.<connection>.v<version>"; Wardstone keeps them out of
// the recovery bundle, because a Personal credential belongs to one
// Person and is re-entered by them, never restored by the household
// (ADR-W033 D1, epic #326 S02).
const PersonalPrefix = "pc."

// domain separates this digest from every other sha256 on the wire.
// Part of the contract: changing it is a new evidence scheme.
const domain = "wardstone-erase-v1"

// Evidence is sha256(domain || name || 0 || nonce || 0 ||
// decimal(erasedVersion) || 0 || "absent"|"present").
func Evidence(name, nonce string, erasedVersion uint32, absent bool) []byte {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(nonce))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatUint(uint64(erasedVersion), 10)))
	h.Write([]byte{0})
	if absent {
		h.Write([]byte("absent"))
	} else {
		h.Write([]byte("present"))
	}
	return h.Sum(nil)
}

// Verify reports whether evidence proves that name is absent from the
// store, for the erase that carried nonce. A result that says "present",
// names another request, or has been altered does not verify.
func Verify(name, nonce string, erasedVersion uint32, absent bool, evidence []byte) bool {
	if !absent || nonce == "" {
		return false
	}
	want := Evidence(name, nonce, erasedVersion, true)
	return subtle.ConstantTimeCompare(want, evidence) == 1
}
