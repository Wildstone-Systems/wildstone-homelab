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
//	WILD_SECRET_MAILBOX_APP_PASSWORD  the node-materialized app password
//
// It claims work only for the connection its token is bound to, and only
// while its node holds that connection's lease; a result from a lease
// that has moved on is refused by Keystone and dropped here. Its answers
// are the four read queries of the mail domain and nothing else.
//
// Until an IMAP reader is plugged in (readerFor) and a credential is
// present, every answer is an explicit not_configured unavailability,
// and no mailbox is opened.
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
	"strconv"
	"syscall"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail"
	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
)

var version = "dev" // set via -ldflags at release

// Environment names specific to the mail adapter. The generic ones are
// capability.AdapterEnv*.
const (
	envUsername = "WILD_MAILBOX_USERNAME"
	envPassword = capability.AdapterEnvSecretPrefix + "MAILBOX_APP_PASSWORD"
	envHealth   = "WILD_HEALTH_ADDR"
)

// config is everything the environment says, checked.
type config struct {
	KeystoneAPI string
	Token       string
	Settings    mail.Settings
	Credentials mail.Credentials
	HealthAddr  string
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
		Credentials: mail.Credentials{Username: getenv(envUsername), Password: getenv(envPassword)},
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
	return c, nil
}

// readerFor chooses the mailbox reader. With no credential there is
// nothing to read with. The IMAP reader (keystone/adapter/mail/imapread,
// epic #326 S04) plugs in here; until it does, the adapter says it is
// not configured rather than pretending to have looked.
func readerFor(c config) mail.Reader {
	if !c.Credentials.Present() {
		return mail.NotConfigured{Reason: "no mailbox credential has been onboarded for this connection"}
	}
	return mail.NotConfigured{Reason: "this build has no IMAP reader"}
}

// healthHandler answers readiness: ready once Keystone has answered a
// claim recently. It says whether a reader is configured, and never
// anything about the mailbox or the credential.
func healthHandler(r *adapter.Runner, c config, window time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		last := r.LastContact()
		ready := !last.IsZero() && time.Since(last) <= window
		_, notConfigured := readerFor(c).(mail.NotConfigured)
		w.Header().Set("Content-Type", "application/json")
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ready": ready, "connection": c.Settings.Connection, "node": c.Settings.Node,
			"lastContact": last, "configured": !notConfigured, "version": version,
		})
	})
}

func run(ctx context.Context, c config, log *slog.Logger) error {
	up := &mail.Upstream{Settings: c.Settings, Reader: readerFor(c)}
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
		srv := &http.Server{Handler: healthHandler(runner, c, time.Minute), ReadHeaderTimeout: 5 * time.Second}
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
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
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
