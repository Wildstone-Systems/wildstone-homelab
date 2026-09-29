package main

// `wild status|plan|audit|logs|backup` (ADR-W015 A2): thin mappings over
// the public API, sharing the WILD_TOKEN/-api client conventions.

import (
	"encoding/json"
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
		backupUsage()
		return 2
	}
	sub, rest := args[0], args[1:]
	if sub == "target" {
		return runBackupTarget(rest)
	}
	if sub == "plan" {
		return runBackupPlanAuthor(rest)
	}
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
	from := fs.String("from", "local", "restore only: local, or offsite to restore from the plan's off-site copy")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	token, ok := requireToken("backup")
	if !ok {
		return 2
	}
	switch sub {
	case "list", "plans":
		return simpleCall("backup", *apiBase, token, http.MethodGet, "/api/v1/backups", nil)
	case "run":
		return simpleCall("backup", *apiBase, token, http.MethodPost, "/api/v1/backups/"+url.PathEscape(plan)+"/run", nil)
	case "restore":
		path := "/api/v1/backups/" + url.PathEscape(plan) + "/restore"
		if *from != "local" {
			path += "?from=" + url.QueryEscape(*from)
		}
		return simpleCall("backup", *apiBase, token, http.MethodPost, path, nil)
	default:
		backupUsage()
		return 2
	}
}

func runBackupTarget(args []string) int {
	if len(args) < 1 {
		backupUsage()
		return 2
	}
	sub, rest := args[0], args[1:]
	var name string
	if sub == "test" || sub == "propose" {
		name, rest = splitLeadingArg(rest)
		if name == "" {
			fmt.Fprintf(os.Stderr, "wild backup target %s: target name required\n", sub)
			return 2
		}
	}
	fs := flag.NewFlagSet("wild backup target "+sub, flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	node := fs.String("node", "", "node holding the target credentials")
	endpoint := fs.String("endpoint", "", "S3-compatible API endpoint")
	bucket := fs.String("bucket", "", "existing bucket name")
	prefix := fs.String("prefix", "", "optional bucket prefix")
	region := fs.String("region", "", "provider region")
	access := fs.String("access-secret", "", "access key reference, secret/<name>")
	secret := fs.String("secret-key-secret", "", "secret key reference, secret/<name>")
	pathStyle := fs.Bool("path-style", false, "use path-style bucket addressing")
	insecure := fs.Bool("insecure-http", false, "allow an http endpoint")
	appendOnly := fs.Bool("append-only", false, "credential cannot overwrite or delete history")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	token, ok := requireToken("backup target")
	if !ok {
		return 2
	}
	switch sub {
	case "list":
		return simpleCall("backup target", *apiBase, token, http.MethodGet, "/api/v1/backups/targets", nil)
	case "test":
		return simpleCall("backup target", *apiBase, token, http.MethodPost, "/api/v1/backups/targets/"+url.PathEscape(name)+"/test", nil)
	case "propose":
		body := map[string]any{"name": name, "node": *node, "spec": map[string]any{
			"type": "s3", "endpoint": *endpoint, "bucket": *bucket, "prefix": *prefix,
			"region": *region, "pathStyle": *pathStyle, "insecureHTTP": *insecure,
			"appendOnly": *appendOnly, "accessKeySecret": *access, "secretKeySecret": *secret,
		}}
		return simpleCall("backup target", *apiBase, token, http.MethodPost, "/api/v1/backups/targets", body)
	default:
		backupUsage()
		return 2
	}
}

// runBackupPlanAuthor submits a complete BackupPlan spec from JSON. Keeping
// the helper lossless matters: it can author paths/excludes, local NAS
// repositories and every off-site route without an ever-growing flag DSL.
func runBackupPlanAuthor(args []string) int {
	if len(args) < 1 || args[0] != "propose" {
		backupUsage()
		return 2
	}
	name, rest := splitLeadingArg(args[1:])
	if name == "" {
		fmt.Fprintln(os.Stderr, "wild backup plan propose: plan name required")
		return 2
	}
	fs := flag.NewFlagSet("wild backup plan propose", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	node := fs.String("node", "", "node that runs the backup")
	specPath := fs.String("spec", "", "JSON file containing the BackupPlan spec")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	if *specPath == "" {
		fmt.Fprintln(os.Stderr, "wild backup plan propose: -spec is required")
		return 2
	}
	data, err := os.ReadFile(*specPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild backup plan propose:", err)
		return 1
	}
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		fmt.Fprintln(os.Stderr, "wild backup plan propose: invalid JSON:", err)
		return 1
	}
	token, ok := requireToken("backup plan")
	if !ok {
		return 2
	}
	return simpleCall("backup plan", *apiBase, token, http.MethodPut, "/api/v1/backups/plans/"+url.PathEscape(name), map[string]any{"node": *node, "spec": spec})
}

func backupUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild backup plans|list [-api URL]
  wild backup run <plan> [-api URL]
  wild backup restore <plan> [-from local|offsite] [-api URL]
  wild backup target list [-api URL]
  wild backup target test <name> [-api URL]
  wild backup target propose <name> -node ID -endpoint URL -bucket NAME -access-secret secret/NAME -secret-key-secret secret/NAME [options]
  wild backup plan propose <name> -node ID -spec plan.json [-api URL]`)
}

func planUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild plan list [-api URL]
  wild plan show <id> [-api URL]
  wild plan approve <id> [-api URL]
  wild plan reject <id> [-api URL]`)
}
