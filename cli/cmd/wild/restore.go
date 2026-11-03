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
	fs.Parse(args[1:]) //nolint:errcheck // ExitOnError
	if sub == "reconcile" {
		fmt.Fprintln(os.Stderr, "wild restore reconcile: reconciliation requires signed evidence and a fresh active Owner Console session; API bearer tokens cannot authorize it")
		return 2
	}
	token, ok := requireToken("restore")
	if !ok {
		return 2
	}
	switch sub {
	case "status":
		return simpleCall("restore", *apiBase, token, http.MethodGet, "/api/v1/restore", nil)
	default:
		restoreUsage()
		return 2
	}
}

func restoreUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild restore status [-api URL]

Journal reconciliation uses POST /api/v1/restore/reconcile with signed evidence
from a fresh active Owner Console session. Quarantine release is separate.`)
}
