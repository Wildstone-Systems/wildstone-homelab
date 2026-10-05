// Package mailadapter is the configuration half of the wild-mail-adapter
// program (keystone/cmd/wild-mail-adapter; epic #326, ADR-W030 A5.2/A5.3):
// how a rendered placement's environment becomes a checked Config, and
// how a Config becomes the mailbox reader behind mail.Upstream. It lives
// in a package so the end-to-end proofs drive the adapter from the very
// environment a render Plan wrote, exactly as the program does.
package mailadapter

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail/imapread"
	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
)

// Environment names specific to the mail adapter. The generic ones are
// capability.AdapterEnv*.
const (
	EnvUsername       = "WILD_MAILBOX_USERNAME"
	EnvFilters        = "WILD_MAILBOX_FILTERS"
	EnvPassword       = capability.AdapterEnvSecretPrefix + "MAILBOX_APP_PASSWORD"
	EnvFingerprintKey = capability.AdapterEnvSecretPrefix + "MAILBOX_FINGERPRINT_KEY"
	EnvHealth         = "WILD_HEALTH_ADDR"
)

// MinConnectionSecret is the shortest fingerprint-key secret accepted.
const MinConnectionSecret = 32

// Config is everything the environment says, checked.
type Config struct {
	KeystoneAPI string
	Token       string
	Settings    mail.Settings
	Credentials mail.Credentials
	Filters     imapread.Filters
	HealthAddr  string
	// connectionSecret keys fingerprints and references. Never printed.
	connectionSecret []byte
	// RootCAs replaces the system trust pool; tests only.
	RootCAs *x509.CertPool
}

// LoadConfig reads and checks the environment. It fails closed: a
// rendering from a newer adapter contract than this program understands
// is refused rather than half-honoured.
func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		KeystoneAPI: getenv(capability.AdapterEnvKeystoneAPI),
		Token:       getenv(capability.AdapterEnvKeystoneToken),
		HealthAddr:  getenv(EnvHealth),
		Settings: mail.Settings{
			IMAPHost:       getenv(capability.AdapterEnvParamPrefix + "IMAP_HOST"),
			IMAPPort:       getenv(capability.AdapterEnvParamPrefix + "IMAP_PORT"),
			IMAPEncryption: getenv(capability.AdapterEnvParamPrefix + "IMAP_ENCRYPTION"),
			UsernameDomain: getenv(capability.AdapterEnvParamPrefix + "USERNAME_DOMAIN"),
			Connection:     getenv(capability.AdapterEnvConnection),
			Node:           getenv(capability.AdapterEnvNode),
			Package:        getenv(capability.AdapterEnvPackage),
			PackageVersion: getenv(capability.AdapterEnvPackageVersion),
		},
		Credentials:      mail.Credentials{Username: getenv(EnvUsername), Password: getenv(EnvPassword)},
		connectionSecret: []byte(getenv(EnvFingerprintKey)),
	}
	if c.KeystoneAPI == "" || c.Token == "" {
		return Config{}, fmt.Errorf("%s and %s are required", capability.AdapterEnvKeystoneAPI, capability.AdapterEnvKeystoneToken)
	}
	if c.Settings.Connection == "" {
		return Config{}, fmt.Errorf("%s is required", capability.AdapterEnvConnection)
	}
	contract, err := strconv.Atoi(getenv(capability.AdapterEnvContract))
	if err != nil || contract < 1 {
		return Config{}, fmt.Errorf("%s must name the adapter contract this container was rendered against", capability.AdapterEnvContract)
	}
	if contract > pkgspec.AdapterContract {
		return Config{}, fmt.Errorf("rendered against adapter contract %d; this program understands %d — upgrade the node's Wildstone programs",
			contract, pkgspec.AdapterContract)
	}
	switch c.Settings.IMAPEncryption {
	case "", "ssl", "tls":
	default:
		return Config{}, fmt.Errorf("imap encryption must be ssl or tls, got %q", c.Settings.IMAPEncryption)
	}
	if raw := getenv(EnvFilters); raw != "" {
		// The approved scope is decoded strictly: a scope this program
		// half-understands would read more than the Person approved.
		dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c.Filters); err != nil {
			return Config{}, fmt.Errorf("%s: %v", EnvFilters, err)
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("%s: trailing data", EnvFilters)
		}
		if err := c.Filters.Validate(); err != nil {
			return Config{}, fmt.Errorf("%s: %v", EnvFilters, err)
		}
	}
	return c, nil
}

// ConnectionKey derives the imapread key from the connection's secret,
// bound to the connection's name, so even a secret provisioned twice by
// mistake cannot give two connections comparable fingerprints or
// interchangeable references.
func ConnectionKey(secret []byte, connection string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("wild-mail-adapter/connection-key/v1\x00" + connection))
	return m.Sum(nil)
}

// ReaderFor chooses the mailbox reader: the read-only IMAP reader when
// the connection has everything it needs, otherwise a NotConfigured
// that says which piece is missing. It never connects; the first
// operation does.
func ReaderFor(c Config) mail.Reader {
	if !c.Credentials.Present() {
		return mail.NotConfigured{Reason: "no mailbox credential has been onboarded for this connection"}
	}
	if len(c.connectionSecret) < MinConnectionSecret {
		return mail.NotConfigured{Reason: "no connection fingerprint key has been provisioned for this connection"}
	}
	if c.Settings.IMAPHost == "" {
		return mail.NotConfigured{Reason: "the mail package has no IMAP host"}
	}
	port := c.Settings.IMAPPort
	if port == "" {
		port = "993"
		if c.Settings.IMAPEncryption == string(imapread.SecurityStartTLS) {
			port = "143"
		}
	}
	r, err := imapread.New(imapread.Config{
		Addr:           net.JoinHostPort(c.Settings.IMAPHost, port),
		Security:       imapread.Security(c.Settings.IMAPEncryption),
		RootCAs:        c.RootCAs,
		Username:       c.Credentials.Username,
		UsernameDomain: c.Settings.UsernameDomain,
		Password:       c.Credentials.Password,
		Key:            ConnectionKey(c.connectionSecret, c.Settings.Connection),
		Filters:        c.Filters,
	})
	if err != nil {
		// imapread's validation errors name fields, never values.
		return mail.NotConfigured{Reason: err.Error()}
	}
	return imapread.NewMailReader(r)
}
