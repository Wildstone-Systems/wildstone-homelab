// Command wardstoned is the Wildstone host agent: the only component that
// touches the host. It dials out to Keystone over mTLS gRPC and serves a
// closed, typed operation surface. There is no Exec(string) here and there
// never will be.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/chickenrat17/wildstone-homelab/wardstone/internal/agent"
)

var version = "dev" // set via -ldflags at release

func main() {
	nodeID := flag.String("node-id", "", "unique node identifier (required)")
	keystone := flag.String("keystone", "", "Keystone gateway address host:port (required)")
	dataDir := flag.String("data-dir", "/etc/wildstone/wardstone", "directory for node identity")
	joinToken := flag.String("join-token", "", "one-time join token (first run only)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	a, err := agent.New(agent.Config{
		NodeID:       *nodeID,
		KeystoneAddr: *keystone,
		DataDir:      *dataDir,
		JoinToken:    *joinToken,
		Version:      version,
		Logger:       log,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "wardstoned:", err)
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("agent exited", "error", err)
		os.Exit(1)
	}
}
