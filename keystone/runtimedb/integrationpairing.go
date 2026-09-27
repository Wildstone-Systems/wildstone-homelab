package runtimedb

// Paired integration credentials (ADR-W042 A6, #384): what Keystone keeps
// after pairing with an app it does not run. See migration 0089.
//
// Sealed with the household secret key, but outside the household secret
// store's person-and-grant model: this is the control plane's credential
// for one integration, used for every household call to it and never
// revealed through the API. Pairing returns nothing secret; status reads
// return the endpoint and when, never the key.

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// IntegrationPairing is a pairing, without its credential.
type IntegrationPairing struct {
	Capability string `json:"capability"`
	Endpoint   string `json:"endpoint"`
	PairedAt   string `json:"pairedAt"`
	PairedBy   string `json:"pairedBy"`
}

// ErrNotPaired is returned when a capability has no pairing.
var ErrNotPaired = errors.New("integration is not paired")

func pairingAAD(capability, endpoint string) []byte {
	return []byte(fmt.Sprintf("integration-pairing/v1/%s/%s", capability, endpoint))
}

// SavePairing seals key and stores it for capability at endpoint,
// replacing any earlier pairing.
func (v *SecretVault) SavePairing(capability, endpoint, key, by string) error {
	capability, endpoint = strings.TrimSpace(capability), strings.TrimSpace(endpoint)
	if capability == "" || endpoint == "" {
		return errors.New("a pairing needs a capability and an endpoint")
	}
	if key == "" || len(key) > 8192 {
		return errors.New("a paired credential must contain 1 to 8192 bytes")
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := v.aead.Seal(nonce, nonce, []byte(key), pairingAAD(capability, endpoint))
	_, err := v.db.sql.Exec(`INSERT INTO integration_pairings(capability,endpoint,key_ciphertext,paired_at,paired_by)
		VALUES(?,?,?,?,?)
		ON CONFLICT(capability) DO UPDATE SET endpoint=excluded.endpoint, key_ciphertext=excluded.key_ciphertext,
			paired_at=excluded.paired_at, paired_by=excluded.paired_by`,
		capability, endpoint, sealed, now(), by)
	return err
}

// PairingCredential returns the endpoint and the unsealed credential for
// capability. For Keystone's own use only: no API route returns it.
func (v *SecretVault) PairingCredential(capability string) (string, string, error) {
	var endpoint string
	var sealed []byte
	err := v.db.sql.QueryRow(`SELECT endpoint,key_ciphertext FROM integration_pairings WHERE capability=?`, capability).
		Scan(&endpoint, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotPaired
	}
	if err != nil {
		return "", "", err
	}
	key, err := v.openPairing(capability, endpoint, sealed)
	if err != nil {
		return "", "", err
	}
	return endpoint, string(key), nil
}

func (v *SecretVault) openPairing(capability, endpoint string, sealed []byte) ([]byte, error) {
	n := v.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("paired credential is corrupt")
	}
	raw, err := v.aead.Open(nil, sealed[:n], sealed[n:], pairingAAD(capability, endpoint))
	if err != nil {
		return nil, errors.New("paired credential cannot be opened with this key")
	}
	return raw, nil
}

// Pairing describes capability's pairing, without the credential.
func (db *DB) Pairing(capability string) (IntegrationPairing, error) {
	p := IntegrationPairing{Capability: capability}
	err := db.sql.QueryRow(`SELECT endpoint,paired_at,paired_by FROM integration_pairings WHERE capability=?`, capability).
		Scan(&p.Endpoint, &p.PairedAt, &p.PairedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotPaired
	}
	return p, err
}

// DeletePairing forgets capability's pairing. Deleting one that does not
// exist is not an error.
func (db *DB) DeletePairing(capability string) error {
	_, err := db.sql.Exec(`DELETE FROM integration_pairings WHERE capability=?`, capability)
	return err
}

// checkPairingsOpen proves every stored pairing opens with the vault's
// key, so a wrong key is refused at startup rather than at first use.
func (v *SecretVault) checkPairingsOpen() error {
	rows, err := v.db.sql.Query(`SELECT capability,endpoint,key_ciphertext FROM integration_pairings`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var capability, endpoint string
		var sealed []byte
		if err := rows.Scan(&capability, &endpoint, &sealed); err != nil {
			return err
		}
		raw, oerr := v.openPairing(capability, endpoint, sealed)
		clear(raw)
		if oerr != nil {
			return errors.New("household secret key or paired credential invalid; refusing to open store")
		}
	}
	return rows.Err()
}
