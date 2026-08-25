package main

// `wild node` (ADR-W014 A3): pairing. mint-join asks Keystone for a
// single-use join token and prints the ready-to-run install command the
// endpoint composes (token + CA fingerprint baked in).

import (
	"flag"
	"fmt"
	"net/http"
	"os"
)

func runNode(args []string) int {
	if len(args) < 1 || args[0] != "mint-join" {
		nodeUsage()
		return 2
	}
	fs := flag.NewFlagSet("wild node mint-join", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	ttl := fs.Int64("ttl", 0, "token TTL in seconds (default: server default, 15m)")
	fs.Parse(args[1:]) //nolint:errcheck // ExitOnError

	token := os.Getenv("WILD_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "wild node: set WILD_TOKEN to an admin API token")
		return 2
	}
	var body any
	if *ttl > 0 {
		body = map[string]int64{"ttlSeconds": *ttl}
	}
	out, err := apiCall(*apiBase, token, http.MethodPost, "/api/v1/join-tokens", body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild node:", err)
		return 1
	}
	fmt.Println(out)
	return 0
}

func nodeUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild node mint-join [-ttl seconds] [-api URL]
Authentication: set WILD_TOKEN to an admin API token.`)
}
