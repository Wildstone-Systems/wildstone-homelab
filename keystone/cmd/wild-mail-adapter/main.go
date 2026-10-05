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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail/mailadapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
)

var version = "dev" // set via -ldflags at release

// The configuration lives in keystone/adapter/mail/mailadapter so the
// end-to-end proofs build the adapter from a rendered environment the
// same way; these names keep this file reading as it did.
type config = mailadapter.Config

func loadConfig(getenv func(string) string) (config, error) { return mailadapter.LoadConfig(getenv) }

func readerFor(c config) mail.Reader { return mailadapter.ReaderFor(c) }

func connectionKey(secret []byte, connection string) []byte {
	return mailadapter.ConnectionKey(secret, connection)
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
