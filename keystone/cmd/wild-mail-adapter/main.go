// Command wild-mail-adapter is the read-only mailbox adapter bundled with
// the mail-roundcube package (epic #326; ADR-W030 A5/A5.1).
//
// It is dormant by construction. Installing the package puts nothing of
// it anywhere but the program file the installer ships to every node
// (/srv/wildstone/bin). It runs only when a Person's approved connection
// placement renders its container (capability.Engine.ExpandAdapter), one
// per prepared node, and it is configured entirely by that rendering's
// environment:
//
//	KEYSTONE_API, KEYSTONE_TOKEN   where to claim work, as whom (an
//	                               integration-adapter principal bound to
//	                               this connection and node)
//	WILD_CONNECTION, WILD_NODE     which connection, which placement
//	WILD_PACKAGE[_VERSION]         the installed package it came from
//	WILD_ADAPTER_CONTRACT          the contract it was rendered against
//	WILD_PARAM_IMAP_*              the provider endpoint, inherited
//	WILD_PARAM_USERNAME_DOMAIN
//	WILD_MAILBOX_USERNAME          the Person's login name (not secret)
//	WILD_MAILBOX_FILTERS           the approved source scope, JSON
//	                               {folders, senders, subjectContains};
//	                               absent means INBOX, any sender
//	WILD_SECRET_MAILBOX_APP_PASSWORD  the node-materialized app password
//	WILD_SECRET_MAILBOX_FINGERPRINT_KEY  the connection's own secret
//	                               (>= 32 bytes) that keys message
//	                               fingerprints and references
//
// It claims work only for the connection its token is bound to, and only
// while its node holds that connection's lease; a result from a lease
// that has moved on is refused by Keystone and dropped here. Its answers
// are the four read queries of the mail domain and nothing else.
//
// Until a credential and a connection key are present, every answer is
// an explicit not_configured unavailability, and no mailbox is opened.
// With both, readerFor mounts the read-only IMAP reader
// (keystone/adapter/mail/imapread) behind mail.Upstream.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail/imapread"
	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
)

var version = "dev" // set via -ldflags at release

// Environment names specific to the mail adapter. The generic ones are
// capability.AdapterEnv*.
const (
	envUsername       = "WILD_MAILBOX_USERNAME"
	envFilters        = "WILD_MAILBOX_FILTERS"
	envPassword       = capability.AdapterEnvSecretPrefix + "MAILBOX_APP_PASSWORD"
	envFingerprintKey = capability.AdapterEnvSecretPrefix + "MAILBOX_FINGERPRINT_KEY"
	envHealth         = "WILD_HEALTH_ADDR"
)

// minConnectionSecret is the shortest fingerprint-key secret accepted.
const minConnectionSecret = 32

// config is everything the environment says, checked.
type config struct {
	KeystoneAPI string
	Token       string
	Settings    mail.Settings
	Credentials mail.Credentials
	Filters     imapread.Filters
	HealthAddr  string
	// connectionSecret keys fingerprints and references. Never printed.
	connectionSecret []byte
	// rootCAs replaces the system trust pool; tests only.
	rootCAs *x509.CertPool
}

// loadConfig reads and checks the environment. It fails closed: a
// rendering from a newer adapter contract than this program understands
// is refused rather than half-honoured.
func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		KeystoneAPI: getenv(capability.AdapterEnvKeystoneAPI),
		Token:       getenv(capability.AdapterEnvKeystoneToken),
		HealthAddr:  getenv(envHealth),
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
		Credentials:      mail.Credentials{Username: getenv(envUsername), Password: getenv(envPassword)},
		connectionSecret: []byte(getenv(envFingerprintKey)),
	}
	if c.KeystoneAPI == "" || c.Token == "" {
		return config{}, fmt.Errorf("%s and %s are required", capability.AdapterEnvKeystoneAPI, capability.AdapterEnvKeystoneToken)
	}
	if c.Settings.Connection == "" {
		return config{}, fmt.Errorf("%s is required", capability.AdapterEnvConnection)
	}
	contract, err := strconv.Atoi(getenv(capability.AdapterEnvContract))
	if err != nil || contract < 1 {
		return config{}, fmt.Errorf("%s must name the adapter contract this container was rendered against", capability.AdapterEnvContract)
	}
	if contract > pkgspec.AdapterContract {
		return config{}, fmt.Errorf("rendered against adapter contract %d; this program understands %d — upgrade the node's Wildstone programs",
			contract, pkgspec.AdapterContract)
	}
	switch c.Settings.IMAPEncryption {
	case "", "ssl", "tls":
	default:
		return config{}, fmt.Errorf("imap encryption must be ssl or tls, got %q", c.Settings.IMAPEncryption)
	}
	if raw := getenv(envFilters); raw != "" {
		// The approved scope is decoded strictly: a scope this program
		// half-understands would read more than the Person approved.
		dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c.Filters); err != nil {
			return config{}, fmt.Errorf("%s: %v", envFilters, err)
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return config{}, fmt.Errorf("%s: trailing data", envFilters)
		}
		if err := c.Filters.Validate(); err != nil {
			return config{}, fmt.Errorf("%s: %v", envFilters, err)
		}
	}
	return c, nil
}

