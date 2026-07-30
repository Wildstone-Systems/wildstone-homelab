// Command keystone is the Wildstone control plane: API gateway, Reconciler,
// Capability Engine, Policy Engine, Statefile, and Butler host process.
// Spike S1 scope: it runs the agent gateway (join + session) so Wardstone
// nodes can connect; reconciler, API, and Statefile land in S2/E2.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/gateway"
)

func main() {
	listen := flag.String("listen", ":9440", "gateway listen address")
	joinTokens := flag.String("join-tokens", "", "comma-separated one-time join tokens (dev/test; MintJoinToken is the production path)")
	hosts := flag.String("hosts", "localhost,127.0.0.1", "comma-separated SANs for the gateway certificate")
	dataDir := flag.String("data-dir", "/var/lib/wildstone/keystone", "directory persisting the CA, op-signing key, and join tokens across restarts")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	gw, err := gateway.New(gateway.Config{
		JoinTokens: splitNonEmpty(*joinTokens),
		Hosts:      splitNonEmpty(*hosts),
		DataDir:    *dataDir,
		Logger:     log,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "keystone:", err)
		os.Exit(1)
	}
	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "keystone:", err)
		os.Exit(1)
	}
	log.Info("gateway listening", "addr", lis.Addr().String())
	if err := gw.Serve(lis); err != nil {
		log.Error("gateway exited", "error", err)
		os.Exit(1)
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
