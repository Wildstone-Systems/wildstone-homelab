package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
)

func runRestore(args []string) int {
	if len(args) == 0 {
		restoreUsage()
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("wild restore "+sub, flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	release := fs.Bool("release", false, "release quarantine after replay")
	fs.Parse(args[1:]) //nolint:errcheck // ExitOnError
	token, ok := requireToken("restore")
	if !ok {
		return 2
	}
	switch sub {
	case "status":
		return simpleCall("restore", *apiBase, token, http.MethodGet, "/api/v1/restore", nil)
	case "reconcile":
		return simpleCall("restore", *apiBase, token, http.MethodPost, "/api/v1/restore/reconcile",
			map[string]bool{"journalCurrent": true, "release": *release})
	default:
		restoreUsage()
		return 2
	}
}

func restoreUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild restore status [-api URL]
  wild restore reconcile [--release] [-api URL]`)
}
