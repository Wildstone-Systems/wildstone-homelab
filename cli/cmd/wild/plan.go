package main

// `wild status|plan|audit|logs|backup` (ADR-W015 A2): thin mappings over
// the public API, sharing the WILD_TOKEN/-api client conventions.

import (
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
)

// requireToken fetches WILD_TOKEN or exits with guidance.
func requireToken(cmd string) (string, bool) {
	token := os.Getenv("WILD_TOKEN")
	if token == "" {
		fmt.Fprintf(os.Stderr, "wild %s: set WILD_TOKEN to an API token\n", cmd)
		return "", false
	}
	return token, true
}

// simpleCall runs one API call and prints the pretty JSON reply.
func simpleCall(cmd, apiBase, token, method, path string, body any) int {
	out, err := apiCall(apiBase, token, method, path, body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wild %s: %v\n", cmd, err)
		return 1
	}
	fmt.Println(out)
	return 0
}

func runStatus(args []string) int {
	fs := flag.NewFlagSet("wild status", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	token, ok := requireToken("status")
	if !ok {
		return 2
	}
	// Composed client-side (ADR-W015 A2): three reads, no new endpoint.
	for _, section := range []struct{ title, path string }{
		{"whoami", "/api/v1/whoami"},
		{"nodes", "/api/v1/nodes"},
		{"observed", "/api/v1/observed"},
		{"recent events", "/api/v1/events?limit=15"},
	} {
		out, err := apiCall(*apiBase, token, http.MethodGet, section.path, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wild status: %s: %v\n", section.title, err)
			return 1
		}
		fmt.Printf("--- %s\n%s\n", section.title, out)
	}
	return 0
}

func runPlan(args []string) int {
	if len(args) < 1 {
		planUsage()
		return 2
	}
	sub, rest := args[0], args[1:]
	var id string
	if sub == "show" || sub == "approve" || sub == "reject" {
		id, rest = splitLeadingArg(rest)
		if id == "" {
			fmt.Fprintf(os.Stderr, "wild plan %s: plan id required\n", sub)
			return 2
		}
	}
	fs := flag.NewFlagSet("wild plan "+sub, flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	token, ok := requireToken("plan")
	if !ok {
		return 2
	}
	switch sub {
	case "list":
		return simpleCall("plan", *apiBase, token, http.MethodGet, "/api/v1/plans", nil)
	case "show":
		return simpleCall("plan", *apiBase, token, http.MethodGet, "/api/v1/plans/"+url.PathEscape(id), nil)
	case "approve":
		return simpleCall("plan", *apiBase, token, http.MethodPost, "/api/v1/plans/"+url.PathEscape(id)+"/approve", nil)
	case "reject":
		return simpleCall("plan", *apiBase, token, http.MethodPost, "/api/v1/plans/"+url.PathEscape(id)+"/reject", nil)
	default:
		planUsage()
		return 2
	}
}

func runAudit(args []string) int {
	fs := flag.NewFlagSet("wild audit", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	token, ok := requireToken("audit")
	if !ok {
		return 2
	}
	return simpleCall("audit", *apiBase, token, http.MethodGet, "/api/v1/audit", nil)
}

func runLogs(args []string) int {
	name, rest := splitLeadingArg(args)
	if name == "" {
		fmt.Fprintln(os.Stderr, "usage: wild logs <container> -node <id> [-tail N] [-api URL]")
		return 2
	}
	fs := flag.NewFlagSet("wild logs", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	node := fs.String("node", "", "target node id (required)")
	tail := fs.Int("tail", 0, "lines from the end (0 = agent default)")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	if *node == "" {
		fmt.Fprintln(os.Stderr, "wild logs: -node is required")
		return 2
	}
	token, ok := requireToken("logs")
	if !ok {
		return 2
	}
	path := fmt.Sprintf("/api/v1/nodes/%s/containers/%s/logs", url.PathEscape(*node), url.PathEscape(name))
	if *tail > 0 {
		path += fmt.Sprintf("?tail=%d", *tail)
	}
	return simpleCall("logs", *apiBase, token, http.MethodGet, path, nil)
}

func runBackup(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: wild backup list|run <plan>|restore <plan> [-api URL]")
		return 2
	}
	sub, rest := args[0], args[1:]
	var plan string
	if sub == "run" || sub == "restore" {
		plan, rest = splitLeadingArg(rest)
		if plan == "" {
			fmt.Fprintf(os.Stderr, "wild backup %s: plan name required\n", sub)
			return 2
		}
	}
	fs := flag.NewFlagSet("wild backup "+sub, flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	token, ok := requireToken("backup")
	if !ok {
		return 2
	}
	switch sub {
	case "list":
		return simpleCall("backup", *apiBase, token, http.MethodGet, "/api/v1/backups", nil)
	case "run":
		return simpleCall("backup", *apiBase, token, http.MethodPost, "/api/v1/backups/"+url.PathEscape(plan)+"/run", nil)
	case "restore":
		return simpleCall("backup", *apiBase, token, http.MethodPost, "/api/v1/backups/"+url.PathEscape(plan)+"/restore", nil)
	default:
		fmt.Fprintln(os.Stderr, "usage: wild backup list|run <plan>|restore <plan> [-api URL]")
		return 2
	}
}

func planUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild plan list [-api URL]
  wild plan show <id> [-api URL]
  wild plan approve <id> [-api URL]
  wild plan reject <id> [-api URL]`)
}
