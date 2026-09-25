// Command agent-bridge runs beside an external agent runtime (Hermes
// today; ADR-W039 §8 Phase 5/6, docs/design/hermes-capability-package-
// and-execution-profiles.md) and does two things (#349):
//
//  1. A delivery loop: long-polls Wildstone's Agent API for this Agent's
//     own deliveries (agentapi.DeliveryClient, ADR-W040 §4's pull
//     fallback), and for each turn or subscription delivery, fetches the
//     Manifest on its handle and starts a Hermes run for it, reports the
//     run's lifecycle back over the Work family (Start/Complete/Fail),
//     and acknowledges the delivery only once that report has landed.
//  2. An MCP server Hermes calls back into Wildstone through (ADR-W040
//     §6's translation layer, implemented here rather than inside
//     Keystone: Keystone holds no MCP dependency, and this process
//     already holds the one credential every such call must be checked
//     against). It never exposes that credential to Hermes.
//
// It is deliberately dumb, in the same sense cmd/chat-adapter is: it
// holds two narrow credentials and shows neither to the other side —
// WILDSTONE_RUNTIME_TOKEN is Wildstone's bearer, checked on every call to
// Keystone and never logged; MCP_SHARED_SECRET is Hermes's own secret,
// checked on every MCP call and never forwarded to Wildstone. Nothing
// about the household is visible to it beyond what a Manifest and an
// Invoke result already carry, both already redacted and enveloped by
// Wildstone before this process ever sees them.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi/httpclient"
)

var version = "dev" // set via -ldflags at release, exactly as chat-adapter's is.

func main() {
	// Flags carry defaults from the environment: this runs as a
	// container, where a package declares configuration as env, and the
	// same binary run by hand still wants flags. Credentials are always
	// env-only (see below), never a flag — a flag sits in every process
	// listing on the host (ADR-W017 A4, the chat adapter's own rule).
	fs := flag.NewFlagSet("agent-bridge", flag.ExitOnError)
	wildstoneURL := fs.String("wildstone-url", envOr("WILDSTONE_URL", "http://127.0.0.1:9441"), "Keystone's base URL, as this runtime can reach it")
	hermesURL := fs.String("hermes-url", envOr("HERMES_URL", "http://127.0.0.1:8642"), "Hermes's API server base URL, over the container network")
	mcpListen := fs.String("mcp-listen", envOr("MCP_LISTEN", ":8765"), "address the Wildstone MCP endpoint listens on (internal network only)")
	pollWait := fs.Duration("poll-wait", 25*time.Second, "how long to long-poll Wildstone's deliveries endpoint")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Parse(os.Args[1:]) //nolint:errcheck // ExitOnError

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *showVersion {
		fmt.Println("agent-bridge", version)
		return
	}

	wildstoneToken := os.Getenv("WILDSTONE_RUNTIME_TOKEN")
	if wildstoneToken == "" {
		log.Error("WILDSTONE_RUNTIME_TOKEN is not set; the bridge needs this Agent's runtime bearer")
		os.Exit(2)
	}
	hermesKey := os.Getenv("HERMES_API_KEY")
	if hermesKey == "" {
		log.Error("HERMES_API_KEY is not set; the bridge cannot start a Hermes run")
		os.Exit(2)
	}
	mcpSecret := os.Getenv("MCP_SHARED_SECRET")
	if mcpSecret == "" {
		log.Error("MCP_SHARED_SECRET is not set; the MCP endpoint would trust anyone who can reach it")
		os.Exit(2)
	}

	ks := &httpclient.Client{BaseURL: *wildstoneURL, Bearer: wildstoneToken}
	hc := &hermesClient{Base: *hermesURL, APIKey: hermesKey}
	runs := newRunRegistry()

	br := &bridge{
		keystone: ks,
		hermes:   hc,
		runs:     runs,
		log:      log,
		pollWait: *pollWait,
		version:  version,
	}

	mcp := &mcpServer{keystone: ks, runs: runs, secret: mcpSecret, log: log}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp)
	srv := &http.Server{Addr: *mcpListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		log.Info("mcp endpoint listening", "addr", *mcpListen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("mcp server stopped", "err", err)
		}
	}()

	heartbeatDone := make(chan struct{})
	go func() { br.heartbeatLoop(ctx); close(heartbeatDone) }()
	deliveryDone := make(chan struct{})
	go func() { br.deliveryLoop(ctx); close(deliveryDone) }()

	log.Info("agent bridge starting", "wildstone", *wildstoneURL, "hermes", *hermesURL, "mcp", *mcpListen)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	<-serveDone
	<-heartbeatDone
	<-deliveryDone
	log.Info("agent bridge stopped")
}

// envOr reads an environment variable, falling back to a default.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// sleepBackoff waits, doubling up to a ceiling — chat-adapter's own
// helper, copied rather than shared because two ten-line functions are
// cheaper than a dependency between two independently-released binaries.
func sleepBackoff(ctx context.Context, d time.Duration) time.Duration {
	const maxBackoff = 30 * time.Second
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
	if d *= 2; d > maxBackoff {
		d = maxBackoff
	}
	return d
}