// connectionKey derives the imapread key from the connection's secret,
// bound to the connection's name, so even a secret provisioned twice by
// mistake cannot give two connections comparable fingerprints or
// interchangeable references.
func connectionKey(secret []byte, connection string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("wild-mail-adapter/connection-key/v1\x00" + connection))
	return m.Sum(nil)
}

// readerFor chooses the mailbox reader: the read-only IMAP reader when
// the connection has everything it needs, otherwise a NotConfigured
// that says which piece is missing. It never connects; the first
// operation does.
func readerFor(c config) mail.Reader {
	if !c.Credentials.Present() {
		return mail.NotConfigured{Reason: "no mailbox credential has been onboarded for this connection"}
	}
	if len(c.connectionSecret) < minConnectionSecret {
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
		RootCAs:        c.rootCAs,
		Username:       c.Credentials.Username,
		UsernameDomain: c.Settings.UsernameDomain,
		Password:       c.Credentials.Password,
		Key:            connectionKey(c.connectionSecret, c.Settings.Connection),
		Filters:        c.Filters,
	})
	if err != nil {
		// imapread's validation errors name fields, never values.
		return mail.NotConfigured{Reason: err.Error()}
	}
	return imapread.NewMailReader(r)
}

// healthHandler answers readiness: ready once Keystone has answered a
// claim recently. It says whether a reader is configured, and never
// anything about the mailbox or the credential.
func healthHandler(r *adapter.Runner, c config, configured bool, window time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		last := r.LastContact()
		ready := !last.IsZero() && time.Since(last) <= window
		w.Header().Set("Content-Type", "application/json")
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ready": ready, "connection": c.Settings.Connection, "node": c.Settings.Node,
			"lastContact": last, "configured": configured, "version": version,
		})
	})
}

func run(ctx context.Context, c config, log *slog.Logger) error {
	reader := readerFor(c)
	_, notConfigured := reader.(mail.NotConfigured)
	up := &mail.Upstream{Settings: c.Settings, Reader: reader}
	runner := &adapter.Runner{
		Client:   &adapter.Client{BaseURL: c.KeystoneAPI, Token: c.Token, HTTP: &http.Client{Timeout: 45 * time.Second}},
		Upstream: up,
		Log:      func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) },
	}
	caps, _ := up.Capabilities(ctx)
	available := 0
	for _, cp := range caps {
		if cp.Available {
			available++
		}
	}
	log.Info("wild-mail-adapter starting", "version", version, "connection", c.Settings.Connection,
		"node", c.Settings.Node, "package", c.Settings.Package+"@"+c.Settings.PackageVersion,
		"operations", len(caps), "available", available)

	if c.HealthAddr != "" {
		ln, err := net.Listen("tcp", c.HealthAddr)
		if err != nil {
			return fmt.Errorf("health listener: %w", err)
		}
		srv := &http.Server{Handler: healthHandler(runner, c, !notConfigured, time.Minute), ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		defer func() {
			shut, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(shut)
		}()
	}
	err := runner.Run(ctx)
	if errors.Is(err, context.Canceled) {
		log.Info("wild-mail-adapter stopped")
		return nil
	}
	return err
}

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	check := flag.Bool("check", false, "check the environment and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("wild-mail-adapter", version)
		return
	}
	// Every line passes through the redactor: the adapter logs identifiers
	// and fixed categories only, and a secret-shaped value that reached a
	// line by mistake is replaced rather than written (epic #326 S08).
	log := slog.New(redact.NewLogHandler(slog.NewTextHandler(os.Stderr, nil)))
	c, err := loadConfig(os.Getenv)
	if err != nil {
		log.Error("wild-mail-adapter: refusing to start", "err", err)
		os.Exit(2)
	}
	if *check {
		fmt.Println("ok")
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, c, log); err != nil {
		log.Error("wild-mail-adapter", "err", err)
		os.Exit(1)
	}
}
